package sqlbus

import (
	"context"
	"database/sql"
	"fmt"
	"strconv"
	"strings"
	"time"

	"github.com/cgardev/gokeel/eventbus"

	"github.com/google/uuid"
)

const (
	messageTableName  = "event_message"
	listenerTableName = "event_message_listener"
	consumerTableName = "event_message_consumer"
	deliveryTableName = "event_message_delivery"
)

// timeLayout is the fixed-width UTC representation of every persisted
// timestamp. Unlike time.RFC3339Nano, which trims trailing fractional zeros,
// this layout pads the fraction to nine digits, so the lexicographic order of
// the TEXT columns equals their chronological order. That property is
// load-bearing: materialization frontiers, due-delivery ordering, and
// retention all compare these columns in SQL.
const timeLayout = "2006-01-02T15:04:05.000000000Z"

// dueDeliveryColumns lists the delivery columns in the order scanDueDelivery
// reads them, followed by the message columns it needs to restore the event.
const dueDeliveryColumns = "d.message_id, d.listener_id, d.instance, d.status, d.attempts, " +
	"d.claim_token, d.claim_date, d.next_attempt_date, d.completion_date, d.last_error, " +
	"m.event_type, m.serialized_event, m.publisher_node, m.publication_date"

// The statements are written with positional ? placeholders and rebound to
// the driver's placeholder style by the dialect, mirroring the outbox store.
const (
	insertMessageStatement = "INSERT INTO " + messageTableName + " " +
		"(id, event_type, serialized_event, publisher_node, publication_date) VALUES (?, ?, ?, ?, ?)"

	insertDeliveryStatement = "INSERT INTO " + deliveryTableName + " " +
		"(message_id, listener_id, instance, status, attempts) VALUES (?, ?, ?, ?, 0) " +
		"ON CONFLICT DO NOTHING"

	insertListenerModeStatement = "INSERT INTO " + listenerTableName + " " +
		"(listener_id, delivery_mode) VALUES (?, ?) ON CONFLICT DO NOTHING"

	selectListenerModeStatement = "SELECT delivery_mode FROM " + listenerTableName + " WHERE listener_id = ?"

	// An existing registration keeps its boundary and frontier, so a durable
	// group resumes where it left off, but its heartbeat is refreshed: a node
	// re-attaching under a stable identity must rejoin with fresh liveness,
	// not with the staleness it accumulated while it was down.
	insertConsumerStatement = "INSERT INTO " + consumerTableName + " " +
		"(listener_id, instance, event_type, delivery_mode, start_boundary, frontier, " +
		"registration_date, heartbeat_date) VALUES (?, ?, ?, ?, ?, ?, ?, ?) " +
		"ON CONFLICT (listener_id, instance, event_type) " +
		"DO UPDATE SET heartbeat_date = excluded.heartbeat_date"

	heartbeatStatement = "UPDATE " + consumerTableName + " SET heartbeat_date = ? " +
		"WHERE listener_id = ? AND instance = ? AND event_type = ?"

	deleteConsumersStatement = "DELETE FROM " + consumerTableName + " WHERE listener_id = ?"

	deleteListenerModeStatement = "DELETE FROM " + listenerTableName + " WHERE listener_id = ?"

	// The frontier is read from the consumer row inside the statement, so the
	// scan floor is always the durable one. A missing consumer row (for
	// example, one reaped by broadcast expiry) yields a NULL frontier, whose
	// comparison matches no rows: an unregistered consumer materializes
	// nothing until its heartbeat re-registers it.
	materializeDeliveriesStatement = "INSERT INTO " + deliveryTableName + " " +
		"(message_id, listener_id, instance, status, attempts) " +
		"SELECT m.id, ?, ?, ?, 0 FROM " + messageTableName + " m " +
		"WHERE m.event_type = ? " +
		"AND m.publication_date >= (SELECT c.frontier FROM " + consumerTableName + " c " +
		"WHERE c.listener_id = ? AND c.instance = ? AND c.event_type = ?) " +
		"AND NOT EXISTS (SELECT 1 FROM " + deliveryTableName + " d " +
		"WHERE d.message_id = m.id AND d.listener_id = ? AND d.instance = ?) " +
		"ON CONFLICT DO NOTHING"

	advanceFrontierStatement = "UPDATE " + consumerTableName + " SET frontier = ? " +
		"WHERE listener_id = ? AND instance = ? AND event_type = ? AND frontier < ?"

	findDueDeliveriesStatement = "SELECT " + dueDeliveryColumns + " " +
		"FROM " + deliveryTableName + " d " +
		"JOIN " + messageTableName + " m ON m.id = d.message_id " +
		"WHERE d.listener_id = ? AND d.instance = ? " +
		"AND (d.status = ? OR (d.status = ? AND d.next_attempt_date <= ?) " +
		"OR (d.status = ? AND d.claim_date < ?)) " +
		"ORDER BY m.publication_date ASC LIMIT ?"

	findExhaustedDeliveriesStatement = "SELECT " + dueDeliveryColumns + " " +
		"FROM " + deliveryTableName + " d " +
		"JOIN " + messageTableName + " m ON m.id = d.message_id " +
		"WHERE d.status = ? ORDER BY m.publication_date ASC LIMIT ?"

	// The eligibility guard is re-evaluated by the UPDATE itself, so exactly
	// one of several concurrent claimants wins; the losers observe zero
	// affected rows. Counting the attempt inside the same statement keeps the
	// counter immune to interleaving.
	claimDeliveryStatement = "UPDATE " + deliveryTableName + " " +
		"SET status = ?, claim_token = ?, claim_date = ?, attempts = attempts + 1 " +
		"WHERE message_id = ? AND listener_id = ? AND instance = ? " +
		"AND (status = ? OR (status = ? AND next_attempt_date <= ?) " +
		"OR (status = ? AND claim_date < ?))"

	completeDeliveryStatement = "UPDATE " + deliveryTableName + " " +
		"SET status = ?, completion_date = ?, last_error = '' " +
		"WHERE message_id = ? AND listener_id = ? AND instance = ? " +
		"AND status = ? AND claim_token = ?"

	failDeliveryStatement = "UPDATE " + deliveryTableName + " " +
		"SET status = CASE WHEN attempts >= ? THEN ? ELSE ? END, " +
		"last_error = ?, next_attempt_date = ? " +
		"WHERE message_id = ? AND listener_id = ? AND instance = ? " +
		"AND status = ? AND claim_token = ?"

	resubmitDeliveryStatement = "UPDATE " + deliveryTableName + " " +
		"SET status = ?, attempts = 0, next_attempt_date = NULL, last_error = '' " +
		"WHERE message_id = ? AND listener_id = ? AND instance = ? AND status IN (?, ?)"

	expireBroadcastConsumersStatement = "DELETE FROM " + consumerTableName + " " +
		"WHERE delivery_mode = ? AND heartbeat_date < ?"

	// A message is settled when every registered consumer whose registration
	// covers it (matching event type, start boundary at or before the
	// publication) holds a completed delivery for it. Exhausted deliveries do
	// not count as settled: a dead letter pins its message, and so its
	// payload, until Bridge.Resubmit revives it or the hard age cap removes
	// it loudly.
	deleteSettledMessagesStatement = "DELETE FROM " + messageTableName + " " +
		"WHERE publication_date < ? AND NOT EXISTS (" +
		"SELECT 1 FROM " + consumerTableName + " c " +
		"WHERE c.event_type = " + messageTableName + ".event_type " +
		"AND c.start_boundary <= " + messageTableName + ".publication_date " +
		"AND NOT EXISTS (SELECT 1 FROM " + deliveryTableName + " d " +
		"WHERE d.message_id = " + messageTableName + ".id " +
		"AND d.listener_id = c.listener_id AND d.instance = c.instance " +
		"AND d.status = ?))"

	deleteMessagesOlderThanStatement = "DELETE FROM " + messageTableName + " WHERE publication_date < ?"

	deleteDeliveriesWithoutMessageStatement = "DELETE FROM " + deliveryTableName + " " +
		"WHERE NOT EXISTS (SELECT 1 FROM " + messageTableName + " m " +
		"WHERE m.id = " + deliveryTableName + ".message_id)"

	deleteDeliveriesWithoutConsumerStatement = "DELETE FROM " + deliveryTableName + " " +
		"WHERE NOT EXISTS (SELECT 1 FROM " + consumerTableName + " c " +
		"WHERE c.listener_id = " + deliveryTableName + ".listener_id " +
		"AND c.instance = " + deliveryTableName + ".instance)"
)

