package outbox

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

// CompletionMode selects how a completed publication is settled.
type CompletionMode int

const (
	// CompletionModeUpdate keeps the row, recording status and completion date.
	CompletionModeUpdate CompletionMode = iota

	// CompletionModeDelete removes the row.
	CompletionModeDelete

	// CompletionModeArchive moves the row to the archive table.
	CompletionModeArchive
)

const (
	tableName        = "event_publication"
	archiveTableName = "event_publication_archive"
)

// selectColumns lists the publication columns in the order scanPublication reads
// them. It matches the columns declared by the migration scripts.
const selectColumns = "id, listener_id, event_type, serialized_event, publication_date, " +
	"completion_date, status, completion_attempts, last_resubmission_date"

// The statements are written with positional ? placeholders and rebound to the
// driver's placeholder style by the dialect, mirroring the shape of the Spring
// Modulith JdbcEventPublicationRepository queries.
const (
	insertPublicationStatement = "INSERT INTO " + tableName + " " +
		"(id, listener_id, event_type, serialized_event, publication_date, status, completion_attempts) " +
		"VALUES (?, ?, ?, ?, ?, ?, ?)"

	claimProcessingStatement = "UPDATE " + tableName + " SET status = ? " +
		"WHERE id = ? AND status NOT IN (?, ?)"

	markFailedStatement = "UPDATE " + tableName + " SET status = ? WHERE id = ? AND status != ?"

	markCompletedStatement = "UPDATE " + tableName + " " +
		"SET status = ?, completion_date = ? WHERE id = ?"

	deletePublicationStatement = "DELETE FROM " + tableName + " WHERE id = ?"

	selectForArchiveStatement = "SELECT " + selectColumns + " FROM " + tableName + " WHERE id = ?"

	insertArchiveStatement = "INSERT INTO " + archiveTableName + " " +
		"(id, listener_id, event_type, serialized_event, publication_date, completion_date, status, " +
		"completion_attempts, last_resubmission_date) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?) ON CONFLICT DO NOTHING"

	selectAttemptsStatement = "SELECT completion_attempts FROM " + tableName + " " +
		"WHERE id = ? AND status NOT IN (?, ?)"

	markResubmittedStatement = "UPDATE " + tableName + " " +
		"SET status = ?, completion_attempts = ?, last_resubmission_date = ? " +
		"WHERE id = ? AND status NOT IN (?, ?) AND completion_attempts = ?"

	findIncompleteStatement = "SELECT " + selectColumns + " FROM " + tableName + " " +
		"WHERE status != ? ORDER BY publication_date ASC"

	findIncompleteBeforeStatement = "SELECT " + selectColumns + " FROM " + tableName + " " +
		"WHERE status != ? AND publication_date < ? ORDER BY publication_date ASC"
)

// dialect captures the per-database differences the store needs at query time:
// which Dialect value to hand the Migrator, and how positional placeholders are
// rendered.
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

// dollarPlaceholders rewrites the positional ? placeholders to PostgreSQL's $N
// form. The statements in this package never contain a ? inside a string
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

// sqlStore is the dialect-parameterized implementation shared by SQLiteStore and
// PostgresStore. It writes publications with native SQL and brings the schema up
// to date through the configured Migrator (NativeMigrator by default), so the
// core depends only on database/sql.
type sqlStore struct {
	database       *sql.DB
	completionMode CompletionMode
	dialect        dialect
	migrator       Migrator
}

// Option customizes a store at construction time.
type Option func(*sqlStore)

// WithMigrator overrides the default native Migrator the store uses in
// Initialize. The default keeps the outbox core free of any migration-engine
// dependency; pass gowaymigrator.New() from
// github.com/cgardev/gokeel/outbox/gowaymigrator to opt in to goway.
func WithMigrator(m Migrator) Option {
	return func(s *sqlStore) {
		if m != nil {
			s.migrator = m
		}
	}
}

