package sqlbus

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/cgardev/gokeel/eventbus"

	"github.com/cgardev/gooq"
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
// load-bearing: materialization frontiers, due-delivery ordering, the FIFO
// total order, and retention all compare these columns in SQL.
const timeLayout = "2006-01-02T15:04:05.000000000Z"

// dialect captures the per-database differences the store needs: which
// Dialect value to hand the Migrator, and which gooq dialect renders the
// statements.
type dialect struct {
	kind Dialect
	gooq gooq.Dialect
}

func sqliteDialect() dialect {
	return dialect{kind: DialectSQLite, gooq: gooq.SQLite()}
}

func postgresDialect() dialect {
	return dialect{kind: DialectPostgres, gooq: gooq.Postgres()}
}

// sqlStore is the dialect-parameterized implementation shared by SQLiteStore
// and PostgresStore. Every statement is built with gooq and rendered for the
// active dialect at call time; the schema is brought up to date through the
// configured Migrator (NativeMigrator by default).
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

// renderable is the statement surface the query helpers need: every gooq
// builder renders itself for a chosen dialect.
type renderable interface {
	SQLFor(d gooq.Dialect) (string, []any, error)
}

// execute renders the statement for the active dialect and runs it through
// the querier.
func (s *sqlStore) execute(
	ctx context.Context, querier Querier, statement renderable,
) (sql.Result, error) {
	query, args, err := statement.SQLFor(s.dialect.gooq)
	if err != nil {
		return nil, err
	}
	return querier.ExecContext(ctx, query, args...)
}

// executeCounting executes the statement on the store connection and returns
// how many rows it affected.
func (s *sqlStore) executeCounting(ctx context.Context, statement renderable) (int64, error) {
	result, err := s.execute(ctx, s.database, statement)
	if err != nil {
		return 0, err
	}
	return result.RowsAffected()
}

// fetch renders the query for the active dialect and returns its rows.
func (s *sqlStore) fetch(ctx context.Context, statement renderable) (*sql.Rows, error) {
	query, args, err := statement.SQLFor(s.dialect.gooq)
	if err != nil {
		return nil, err
	}
	return s.database.QueryContext(ctx, query, args...)
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
	insert := gooq.InsertInto(gooqMessage).
		Columns(gooqMessage.ID, gooqMessage.EventType, gooqMessage.SerializedEvent,
			gooqMessage.PublisherNode, gooqMessage.PublicationDate).
		Values(message.ID.String(), message.EventType, message.SerializedEvent,
			string(message.PublisherNode), formatTime(message.PublicationDate))
	if _, err := s.execute(ctx, querier, insert); err != nil {
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
		insert := gooq.InsertInto(gooqDelivery).
			Columns(gooqDelivery.MessageID, gooqDelivery.ListenerID,
				gooqDelivery.Instance, gooqDelivery.Status, gooqDelivery.Attempts).
			Values(key.MessageID.String(), string(key.ListenerID), key.Instance,
				string(StatusPending), int64(0)).
			OnConflictDoNothing()
		if _, err := s.execute(ctx, querier, insert); err != nil {
			return fmt.Errorf("persist delivery of %s to %s: %w", key.MessageID, key.ListenerID, err)
		}
	}
	return nil
}