// dialect captures the per-database differences the store needs at query
// time: which Dialect value to hand the Migrator, and how positional
// placeholders are rendered.
type dialect struct {
	kind        Dialect
	placeholder func(statement string) string
}

func sqliteDialect() dialect {
	return dialect{kind: DialectSQLite, placeholder: keepPlaceholders}
}

func postgresDialect() dialect {
	return dialect{kind: DialectPostgres, placeholder: dollarPlaceholders}
}

// keepPlaceholders leaves the ? placeholders SQLite understands unchanged.
func keepPlaceholders(statement string) string { return statement }

// dollarPlaceholders rewrites the positional ? placeholders to PostgreSQL's
// $N form. The statements in this package never contain a ? inside a string
// literal, so a straight left-to-right substitution is correct.
func dollarPlaceholders(statement string) string {
	var builder strings.Builder
	builder.Grow(len(statement) + 8)
	parameter := 0
	for index := 0; index < len(statement); index++ {
		if statement[index] == '?' {
			parameter++
			builder.WriteByte('$')
			builder.WriteString(strconv.Itoa(parameter))
			continue
		}
		builder.WriteByte(statement[index])
	}
	return builder.String()
}

// sqlStore is the dialect-parameterized implementation shared by SQLiteStore
// and PostgresStore. It writes messages and deliveries with native SQL and
// brings the schema up to date through the configured Migrator
// (NativeMigrator by default), so the core depends only on database/sql.
type sqlStore struct {
	database *sql.DB
	dialect  dialect
	migrator Migrator
}

