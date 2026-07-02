package outbox

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

// timeLayout renders timestamps in UTC with a fixed-width, nine-digit
// fraction, so the lexicographic order of the stored TEXT values equals their
// chronological order. The incomplete-before filter compares dates inside
// SQL, which would misorder the variable-width RFC3339Nano rendering.
const timeLayout = "2006-01-02T15:04:05.000000000Z07:00"

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

// executeSettling executes the statement on the store connection and reports
// whether it affected at least one row, the shape every guarded transition
// shares.
func (s *sqlStore) executeSettling(
	ctx context.Context, querier Querier, statement renderable,
) (bool, error) {
	result, err := s.execute(ctx, querier, statement)
	if err != nil {
		return false, err
	}
	affected, err := result.RowsAffected()
	if err != nil {
		return false, err
	}
	return affected > 0, nil
}

// fetch renders the query for the active dialect and returns its rows.
func (s *sqlStore) fetch(
	ctx context.Context, querier Querier, statement renderable,
) (*sql.Rows, error) {
	query, args, err := statement.SQLFor(s.dialect.gooq)
	if err != nil {
		return nil, err
	}
	return querier.QueryContext(ctx, query, args...)
}

// Initialize brings the database schema up to date by applying the embedded
// migration scripts through the configured Migrator. The default native
// Migrator uses database/sql only; a goway-backed Migrator can be supplied
// with WithMigrator.
func (s *sqlStore) Initialize(ctx context.Context) error {
	if err := s.migrator.Migrate(ctx, s.database, s.dialect.kind, Schema()); err != nil {
		return fmt.Errorf("apply outbox schema: %w", err)
	}
	return nil
}

// Create writes the publication through the provided querier, so it joins the
// transaction of the business change that produced the event.
func (s *sqlStore) Create(ctx context.Context, querier Querier, publication Publication) error {
	insert := gooq.InsertInto(gooqPublication).
		Columns(gooqPublication.ID, gooqPublication.ListenerID, gooqPublication.EventType,
			gooqPublication.SerializedEvent, gooqPublication.PublicationDate,
			gooqPublication.Status, gooqPublication.CompletionAttempts,
			gooqPublication.LastResubmissionDate).
		Values(publication.ID.String(), string(publication.ListenerID), publication.EventType,
			publication.SerializedEvent, formatTime(publication.PublicationDate),
			string(publication.Status), int64(publication.CompletionAttempts),
			nullableTimeValue(publication.LastResubmissionDate))
	if _, err := s.execute(ctx, querier, insert); err != nil {
		return fmt.Errorf("persist publication %s: %w", publication.ID, err)
	}
	return nil
}

// generationCondition matches one publication at the attempt generation the
// caller holds, in the given status: the guard every dispatcher transition
// carries, so a dispatcher that lost its publication to a resubmission
// affects zero rows instead of overwriting the outcome of the current holder.
func generationCondition(id uuid.UUID, status Status, attempts int) gooq.Condition {
	return gooqPublication.ID.EQ(id.String()).
		And(gooqPublication.Status.EQ(string(status))).
		And(gooqPublication.CompletionAttempts.EQ(int64(attempts)))
}

// ClaimProcessing atomically transitions a published or resubmitted
// publication of the given attempt generation into processing, stamping the
// attempt start so the resubmitter grace protects the claimed delivery. The
// status and generation guards make the update succeed for exactly one of
// several concurrent dispatchers; the losers observe zero affected rows. A
// failed publication is not claimable: it re-enters delivery only through
// MarkResubmitted.
func (s *sqlStore) ClaimProcessing(ctx context.Context, id uuid.UUID, attempts int) (bool, error) {
	update := gooq.Update(gooqPublication).
		Set(gooqPublication.Status.Set(string(StatusProcessing))).
		Set(gooqPublication.LastResubmissionDate.Set(formatTime(time.Now().UTC()))).
		Where(gooqPublication.ID.EQ(id.String()).
			And(gooqPublication.Status.In(string(StatusPublished), string(StatusResubmitted))).
			And(gooqPublication.CompletionAttempts.EQ(int64(attempts))))
	claimed, err := s.executeSettling(ctx, s.database, update)
	if err != nil {
		return false, fmt.Errorf("claim publication %s: %w", id, err)
	}
	return claimed, nil
}

// MarkFailed records that processing the publication of the given attempt
// generation failed, leaving it for a later resubmission. It reports false
// when the publication was fenced out by a concurrent resubmission or
// settlement.
func (s *sqlStore) MarkFailed(ctx context.Context, id uuid.UUID, attempts int) (bool, error) {
	update := gooq.Update(gooqPublication).
		Set(gooqPublication.Status.Set(string(StatusFailed))).
		Where(generationCondition(id, StatusProcessing, attempts))
	settled, err := s.executeSettling(ctx, s.database, update)
	if err != nil {
		return false, fmt.Errorf("mark publication %s as failed: %w", id, err)
	}
	return settled, nil
}