// RegisterListenerMode records the delivery mode and the ordering of the
// listener with first-registration-wins semantics and returns the pair that
// won, so the caller can detect a conflicting registration made by another
// node.
func (s *sqlStore) RegisterListenerMode(
	ctx context.Context, id eventbus.ListenerID, mode DeliveryMode, ordering Ordering,
) (DeliveryMode, Ordering, error) {
	insert := gooq.InsertInto(gooqListener).
		Columns(gooqListener.ListenerID, gooqListener.DeliveryMode, gooqListener.Ordering).
		Values(string(id), string(mode), string(ordering)).
		OnConflictDoNothing()
	if _, err := s.execute(ctx, s.database, insert); err != nil {
		return "", "", fmt.Errorf("register delivery mode of %s: %w", id, err)
	}

	winners, err := gooq.Select2(gooqListener.DeliveryMode, gooqListener.Ordering).
		From(gooqListener).
		Where(gooqListener.ListenerID.EQ(string(id))).
		Using(s.dialect.gooq).
		Fetch(ctx, s.database)
	if err != nil {
		return "", "", fmt.Errorf("read delivery mode of %s: %w", id, err)
	}
	if len(winners) == 0 {
		return "", "", fmt.Errorf("read delivery mode of %s: registration row is missing", id)
	}
	return DeliveryMode(winners[0].V1), Ordering(winners[0].V2), nil
}