// Option customizes a store at construction time.
type Option func(*sqlStore)

// WithMigrator overrides the default native Migrator the store uses in
// Initialize. The default keeps the sqlbus core free of any migration-engine
// dependency; pass gowaymigrator.New() from
// github.com/cgardev/gokeel/sqlbus/gowaymigrator to opt in to goway.
func WithMigrator(m Migrator) Option {
	return func(s *sqlStore) {
		if m != nil {
			s.migrator = m
		}
	}
}

func newSQLStore(database *sql.DB, d dialect, options ...Option) *sqlStore {
	s := &sqlStore{
		database: database,
		dialect:  d,
		migrator: NativeMigrator{}, // zero-configuration default, no goway
	}
	for _, option := range options {
		option(s)
	}
	return s
}

func (s *sqlStore) exec(
	ctx context.Context, querier Querier, statement string, args ...any,
) (sql.Result, error) {
	return querier.ExecContext(ctx, s.dialect.placeholder(statement), args...)
}

func (s *sqlStore) query(
	ctx context.Context, querier Querier, statement string, args ...any,
) (*sql.Rows, error) {
	return querier.QueryContext(ctx, s.dialect.placeholder(statement), args...)
}

func (s *sqlStore) execCounting(
	ctx context.Context, statement string, args ...any,
) (int64, error) {
	result, err := s.exec(ctx, s.database, statement, args...)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// Initialize brings the database schema up to date by applying the embedded
// migration scripts through the configured Migrator. The default native
// Migrator uses database/sql only; a goway-backed Migrator can be supplied
// with WithMigrator.
func (s *sqlStore) Initialize(ctx context.Context) error {
	if err := s.migrator.Migrate(ctx, s.database, s.dialect.kind, Schema()); err != nil {
		return fmt.Errorf("apply sqlbus schema: %w", err)
	}
	return nil
}

// CreateMessage writes the message through the provided querier, so it joins
// the transaction of the business change that produced the event.
func (s *sqlStore) CreateMessage(ctx context.Context, querier Querier, message Message) error {
	_, err := s.exec(ctx, querier, insertMessageStatement,
		message.ID.String(),
		message.EventType,
		message.SerializedEvent,
		string(message.PublisherNode),
		formatTime(message.PublicationDate),
	)
	if err != nil {
		return fmt.Errorf("persist message %s: %w", message.ID, err)
	}
	return nil
}

// CreateDeliveries writes one pending delivery row per key through the
// provided querier. The conflict-tolerant insert lets a publisher-created row
// and a dispatcher-materialized row for the same key converge instead of
// failing.
func (s *sqlStore) CreateDeliveries(ctx context.Context, querier Querier, keys []DeliveryKey) error {
	for _, key := range keys {
		_, err := s.exec(ctx, querier, insertDeliveryStatement,
			key.MessageID.String(),
			string(key.ListenerID),
			key.Instance,
			string(StatusPending),
		)
		if err != nil {
			return fmt.Errorf("persist delivery of %s to %s: %w", key.MessageID, key.ListenerID, err)
		}
	}
	return nil
}

// RegisterListenerMode records the delivery mode of the listener with
// first-registration-wins semantics and returns the mode that won, so the
// caller can detect a conflicting registration made by another node.
func (s *sqlStore) RegisterListenerMode(
	ctx context.Context, id eventbus.ListenerID, mode DeliveryMode,
) (DeliveryMode, error) {
	if _, err := s.exec(ctx, s.database, insertListenerModeStatement,
		string(id), string(mode)); err != nil {
		return "", fmt.Errorf("register delivery mode of %s: %w", id, err)
	}

	rows, err := s.query(ctx, s.database, selectListenerModeStatement, string(id))
	if err != nil {
		return "", fmt.Errorf("read delivery mode of %s: %w", id, err)
	}
	defer func() {
		// The rows are fully consumed below; Close only releases the handle.
		_ = rows.Close()
	}()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return "", fmt.Errorf("read delivery mode of %s: %w", id, err)
		}
		return "", fmt.Errorf("read delivery mode of %s: registration row is missing", id)
	}
	var winner string
	if err := rows.Scan(&winner); err != nil {
		return "", fmt.Errorf("read delivery mode of %s: %w", id, err)
	}
	return DeliveryMode(winner), nil
}