// MarkCompleted settles the processing publication of the given attempt
// generation according to the completion mode of the store. It reports false
// when the publication was fenced out by a concurrent resubmission.
func (s *sqlStore) MarkCompleted(
	ctx context.Context, id uuid.UUID, attempts int, completionDate time.Time,
) (bool, error) {
	switch s.completionMode {
	case CompletionModeDelete:
		statement := gooq.DeleteFrom(gooqPublication).
			Where(generationCondition(id, StatusProcessing, attempts))
		settled, err := s.executeSettling(ctx, s.database, statement)
		if err != nil {
			return false, fmt.Errorf("delete completed publication %s: %w", id, err)
		}
		return settled, nil
	case CompletionModeArchive:
		return s.archive(ctx, id, attempts, completionDate)
	default:
		update := gooq.Update(gooqPublication).
			Set(gooqPublication.Status.Set(string(StatusCompleted))).
			Set(gooqPublication.CompletionDate.Set(formatTime(completionDate))).
			Where(generationCondition(id, StatusProcessing, attempts))
		settled, err := s.executeSettling(ctx, s.database, update)
		if err != nil {
			return false, fmt.Errorf("mark publication %s as completed: %w", id, err)
		}
		return settled, nil
	}
}

// archive moves the publication into the archive table inside one
// transaction, so a dispatcher fenced out between the read and the delete
// leaves no early archive copy behind: the rollback discards the copy, the
// source row stays with the current holder, and the eventual real completion
// writes the archive entry under the generation that actually settled the
// publication.
func (s *sqlStore) archive(
	ctx context.Context, id uuid.UUID, attempts int, completionDate time.Time,
) (settled bool, err error) {
	tx, err := s.database.BeginTx(ctx, nil)
	if err != nil {
		return false, fmt.Errorf("archive publication %s: begin transaction: %w", id, err)
	}
	defer func(tx *sql.Tx) {
		if rollbackErr := tx.Rollback(); rollbackErr != nil && !errors.Is(rollbackErr, sql.ErrTxDone) {
			err = errors.Join(err, fmt.Errorf("archive publication %s: rollback: %w", id, rollbackErr))
		}
	}(tx)

	read := selectPublications().From(gooqPublication).
		Where(generationCondition(id, StatusProcessing, attempts)).
		Limit(1)
	publication, found, err := s.fetchOne(ctx, tx, read)
	if err != nil {
		return false, fmt.Errorf("archive publication %s: read: %w", id, err)
	}
	if !found {
		return false, nil
	}

	insert := gooq.InsertInto(gooqArchive).
		Columns(gooqArchive.ID, gooqArchive.ListenerID, gooqArchive.EventType,
			gooqArchive.SerializedEvent, gooqArchive.PublicationDate,
			gooqArchive.CompletionDate, gooqArchive.Status,
			gooqArchive.CompletionAttempts, gooqArchive.LastResubmissionDate).
		Values(publication.ID.String(), string(publication.ListenerID), publication.EventType,
			publication.SerializedEvent, formatTime(publication.PublicationDate),
			formatTime(completionDate), string(StatusCompleted),
			int64(publication.CompletionAttempts),
			nullableTimeValue(publication.LastResubmissionDate)).
		OnConflictDoNothing()
	if _, err := s.execute(ctx, tx, insert); err != nil {
		return false, fmt.Errorf("archive publication %s: write archive entry: %w", id, err)
	}

	remove := gooq.DeleteFrom(gooqPublication).
		Where(generationCondition(id, StatusProcessing, attempts))
	deleted, err := s.executeSettling(ctx, tx, remove)
	if err != nil {
		return false, fmt.Errorf("archive publication %s: delete source row: %w", id, err)
	}
	if !deleted {
		return false, nil
	}

	if err := tx.Commit(); err != nil {
		return false, fmt.Errorf("archive publication %s: commit: %w", id, err)
	}
	return true, nil
}