func newSQLStore(
	database *sql.DB, completionMode CompletionMode, d dialect, options ...Option,
) *sqlStore {
	s := &sqlStore{
		database:       database,
		completionMode: completionMode,
		dialect:        d,
		migrator:       NativeMigrator{}, // zero-configuration default, no goway
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

// Initialize brings the database schema up to date by applying the embedded
// migration scripts through the configured Migrator. The default native Migrator
// uses database/sql only; a goway-backed Migrator can be supplied with
// WithMigrator.
func (s *sqlStore) Initialize(ctx context.Context) error {
	if err := s.migrator.Migrate(ctx, s.database, s.dialect.kind, Schema()); err != nil {
		return fmt.Errorf("apply outbox schema: %w", err)
	}
	return nil
}

// Create writes the publication through the provided querier, so it joins the
// transaction of the business change that produced the event.
func (s *sqlStore) Create(ctx context.Context, querier Querier, publication Publication) error {
	_, err := s.exec(ctx, querier, insertPublicationStatement,
		publication.ID.String(),
		string(publication.ListenerID),
		publication.EventType,
		publication.SerializedEvent,
		formatTime(publication.PublicationDate),
		string(publication.Status),
		int64(publication.CompletionAttempts),
	)
	if err != nil {
		return fmt.Errorf("persist publication %s: %w", publication.ID, err)
	}
	return nil
}

// ClaimProcessing atomically transitions the publication into processing. The
// status guard makes the update succeed for exactly one of several concurrent
// dispatchers; the losers observe zero affected rows.
func (s *sqlStore) ClaimProcessing(ctx context.Context, id uuid.UUID) (bool, error) {
	result, err := s.exec(ctx, s.database, claimProcessingStatement,
		string(StatusProcessing), id.String(), string(StatusProcessing), string(StatusCompleted))
	if err != nil {
		return false, fmt.Errorf("claim publication %s: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("inspect claim of publication %s: %w", id, err)
	}
	return affected > 0, nil
}

// MarkFailed records that processing the publication failed, leaving it
// incomplete for a later resubmission.
func (s *sqlStore) MarkFailed(ctx context.Context, id uuid.UUID) error {
	_, err := s.exec(ctx, s.database, markFailedStatement,
		string(StatusFailed), id.String(), string(StatusCompleted))
	if err != nil {
		return fmt.Errorf("mark publication %s as failed: %w", id, err)
	}
	return nil
}

// MarkCompleted settles the publication according to the completion mode of the
// store.
func (s *sqlStore) MarkCompleted(ctx context.Context, id uuid.UUID, completionDate time.Time) error {
	switch s.completionMode {
	case CompletionModeDelete:
		_, err := s.exec(ctx, s.database, deletePublicationStatement, id.String())
		if err != nil {
			return fmt.Errorf("delete completed publication %s: %w", id, err)
		}
		return nil
	case CompletionModeArchive:
		return s.archive(ctx, id, completionDate)
	default:
		_, err := s.exec(ctx, s.database, markCompletedStatement,
			string(StatusCompleted), formatTime(completionDate), id.String())
		if err != nil {
			return fmt.Errorf("mark publication %s as completed: %w", id, err)
		}
		return nil
	}
}

// archive moves the publication into the archive table as an idempotent sequence
// rather than a read-then-write transaction: the conflict-tolerant insert lets
// concurrent or repeated completions of the same publication converge instead of
// failing, and a crash between the two statements only leaves the source row
// behind, where resubmission settles it again.
func (s *sqlStore) archive(ctx context.Context, id uuid.UUID, completionDate time.Time) error {
	publication, found, err := s.fetchOne(ctx, selectForArchiveStatement, id.String())
	if err != nil {
		return fmt.Errorf("archive publication %s: read: %w", id, err)
	}
	if !found {
		return nil
	}

	_, err = s.exec(ctx, s.database, insertArchiveStatement,
		publication.ID.String(),
		string(publication.ListenerID),
		publication.EventType,
		publication.SerializedEvent,
		formatTime(publication.PublicationDate),
		formatTime(completionDate),
		string(StatusCompleted),
		int64(publication.CompletionAttempts),
		nullableTime(publication.LastResubmissionDate),
	)
	if err != nil {
		return fmt.Errorf("archive publication %s: write archive entry: %w", id, err)
	}

	if _, err := s.exec(ctx, s.database, deletePublicationStatement, id.String()); err != nil {
		return fmt.Errorf("archive publication %s: delete source row: %w", id, err)
	}
	return nil
}

// MarkResubmitted transitions a publication back into delivery, counting the
// attempt. It reports false when another caller resubmitted or settled the
// publication first: the update re-checks both the status and the attempt
// counter it read, so two concurrent resubmissions of the same entry can never
// both succeed, regardless of statement interleaving.
func (s *sqlStore) MarkResubmitted(
	ctx context.Context,
	id uuid.UUID,
	resubmissionDate time.Time,
) (bool, error) {
	rows, err := s.query(ctx, s.database, selectAttemptsStatement,
		id.String(), string(StatusResubmitted), string(StatusCompleted))
	if err != nil {
		return false, fmt.Errorf("mark publication %s as resubmitted: read: %w", id, err)
	}
	attempts, found, err := scanAttempts(rows)
	if err != nil {
		return false, fmt.Errorf("mark publication %s as resubmitted: read: %w", id, err)
	}
	if !found {
		return false, nil
	}

	result, err := s.exec(ctx, s.database, markResubmittedStatement,
		string(StatusResubmitted), attempts+1, formatTime(resubmissionDate),
		id.String(), string(StatusResubmitted), string(StatusCompleted), attempts)
	if err != nil {
		return false, fmt.Errorf("mark publication %s as resubmitted: write: %w", id, err)
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, fmt.Errorf("mark publication %s as resubmitted: inspect: %w", id, err)
	}
	return affected > 0, nil
}

// FindIncomplete returns every publication not yet completed, in publication
// order.
func (s *sqlStore) FindIncomplete(ctx context.Context) ([]Publication, error) {
	rows, err := s.query(ctx, s.database, findIncompleteStatement, string(StatusCompleted))
	return s.collect(rows, err)
}

// FindIncompletePublishedBefore returns every publication not yet completed that
// was published before the reference time, in publication order.
func (s *sqlStore) FindIncompletePublishedBefore(
	ctx context.Context,
	reference time.Time,
) ([]Publication, error) {
	rows, err := s.query(ctx, s.database, findIncompleteBeforeStatement,
		string(StatusCompleted), formatTime(reference))
	return s.collect(rows, err)
}

func (s *sqlStore) collect(rows *sql.Rows, queryErr error) ([]Publication, error) {
	if queryErr != nil {
		return nil, fmt.Errorf("find incomplete publications: %w", queryErr)
	}
	defer rows.Close()

	var publications []Publication
	for rows.Next() {
		publication, err := scanPublication(rows)
		if err != nil {
			return nil, err
		}
		publications = append(publications, publication)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("find incomplete publications: %w", err)
	}
	return publications, nil
}

// fetchOne reads at most one publication, fully consuming and closing the rows
// before returning so a follow-up statement can run on the same database.
func (s *sqlStore) fetchOne(
	ctx context.Context, statement string, args ...any,
) (Publication, bool, error) {
	rows, err := s.query(ctx, s.database, statement, args...)
	if err != nil {
		return Publication{}, false, err
	}
	defer rows.Close()
	if !rows.Next() {
		return Publication{}, false, rows.Err()
	}
	publication, err := scanPublication(rows)
	if err != nil {
		return Publication{}, false, err
	}
	return publication, true, nil
}

func scanAttempts(rows *sql.Rows) (int64, bool, error) {
	defer rows.Close()
	if !rows.Next() {
		return 0, false, rows.Err()
	}
	var attempts int64
	if err := rows.Scan(&attempts); err != nil {
		return 0, false, err
	}
	return attempts, true, nil
}

func scanPublication(rows *sql.Rows) (Publication, error) {
	var (
		id                   string
		listenerID           string
		eventType            string
		serializedEvent      string
		publicationDate      string
		completionDate       sql.NullString
		status               string
		completionAttempts   int64
		lastResubmissionDate sql.NullString
	)
	if err := rows.Scan(&id, &listenerID, &eventType, &serializedEvent, &publicationDate,
		&completionDate, &status, &completionAttempts, &lastResubmissionDate); err != nil {
		return Publication{}, fmt.Errorf("scan publication: %w", err)
	}

	parsedID, err := uuid.Parse(id)
	if err != nil {
		return Publication{}, fmt.Errorf("parse publication identifier %s: %w", id, err)
	}
	parsedPublicationDate, err := parseTime(publicationDate)
	if err != nil {
		return Publication{}, fmt.Errorf("parse publication date of %s: %w", id, err)
	}
	parsedCompletionDate, err := parseNullableTime(completionDate)
	if err != nil {
		return Publication{}, fmt.Errorf("parse completion date of %s: %w", id, err)
	}
	parsedResubmissionDate, err := parseNullableTime(lastResubmissionDate)
	if err != nil {
		return Publication{}, fmt.Errorf("parse resubmission date of %s: %w", id, err)
	}

	return Publication{
		ID:                   parsedID,
		ListenerID:           eventbus.ListenerID(listenerID),
		EventType:            eventType,
		SerializedEvent:      serializedEvent,
		PublicationDate:      parsedPublicationDate,
		CompletionDate:       parsedCompletionDate,
		Status:               Status(status),
		CompletionAttempts:   int(completionAttempts),
		LastResubmissionDate: parsedResubmissionDate,
	}, nil
}

func formatTime(value time.Time) string {
	return value.UTC().Format(time.RFC3339Nano)
}

func parseTime(value string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, value)
}

func nullableTime(value *time.Time) sql.NullString {
	if value == nil {
		return sql.NullString{}
	}
	return sql.NullString{String: formatTime(*value), Valid: true}
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
func NewSQLiteStore(
	database *sql.DB, completionMode CompletionMode, options ...Option,
) *SQLiteStore {
	return &SQLiteStore{newSQLStore(database, completionMode, sqliteDialect(), options...)}
}

// PostgresStore is a Store backed by a PostgreSQL database.
type PostgresStore struct {
	*sqlStore
}

var _ Store = (*PostgresStore)(nil)

// NewPostgresStore constructs a PostgresStore on top of an open PostgreSQL
// database. The schema and queries are the same as the SQLite store; only the
// migration dialect and the placeholder style differ.
func NewPostgresStore(
	database *sql.DB, completionMode CompletionMode, options ...Option,
) *PostgresStore {
	return &PostgresStore{newSQLStore(database, completionMode, postgresDialect(), options...)}
}