// RegisterConsumer records the durable consumer registration. Registering an
// existing consumer again keeps the stored boundary and frontier, so a
// durable group resumes where it left off, and refreshes only the heartbeat.
func (s *sqlStore) RegisterConsumer(ctx context.Context, consumer Consumer) error {
	_, err := s.exec(ctx, s.database, insertConsumerStatement,
		string(consumer.ListenerID),
		consumer.Instance,
		consumer.EventType,
		string(consumer.DeliveryMode),
		formatTime(consumer.StartBoundary),
		formatTime(consumer.Frontier),
		formatTime(consumer.RegistrationDate),
		formatTime(consumer.HeartbeatDate),
	)
	if err != nil {
		return fmt.Errorf("register consumer %s/%s for %s: %w",
			consumer.ListenerID, consumer.Instance, consumer.EventType, err)
	}
	return nil
}

// Heartbeat refreshes the liveness timestamp of the consumer. It reports
// false when the registration row no longer exists (for example, after a
// broadcast expiry reaped it), so the caller can re-register with a freshly
// computed boundary instead of resurrecting a stale one.
func (s *sqlStore) Heartbeat(ctx context.Context, key ConsumerKey, at time.Time) (bool, error) {
	result, err := s.exec(ctx, s.database, heartbeatStatement,
		formatTime(at), string(key.ListenerID), key.Instance, key.EventType)
	if err != nil {
		return false, fmt.Errorf("heartbeat consumer %s/%s: %w", key.ListenerID, key.Instance, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("inspect heartbeat of consumer %s/%s: %w", key.ListenerID, key.Instance, err)
	}
	return affected > 0, nil
}

// RemoveListener deletes every consumer registration and the delivery-mode
// row of the listener, unpinning its messages from retention.
func (s *sqlStore) RemoveListener(ctx context.Context, id eventbus.ListenerID) error {
	if _, err := s.exec(ctx, s.database, deleteConsumersStatement, string(id)); err != nil {
		return fmt.Errorf("remove consumers of %s: %w", id, err)
	}
	if _, err := s.exec(ctx, s.database, deleteListenerModeStatement, string(id)); err != nil {
		return fmt.Errorf("remove delivery mode of %s: %w", id, err)
	}
	return nil
}

// MaterializeDeliveries inserts one pending delivery row for every message
// the consumer covers but has no row for yet, scanning from the consumer's
// durable frontier. The composite primary key makes the insert idempotent, so
// concurrent materializations of one consumer on several nodes converge.
func (s *sqlStore) MaterializeDeliveries(ctx context.Context, key ConsumerKey) (int64, error) {
	count, err := s.execCounting(ctx, materializeDeliveriesStatement,
		string(key.ListenerID),
		key.Instance,
		string(StatusPending),
		key.EventType,
		string(key.ListenerID),
		key.Instance,
		key.EventType,
		string(key.ListenerID),
		key.Instance,
	)
	if err != nil {
		return 0, fmt.Errorf("materialize deliveries of %s/%s for %s: %w",
			key.ListenerID, key.Instance, key.EventType, err)
	}
	return count, nil
}

// AdvanceFrontier raises the materialization floor of the consumer. The
// monotonic guard makes concurrent advances from several nodes converge on
// the highest value.
func (s *sqlStore) AdvanceFrontier(ctx context.Context, key ConsumerKey, frontier time.Time) error {
	value := formatTime(frontier)
	_, err := s.exec(ctx, s.database, advanceFrontierStatement,
		value, string(key.ListenerID), key.Instance, key.EventType, value)
	if err != nil {
		return fmt.Errorf("advance frontier of %s/%s for %s: %w",
			key.ListenerID, key.Instance, key.EventType, err)
	}
	return nil
}

// FindDueDeliveries returns the deliveries of the consumer that are ready to
// claim, oldest publication first: pending rows, failed rows past their
// backoff, and processing rows whose claim lease expired.
func (s *sqlStore) FindDueDeliveries(
	ctx context.Context,
	listener eventbus.ListenerID,
	instance string,
	now time.Time,
	leaseCutoff time.Time,
	limit int,
) ([]DueDelivery, error) {
	rows, err := s.query(ctx, s.database, findDueDeliveriesStatement,
		string(listener),
		instance,
		string(StatusPending),
		string(StatusFailed),
		formatTime(now),
		string(StatusProcessing),
		formatTime(leaseCutoff),
		int64(limit),
	)
	if err != nil {
		return nil, fmt.Errorf("find due deliveries of %s/%s: %w", listener, instance, err)
	}
	defer func() {
		// The rows are fully consumed below; Close only releases the handle.
		_ = rows.Close()
	}()

	var deliveries []DueDelivery
	for rows.Next() {
		delivery, err := scanDueDelivery(rows)
		if err != nil {
			return nil, err
		}
		deliveries = append(deliveries, delivery)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("find due deliveries of %s/%s: %w", listener, instance, err)
	}
	return deliveries, nil
}

// FindExhaustedDeliveries returns the dead letters, oldest publication first,
// up to the limit.
func (s *sqlStore) FindExhaustedDeliveries(ctx context.Context, limit int) ([]DueDelivery, error) {
	rows, err := s.query(ctx, s.database, findExhaustedDeliveriesStatement,
		string(StatusExhausted), int64(limit))
	if err != nil {
		return nil, fmt.Errorf("find exhausted deliveries: %w", err)
	}
	defer func() {
		// The rows are fully consumed below; Close only releases the handle.
		_ = rows.Close()
	}()

	var deliveries []DueDelivery
	for rows.Next() {
		delivery, err := scanDueDelivery(rows)
		if err != nil {
			return nil, err
		}
		deliveries = append(deliveries, delivery)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("find exhausted deliveries: %w", err)
	}
	return deliveries, nil
}

// ClaimDelivery atomically transitions the delivery into processing under the
// given token, counting the attempt. The eligibility guard makes the update
// succeed for exactly one of several concurrent claimants; the losers observe
// zero affected rows.
func (s *sqlStore) ClaimDelivery(
	ctx context.Context, key DeliveryKey, token string, now time.Time, leaseCutoff time.Time,
) (bool, error) {
	result, err := s.exec(ctx, s.database, claimDeliveryStatement,
		string(StatusProcessing),
		token,
		formatTime(now),
		key.MessageID.String(),
		string(key.ListenerID),
		key.Instance,
		string(StatusPending),
		string(StatusFailed),
		formatTime(now),
		string(StatusProcessing),
		formatTime(leaseCutoff),
	)
	if err != nil {
		return false, fmt.Errorf("claim delivery of %s to %s: %w", key.MessageID, key.ListenerID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("inspect claim of %s to %s: %w", key.MessageID, key.ListenerID, err)
	}
	return affected > 0, nil
}

// CompleteDelivery settles a successful delivery. The claim-token fence makes
// a zombie dispatcher, whose lease expired and whose claim was stolen, affect
// zero rows; it reports whether this caller's settlement won.
func (s *sqlStore) CompleteDelivery(
	ctx context.Context, key DeliveryKey, token string, completionDate time.Time,
) (bool, error) {
	result, err := s.exec(ctx, s.database, completeDeliveryStatement,
		string(StatusCompleted),
		formatTime(completionDate),
		key.MessageID.String(),
		string(key.ListenerID),
		key.Instance,
		string(StatusProcessing),
		token,
	)
	if err != nil {
		return false, fmt.Errorf("complete delivery of %s to %s: %w", key.MessageID, key.ListenerID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("inspect completion of %s to %s: %w", key.MessageID, key.ListenerID, err)
	}
	return affected > 0, nil
}

// FailDelivery settles a failed delivery: it records the cause and the next
// attempt date, and moves the delivery to failed, or to exhausted once the
// attempt budget is spent. The claim-token fence mirrors CompleteDelivery.
func (s *sqlStore) FailDelivery(
	ctx context.Context,
	key DeliveryKey,
	token string,
	cause string,
	nextAttemptDate time.Time,
	maximumAttempts int,
) (bool, error) {
	result, err := s.exec(ctx, s.database, failDeliveryStatement,
		int64(maximumAttempts),
		string(StatusExhausted),
		string(StatusFailed),
		cause,
		formatTime(nextAttemptDate),
		key.MessageID.String(),
		string(key.ListenerID),
		key.Instance,
		string(StatusProcessing),
		token,
	)
	if err != nil {
		return false, fmt.Errorf("fail delivery of %s to %s: %w", key.MessageID, key.ListenerID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("inspect failure of %s to %s: %w", key.MessageID, key.ListenerID, err)
	}
	return affected > 0, nil
}

// ResubmitDelivery gives a failed or exhausted delivery a fresh attempt
// budget. It reports false when the delivery is not in a resubmittable state.
func (s *sqlStore) ResubmitDelivery(ctx context.Context, key DeliveryKey) (bool, error) {
	result, err := s.exec(ctx, s.database, resubmitDeliveryStatement,
		string(StatusPending),
		key.MessageID.String(),
		string(key.ListenerID),
		key.Instance,
		string(StatusFailed),
		string(StatusExhausted),
	)
	if err != nil {
		return false, fmt.Errorf("resubmit delivery of %s to %s: %w", key.MessageID, key.ListenerID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("inspect resubmission of %s to %s: %w", key.MessageID, key.ListenerID, err)
	}
	return affected > 0, nil
}

// ExpireBroadcastConsumers removes broadcast registrations whose heartbeat is
// older than the cutoff, so consumers of nodes that left the cluster stop
// pinning messages. Competing registrations are durable and never expire.
func (s *sqlStore) ExpireBroadcastConsumers(ctx context.Context, cutoff time.Time) (int64, error) {
	count, err := s.execCounting(ctx, expireBroadcastConsumersStatement,
		string(DeliveryModeBroadcast), formatTime(cutoff))
	if err != nil {
		return 0, fmt.Errorf("expire broadcast consumers: %w", err)
	}
	return count, nil
}

// DeleteSettledMessages removes messages older than the reference that every
// covering registered consumer has completed.
func (s *sqlStore) DeleteSettledMessages(ctx context.Context, olderThan time.Time) (int64, error) {
	count, err := s.execCounting(ctx, deleteSettledMessagesStatement,
		formatTime(olderThan), string(StatusCompleted))
	if err != nil {
		return 0, fmt.Errorf("delete settled messages: %w", err)
	}
	return count, nil
}

// DeleteMessagesOlderThan unconditionally removes messages older than the
// cutoff. It is the hard age cap that bounds table growth when unsettled
// deliveries would otherwise pin messages forever; the caller reports every
// non-zero count loudly.
func (s *sqlStore) DeleteMessagesOlderThan(ctx context.Context, cutoff time.Time) (int64, error) {
	count, err := s.execCounting(ctx, deleteMessagesOlderThanStatement, formatTime(cutoff))
	if err != nil {
		return 0, fmt.Errorf("delete messages past the maximum age: %w", err)
	}
	return count, nil
}

// DeleteOrphanDeliveries removes deliveries whose message was deleted and
// deliveries whose consumer registration no longer exists. It must run after
// message deletion, never before: completed delivery rows are the memory of
// the materialization anti-join, so removing them while their message
// survives would resurrect the message as a fresh delivery.
func (s *sqlStore) DeleteOrphanDeliveries(ctx context.Context) (int64, error) {
	withoutMessage, err := s.execCounting(ctx, deleteDeliveriesWithoutMessageStatement)
	if err != nil {
		return 0, fmt.Errorf("delete deliveries without a message: %w", err)
	}
	withoutConsumer, err := s.execCounting(ctx, deleteDeliveriesWithoutConsumerStatement)
	if err != nil {
		return withoutMessage, fmt.Errorf("delete deliveries without a consumer: %w", err)
	}
	return withoutMessage + withoutConsumer, nil
}

func scanDueDelivery(rows *sql.Rows) (DueDelivery, error) {
	var (
		messageID       string
		listenerID      string
		instance        string
		status          string
		attempts        int64
		claimToken      string
		claimDate       sql.NullString
		nextAttemptDate sql.NullString
		completionDate  sql.NullString
		lastError       string
		eventType       string
		serializedEvent string
		publisherNode   string
		publicationDate string
	)
	if err := rows.Scan(&messageID, &listenerID, &instance, &status, &attempts,
		&claimToken, &claimDate, &nextAttemptDate, &completionDate, &lastError,
		&eventType, &serializedEvent, &publisherNode, &publicationDate); err != nil {
		return DueDelivery{}, fmt.Errorf("scan due delivery: %w", err)
	}

	parsedID, err := uuid.Parse(messageID)
	if err != nil {
		return DueDelivery{}, fmt.Errorf("parse message identifier %s: %w", messageID, err)
	}
	parsedPublicationDate, err := parseTime(publicationDate)
	if err != nil {
		return DueDelivery{}, fmt.Errorf("parse publication date of %s: %w", messageID, err)
	}
	parsedClaimDate, err := parseNullableTime(claimDate)
	if err != nil {
		return DueDelivery{}, fmt.Errorf("parse claim date of %s: %w", messageID, err)
	}
	parsedNextAttemptDate, err := parseNullableTime(nextAttemptDate)
	if err != nil {
		return DueDelivery{}, fmt.Errorf("parse next attempt date of %s: %w", messageID, err)
	}
	parsedCompletionDate, err := parseNullableTime(completionDate)
	if err != nil {
		return DueDelivery{}, fmt.Errorf("parse completion date of %s: %w", messageID, err)
	}

	return DueDelivery{
		Delivery: Delivery{
			Key: DeliveryKey{
				MessageID:  parsedID,
				ListenerID: eventbus.ListenerID(listenerID),
				Instance:   instance,
			},
			Status:          Status(status),
			Attempts:        int(attempts),
			ClaimToken:      claimToken,
			ClaimDate:       parsedClaimDate,
			NextAttemptDate: parsedNextAttemptDate,
			CompletionDate:  parsedCompletionDate,
			LastError:       lastError,
		},
		Message: Message{
			ID:              parsedID,
			EventType:       eventType,
			SerializedEvent: serializedEvent,
			PublisherNode:   NodeID(publisherNode),
			PublicationDate: parsedPublicationDate,
		},
	}, nil
}

func formatTime(value time.Time) string {
	return value.UTC().Format(timeLayout)
}

func parseTime(value string) (time.Time, error) {
	return time.Parse(timeLayout, value)
}

func parseNullableTime(value sql.NullString) (*time.Time, error) {
	if !value.Valid {
		return nil, nil
	}
	parsed, err := parseTime(value.String)
	if err != nil {
		return nil, err
	}
	return &parsed, nil
}

// SQLiteStore is a Store backed by a SQLite database.
type SQLiteStore struct {
	*sqlStore
}

var _ Store = (*SQLiteStore)(nil)

// NewSQLiteStore constructs a SQLiteStore on top of an open SQLite database.
func NewSQLiteStore(database *sql.DB, options ...Option) *SQLiteStore {
	return &SQLiteStore{newSQLStore(database, sqliteDialect(), options...)}
}

// PostgresStore is a Store backed by a PostgreSQL database.
type PostgresStore struct {
	*sqlStore
}

var _ Store = (*PostgresStore)(nil)

// NewPostgresStore constructs a PostgresStore on top of an open PostgreSQL
// database. The schema and queries are the same as the SQLite store; only the
// migration dialect and the placeholder style differ.
func NewPostgresStore(database *sql.DB, options ...Option) *PostgresStore {
	return &PostgresStore{newSQLStore(database, postgresDialect(), options...)}
}