// MarkResubmitted transitions a published, processing, or failed publication
// of the given attempt generation back into delivery, counting the attempt.
// It reports false when another caller resubmitted or settled the publication
// first: the compare-and-set re-checks the status and the attempt counter the
// caller observed, so the staleness decision made against that observation
// cannot be applied to a row that has moved on in the meantime, and two
// concurrent resubmissions of the same entry can never both succeed.
func (s *sqlStore) MarkResubmitted(
	ctx context.Context,
	id uuid.UUID,
	attempts int,
	resubmissionDate time.Time,
) (int, bool, error) {
	update := gooq.Update(gooqPublication).
		Set(gooqPublication.Status.Set(string(StatusResubmitted))).
		Set(gooqPublication.CompletionAttempts.Set(int64(attempts + 1))).
		Set(gooqPublication.LastResubmissionDate.Set(formatTime(resubmissionDate))).
		Where(gooqPublication.ID.EQ(id.String()).
			And(gooqPublication.Status.In(
				string(StatusPublished), string(StatusProcessing), string(StatusFailed))).
			And(gooqPublication.CompletionAttempts.EQ(int64(attempts))))
	resubmitted, err := s.executeSettling(ctx, s.database, update)
	if err != nil {
		return 0, false, fmt.Errorf("mark publication %s as resubmitted: %w", id, err)
	}
	if !resubmitted {
		return 0, false, nil
	}
	return attempts + 1, true, nil
}

// selectPublications is the shared projection of the publication reads.
func selectPublications() gooq.SelectFromStep[gooq.Record9[
	string, string, string, string, string,
	sql.NullString, string, int64, sql.NullString,
]] {
	return gooq.Select9(
		gooqPublication.ID, gooqPublication.ListenerID, gooqPublication.EventType,
		gooqPublication.SerializedEvent, gooqPublication.PublicationDate,
		gooq.NewField[sql.NullString](gooqPublication.TableImpl, "completion_date"),
		gooqPublication.Status, gooqPublication.CompletionAttempts,
		gooq.NewField[sql.NullString](gooqPublication.TableImpl, "last_resubmission_date"),
	)
}

// FindIncomplete returns every publication not yet completed, in publication
// order.
func (s *sqlStore) FindIncomplete(ctx context.Context) ([]Publication, error) {
	query := selectPublications().From(gooqPublication).
		Where(gooqPublication.Status.NE(string(StatusCompleted))).
		OrderBy(gooqPublication.PublicationDate.Asc())
	return s.collect(ctx, query)
}

// FindIncompletePublishedBefore returns every publication not yet completed
// whose latest delivery attempt started before the reference time, in
// publication order. The age compares against the latest attempt, not the
// original publication, so the grace window of the resubmitter protects
// every in-flight dispatch; COALESCE covers rows created before the last
// resubmission date was seeded at creation time.
func (s *sqlStore) FindIncompletePublishedBefore(
	ctx context.Context,
	reference time.Time,
) ([]Publication, error) {
	latestAttempt := gooq.Coalesce(
		gooqPublication.LastResubmissionDate, gooqPublication.PublicationDate)
	query := selectPublications().From(gooqPublication).
		Where(gooqPublication.Status.NE(string(StatusCompleted)).
			And(latestAttempt.LT(formatTime(reference)))).
		OrderBy(gooqPublication.PublicationDate.Asc())
	return s.collect(ctx, query)
}

func (s *sqlStore) collect(ctx context.Context, query renderable) (publications []Publication, err error) {
	rows, err := s.fetch(ctx, s.database, query)
	if err != nil {
		return nil, fmt.Errorf("find incomplete publications: %w", err)
	}
	defer func(rows *sql.Rows) {
		if closeErr := rows.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("find incomplete publications: close rows: %w", closeErr))
		}
	}(rows)

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

// fetchOne reads at most one publication through the querier, fully consuming
// and closing the rows before returning so a follow-up statement can run on
// the same connection.
func (s *sqlStore) fetchOne(
	ctx context.Context, querier Querier, query renderable,
) (publication Publication, found bool, err error) {
	rows, err := s.fetch(ctx, querier, query)
	if err != nil {
		return Publication{}, false, err
	}
	defer func(rows *sql.Rows) {
		if closeErr := rows.Close(); closeErr != nil {
			err = errors.Join(err, fmt.Errorf("close rows: %w", closeErr))
		}
	}(rows)
	if !rows.Next() {
		return Publication{}, false, rows.Err()
	}
	publication, err = scanPublication(rows)
	if err != nil {
		return Publication{}, false, err
	}
	return publication, true, nil
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
	return value.UTC().Format(timeLayout)
}

func parseTime(value string) (time.Time, error) {
	return time.Parse(time.RFC3339Nano, value)
}

// nullableTimeValue renders an optional timestamp as the bind value of a
// nullable column.
func nullableTimeValue(value *time.Time) sql.NullString {
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
// rendering dialect and the migration dialect differ.
func NewPostgresStore(
	database *sql.DB, completionMode CompletionMode, options ...Option,
) *PostgresStore {
	return &PostgresStore{newSQLStore(database, completionMode, postgresDialect(), options...)}
}