// RegisterConsumer records the durable consumer registration. Registering an
// existing consumer again keeps the stored boundary and frontier, so a
// durable group resumes where it left off, and refreshes only the heartbeat:
// a node re-attaching under a stable identity must rejoin with fresh
// liveness, not with the staleness it accumulated while it was down.
func (s *sqlStore) RegisterConsumer(ctx context.Context, consumer Consumer) error {
	insert := gooq.InsertInto(gooqConsumer).
		Columns(gooqConsumer.ListenerID, gooqConsumer.Instance, gooqConsumer.EventType,
			gooqConsumer.DeliveryMode, gooqConsumer.StartBoundary, gooqConsumer.Frontier,
			gooqConsumer.RegistrationDate, gooqConsumer.HeartbeatDate).
		Values(string(consumer.ListenerID), consumer.Instance, consumer.EventType,
			string(consumer.DeliveryMode), formatTime(consumer.StartBoundary),
			formatTime(consumer.Frontier), formatTime(consumer.RegistrationDate),
			formatTime(consumer.HeartbeatDate)).
		OnConflict(gooqConsumer.ListenerID, gooqConsumer.Instance, gooqConsumer.EventType).
		DoUpdateSet(gooq.SetToExcluded(gooqConsumer.HeartbeatDate))
	if _, err := s.execute(ctx, s.database, insert); err != nil {
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
	update := gooq.Update(gooqConsumer).
		Set(gooqConsumer.HeartbeatDate.Set(formatTime(at))).
		Where(consumerKeyCondition(gooqConsumer, key))
	result, err := s.execute(ctx, s.database, update)
	if err != nil {
		return false, fmt.Errorf("heartbeat consumer %s/%s: %w", key.ListenerID, key.Instance, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("inspect heartbeat of consumer %s/%s: %w", key.ListenerID, key.Instance, err)
	}
	return affected > 0, nil
}

// consumerKeyCondition matches one durable consumer registration.
func consumerKeyCondition(c *consumerTable, key ConsumerKey) gooq.Condition {
	return c.ListenerID.EQ(string(key.ListenerID)).
		And(c.Instance.EQ(key.Instance)).
		And(c.EventType.EQ(key.EventType))
}

// RemoveListener deletes every consumer registration and the delivery-mode
// row of the listener, unpinning its messages from retention.
func (s *sqlStore) RemoveListener(ctx context.Context, id eventbus.ListenerID) error {
	consumers := gooq.DeleteFrom(gooqConsumer).Where(gooqConsumer.ListenerID.EQ(string(id)))
	if _, err := s.execute(ctx, s.database, consumers); err != nil {
		return fmt.Errorf("remove consumers of %s: %w", id, err)
	}
	modes := gooq.DeleteFrom(gooqListener).Where(gooqListener.ListenerID.EQ(string(id)))
	if _, err := s.execute(ctx, s.database, modes); err != nil {
		return fmt.Errorf("remove delivery mode of %s: %w", id, err)
	}
	return nil
}

// MaterializeDeliveries inserts one pending delivery row for every message
// the consumer covers but has no row for yet, scanning from the consumer's
// durable frontier. The frontier is read from the consumer row inside the
// statement, so the scan floor is always the durable one; a missing consumer
// row (for example, one reaped by broadcast expiry) yields a NULL frontier,
// whose comparison matches no rows. The composite primary key makes the
// insert idempotent, so concurrent materializations converge.
func (s *sqlStore) MaterializeDeliveries(ctx context.Context, key ConsumerKey) (int64, error) {
	m := newMessageTable("m")
	existing := newDeliveryTable("d")

	// The conflict clause is attached before the select source: both calls
	// mutate the same statement, and the step interface only exposes the
	// conflict step ahead of the source.
	insert := gooq.InsertInto(gooqDelivery).
		Columns(gooqDelivery.MessageID, gooqDelivery.ListenerID,
			gooqDelivery.Instance, gooqDelivery.Status, gooqDelivery.Attempts)
	insert.OnConflictDoNothing()
	statement := insert.Select(
		gooq.Select5(
			m.ID,
			gooq.RawValue[string]("?", string(key.ListenerID)),
			gooq.RawValue[string]("?", key.Instance),
			gooq.RawValue[string]("?", string(StatusPending)),
			gooq.RawValue[int64]("?", int64(0)),
		).From(m).
			Where(m.EventType.EQ(key.EventType).
				And(m.PublicationDate.GEField(frontierOf(key))).
				And(gooq.NotExists(
					gooq.Select1(gooq.Raw[int64]("1")).From(existing).
						Where(existing.MessageID.EQField(m.ID).
							And(existing.ListenerID.EQ(string(key.ListenerID))).
							And(existing.Instance.EQ(key.Instance)))))))

	count, err := s.executeCounting(ctx, statement)
	if err != nil {
		return 0, fmt.Errorf("materialize deliveries of %s/%s for %s: %w",
			key.ListenerID, key.Instance, key.EventType, err)
	}
	return count, nil
}

// frontierOf is the scalar subquery reading the durable frontier of the
// consumer.
func frontierOf(key ConsumerKey) gooq.Field[string] {
	c := newConsumerTable("c")
	return gooq.ScalarSubquery[string](
		gooq.Select1(c.Frontier).From(c).Where(consumerKeyCondition(c, key)))
}

// AdvanceFrontier raises the materialization floor of the consumer. The
// monotonic guard makes concurrent advances from several nodes converge on
// the highest value.
func (s *sqlStore) AdvanceFrontier(ctx context.Context, key ConsumerKey, frontier time.Time) error {
	value := formatTime(frontier)
	update := gooq.Update(gooqConsumer).
		Set(gooqConsumer.Frontier.Set(value)).
		Where(consumerKeyCondition(gooqConsumer, key).And(gooqConsumer.Frontier.LT(value)))
	if _, err := s.execute(ctx, s.database, update); err != nil {
		return fmt.Errorf("advance frontier of %s/%s for %s: %w",
			key.ListenerID, key.Instance, key.EventType, err)
	}
	return nil
}

// dueCondition matches the deliveries that are ready to claim: pending rows,
// failed rows past their backoff, and processing rows whose claim lease
// expired.
func dueCondition(d *deliveryTable, now, leaseCutoff time.Time) gooq.Condition {
	return d.Status.EQ(string(StatusPending)).
		Or(d.Status.EQ(string(StatusFailed)).And(d.NextAttemptDate.LE(formatTime(now)))).
		Or(d.Status.EQ(string(StatusProcessing)).And(d.ClaimDate.LT(formatTime(leaseCutoff))))
}

// earlierIncompleteExists matches when the consumer holds an incomplete
// delivery earlier than the given position in the total order
// (publication_date, message_id). Exhausted deliveries deliberately do not
// count: a dead letter parks and its successors continue.
func earlierIncompleteExists(
	listener eventbus.ListenerID, instance string,
	before gooq.Field[string], beforeID gooq.Field[string],
) gooq.Condition {
	d2 := newDeliveryTable("d2")
	m2 := newMessageTable("m2")
	return gooq.Exists(
		gooq.Select1(gooq.Raw[int64]("1")).From(d2).
			Join(m2).On(m2.ID.EQField(d2.MessageID)).
			Where(d2.ListenerID.EQ(string(listener)).
				And(d2.Instance.EQ(instance)).
				And(d2.Status.In(incompleteStatuses()...)).
				And(m2.PublicationDate.LTField(before).
					Or(m2.PublicationDate.EQField(before).And(m2.ID.LTField(beforeID))))))
}

// concurrentProcessingExists matches when the consumer holds another delivery
// in processing under a live lease. FIFO execution is serial per consumer:
// without this guard a resubmitted predecessor, which nothing precedes in the
// total order, could start while its successor is still running.
func concurrentProcessingExists(
	listener eventbus.ListenerID, instance string,
	other uuid.UUID, leaseCutoff time.Time,
) gooq.Condition {
	d3 := newDeliveryTable("d3")
	return gooq.Exists(
		gooq.Select1(gooq.Raw[int64]("1")).From(d3).
			Where(d3.ListenerID.EQ(string(listener)).
				And(d3.Instance.EQ(instance)).
				And(d3.Status.EQ(string(StatusProcessing))).
				And(d3.ClaimDate.GE(formatTime(leaseCutoff))).
				And(d3.MessageID.NE(other.String()))))
}

// FindDueDeliveries returns the claimable deliveries of the consumer in the
// total order (publication_date, message_id). For a FIFO consumer only the
// head of the queue is claimable, and only below the materialization
// frontier: below that watermark every visible message already has its
// delivery row, so no late-committing publication can slot in front of the
// head.
func (s *sqlStore) FindDueDeliveries(
	ctx context.Context,
	key ConsumerKey,
	ordering Ordering,
	now time.Time,
	leaseCutoff time.Time,
	limit int,
) ([]DueDelivery, error) {
	d := newDeliveryTable("d")
	m := newMessageTable("m")

	condition := d.ListenerID.EQ(string(key.ListenerID)).
		And(d.Instance.EQ(key.Instance)).
		And(dueCondition(d, now, leaseCutoff))
	if ordering == OrderingFIFO {
		condition = condition.
			And(m.PublicationDate.LTField(frontierOf(key))).
			And(gooq.Not(earlierIncompleteExists(
				key.ListenerID, key.Instance, m.PublicationDate, m.ID)))
	}

	query := gooq.Select14(
		d.MessageID, d.ListenerID, d.Instance, d.Status, d.Attempts,
		d.ClaimToken,
		gooq.NewField[sql.NullString](d.TableImpl, "claim_date"),
		gooq.NewField[sql.NullString](d.TableImpl, "next_attempt_date"),
		gooq.NewField[sql.NullString](d.TableImpl, "completion_date"),
		d.LastError,
		m.EventType, m.SerializedEvent, m.PublisherNode, m.PublicationDate,
	).From(d).
		Join(m).On(m.ID.EQField(d.MessageID)).
		Where(condition).
		OrderBy(m.PublicationDate.Asc(), m.ID.Asc()).
		Limit(int64(limit))

	rows, err := s.fetch(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("find due deliveries of %s/%s: %w", key.ListenerID, key.Instance, err)
	}
	deliveries, err := collectDueDeliveries(rows)
	if err != nil {
		return nil, fmt.Errorf("find due deliveries of %s/%s: %w", key.ListenerID, key.Instance, err)
	}
	return deliveries, nil
}

// FindExhaustedDeliveries returns the dead letters, oldest publication first,
// up to the limit.
func (s *sqlStore) FindExhaustedDeliveries(ctx context.Context, limit int) ([]DueDelivery, error) {
	d := newDeliveryTable("d")
	m := newMessageTable("m")
	query := gooq.Select14(
		d.MessageID, d.ListenerID, d.Instance, d.Status, d.Attempts,
		d.ClaimToken,
		gooq.NewField[sql.NullString](d.TableImpl, "claim_date"),
		gooq.NewField[sql.NullString](d.TableImpl, "next_attempt_date"),
		gooq.NewField[sql.NullString](d.TableImpl, "completion_date"),
		d.LastError,
		m.EventType, m.SerializedEvent, m.PublisherNode, m.PublicationDate,
	).From(d).
		Join(m).On(m.ID.EQField(d.MessageID)).
		Where(d.Status.EQ(string(StatusExhausted))).
		OrderBy(m.PublicationDate.Asc(), m.ID.Asc()).
		Limit(int64(limit))

	rows, err := s.fetch(ctx, query)
	if err != nil {
		return nil, fmt.Errorf("find exhausted deliveries: %w", err)
	}
	deliveries, err := collectDueDeliveries(rows)
	if err != nil {
		return nil, fmt.Errorf("find exhausted deliveries: %w", err)
	}
	return deliveries, nil
}

// claimCondition is the shared eligibility guard of the claim statements: the
// row must identify the delivery, be due, and still hold the attempt count
// the claimant observed, so the attempt counter doubles as a fencing
// generation exactly like the outbox store.
func claimCondition(key DeliveryKey, attempts int, now, leaseCutoff time.Time) gooq.Condition {
	return gooqDelivery.MessageID.EQ(key.MessageID.String()).
		And(gooqDelivery.ListenerID.EQ(string(key.ListenerID))).
		And(gooqDelivery.Instance.EQ(key.Instance)).
		And(gooqDelivery.Attempts.EQ(int64(attempts))).
		And(dueCondition(gooqDelivery, now, leaseCutoff))
}

// ClaimDelivery atomically transitions the delivery into processing under the
// given token, counting the attempt. The eligibility and attempt guards are
// re-evaluated by the update itself, so exactly one of several concurrent
// claimants wins; the losers observe zero affected rows.
func (s *sqlStore) ClaimDelivery(
	ctx context.Context, key DeliveryKey, token string,
	now time.Time, leaseCutoff time.Time, attempts int,
) (bool, error) {
	update := gooq.Update(gooqDelivery).
		Set(gooqDelivery.Status.Set(string(StatusProcessing))).
		Set(gooqDelivery.ClaimToken.Set(token)).
		Set(gooqDelivery.ClaimDate.Set(formatTime(now))).
		Set(gooqDelivery.Attempts.Set(int64(attempts + 1))).
		Where(claimCondition(key, attempts, now, leaseCutoff))
	result, err := s.execute(ctx, s.database, update)
	if err != nil {
		return false, fmt.Errorf("claim delivery of %s to %s: %w", key.MessageID, key.ListenerID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("inspect claim of %s to %s: %w", key.MessageID, key.ListenerID, err)
	}
	return affected > 0, nil
}

// ClaimDeliveryInOrder claims like ClaimDelivery but re-verifies inside the
// update that no earlier incomplete delivery of the same consumer exists —
// between the find and the claim a predecessor may have been resubmitted, and
// its revived delivery must run first — and that no other delivery of the
// consumer is processing under a live lease, so FIFO execution stays serial
// even when a revived predecessor and its running successor race. The claimed
// row never excludes itself, because nothing is earlier than itself in the
// total order.
func (s *sqlStore) ClaimDeliveryInOrder(
	ctx context.Context, key DeliveryKey, token string,
	now time.Time, leaseCutoff time.Time, attempts int, publicationDate time.Time,
) (bool, error) {
	position := formatTime(publicationDate)
	update := gooq.Update(gooqDelivery).
		Set(gooqDelivery.Status.Set(string(StatusProcessing))).
		Set(gooqDelivery.ClaimToken.Set(token)).
		Set(gooqDelivery.ClaimDate.Set(formatTime(now))).
		Set(gooqDelivery.Attempts.Set(int64(attempts + 1))).
		Where(claimCondition(key, attempts, now, leaseCutoff).
			And(gooq.Not(earlierIncompleteExists(
				key.ListenerID, key.Instance,
				gooq.RawValue[string]("?", position),
				gooq.RawValue[string]("?", key.MessageID.String())))).
			And(gooq.Not(concurrentProcessingExists(
				key.ListenerID, key.Instance, key.MessageID, leaseCutoff))))
	result, err := s.execute(ctx, s.database, update)
	if err != nil {
		return false, fmt.Errorf("claim delivery of %s to %s in order: %w", key.MessageID, key.ListenerID, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("inspect ordered claim of %s to %s: %w", key.MessageID, key.ListenerID, err)
	}
	return affected > 0, nil
}

// settlementCondition is the claim-token fence of the settlement statements:
// a zombie dispatcher, whose lease expired and whose claim was stolen,
// affects zero rows.
func settlementCondition(key DeliveryKey, token string) gooq.Condition {
	return gooqDelivery.MessageID.EQ(key.MessageID.String()).
		And(gooqDelivery.ListenerID.EQ(string(key.ListenerID))).
		And(gooqDelivery.Instance.EQ(key.Instance)).
		And(gooqDelivery.Status.EQ(string(StatusProcessing))).
		And(gooqDelivery.ClaimToken.EQ(token))
}

// CompleteDelivery settles a successful delivery; it reports whether this
// caller's settlement won.
func (s *sqlStore) CompleteDelivery(
	ctx context.Context, key DeliveryKey, token string, completionDate time.Time,
) (bool, error) {
	update := gooq.Update(gooqDelivery).
		Set(gooqDelivery.Status.Set(string(StatusCompleted))).
		Set(gooqDelivery.CompletionDate.Set(formatTime(completionDate))).
		Set(gooqDelivery.LastError.Set("")).
		Where(settlementCondition(key, token))
	result, err := s.execute(ctx, s.database, update)
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
// attempt budget is spent. The token fence guarantees the attempts value set
// by this dispatcher's claim is still current, so the exhaustion decision is
// computed from it without re-reading the row.
func (s *sqlStore) FailDelivery(
	ctx context.Context,
	key DeliveryKey,
	token string,
	cause string,
	nextAttemptDate time.Time,
	attempts int,
	maximumAttempts int,
) (bool, error) {
	status := StatusFailed
	if attempts >= maximumAttempts {
		status = StatusExhausted
	}
	update := gooq.Update(gooqDelivery).
		Set(gooqDelivery.Status.Set(string(status))).
		Set(gooqDelivery.LastError.Set(cause)).
		Set(gooqDelivery.NextAttemptDate.Set(formatTime(nextAttemptDate))).
		Where(settlementCondition(key, token))
	result, err := s.execute(ctx, s.database, update)
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
// budget, clearing the backoff of the failed attempts so the fresh budget
// starts immediately. It reports false when the delivery is not in a
// resubmittable state.
func (s *sqlStore) ResubmitDelivery(ctx context.Context, key DeliveryKey) (bool, error) {
	update := gooq.Update(gooqDelivery).
		Set(gooqDelivery.Status.Set(string(StatusPending))).
		Set(gooqDelivery.Attempts.Set(int64(0))).
		Set(gooq.NewField[sql.NullString](gooqDelivery.TableImpl, "next_attempt_date").Set(sql.NullString{})).
		Set(gooqDelivery.LastError.Set("")).
		Where(gooqDelivery.MessageID.EQ(key.MessageID.String()).
			And(gooqDelivery.ListenerID.EQ(string(key.ListenerID))).
			And(gooqDelivery.Instance.EQ(key.Instance)).
			And(gooqDelivery.Status.In(string(StatusFailed), string(StatusExhausted))))
	result, err := s.execute(ctx, s.database, update)
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
	statement := gooq.DeleteFrom(gooqConsumer).
		Where(gooqConsumer.DeliveryMode.EQ(string(DeliveryModeBroadcast)).
			And(gooqConsumer.HeartbeatDate.LT(formatTime(cutoff))))
	count, err := s.executeCounting(ctx, statement)
	if err != nil {
		return 0, fmt.Errorf("expire broadcast consumers: %w", err)
	}
	return count, nil
}

// DeleteSettledMessages removes messages older than the reference that every
// covering registered consumer has completed. Exhausted deliveries do not
// count as settled: a dead letter pins its message, and so its payload, until
// Bridge.Resubmit revives it or the hard age cap removes it loudly.
func (s *sqlStore) DeleteSettledMessages(ctx context.Context, olderThan time.Time) (int64, error) {
	c := newConsumerTable("c")
	d := newDeliveryTable("d")
	completed := gooq.Select1(gooq.Raw[int64]("1")).From(d).
		Where(d.MessageID.EQField(gooqMessage.ID).
			And(d.ListenerID.EQField(c.ListenerID)).
			And(d.Instance.EQField(c.Instance)).
			And(d.Status.EQ(string(StatusCompleted))))
	coveringWithoutCompletion := gooq.Select1(gooq.Raw[int64]("1")).From(c).
		Where(c.EventType.EQField(gooqMessage.EventType).
			And(c.StartBoundary.LEField(gooqMessage.PublicationDate)).
			And(gooq.NotExists(completed)))
	statement := gooq.DeleteFrom(gooqMessage).
		Where(gooqMessage.PublicationDate.LT(formatTime(olderThan)).
			And(gooq.NotExists(coveringWithoutCompletion)))
	count, err := s.executeCounting(ctx, statement)
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
	statement := gooq.DeleteFrom(gooqMessage).
		Where(gooqMessage.PublicationDate.LT(formatTime(cutoff)))
	count, err := s.executeCounting(ctx, statement)
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
	m := newMessageTable("m")
	withoutMessage := gooq.DeleteFrom(gooqDelivery).
		Where(gooq.NotExists(
			gooq.Select1(gooq.Raw[int64]("1")).From(m).
				Where(m.ID.EQField(gooqDelivery.MessageID))))
	deletedWithoutMessage, err := s.executeCounting(ctx, withoutMessage)
	if err != nil {
		return 0, fmt.Errorf("delete deliveries without a message: %w", err)
	}

	c := newConsumerTable("c")
	withoutConsumer := gooq.DeleteFrom(gooqDelivery).
		Where(gooq.NotExists(
			gooq.Select1(gooq.Raw[int64]("1")).From(c).
				Where(c.ListenerID.EQField(gooqDelivery.ListenerID).
					And(c.Instance.EQField(gooqDelivery.Instance)))))
	deletedWithoutConsumer, err := s.executeCounting(ctx, withoutConsumer)
	if err != nil {
		return deletedWithoutMessage, fmt.Errorf("delete deliveries without a consumer: %w", err)
	}
	return deletedWithoutMessage + deletedWithoutConsumer, nil
}

// collectDueDeliveries scans every row of a due-delivery projection.
func collectDueDeliveries(rows *sql.Rows) (deliveries []DueDelivery, err error) {
	defer func(rows *sql.Rows) {
		if closeErr := rows.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close due delivery rows: %w", closeErr))
		}
	}(rows)

	for rows.Next() {
		delivery, err := scanDueDelivery(rows)
		if err != nil {
			return nil, err
		}
		deliveries = append(deliveries, delivery)
	}
	if err := rows.Err(); err != nil {
		return nil, err
	}
	return deliveries, nil
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
// rendering dialect and the migration dialect differ.
func NewPostgresStore(database *sql.DB, options ...Option) *PostgresStore {
	return &PostgresStore{newSQLStore(database, postgresDialect(), options...)}
}
