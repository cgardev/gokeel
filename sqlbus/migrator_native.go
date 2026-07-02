package sqlbus

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

// nativeAppliedTable records which scripts the native migrator has already
// applied, so scripts whose DDL is not re-runnable (such as ALTER TABLE ADD
// COLUMN) stay idempotent without a full migration engine. It is distinct
// from SchemaHistoryTable, which belongs to the goway adapter: a database
// must stay with the migrator that initialized it.
const nativeAppliedTable = "event_message_native_schema"

// NativeMigrator applies the sqlbus schema using only database/sql. It is the
// default Migrator wired by the store constructors, so the common case pulls
// in no third-party migration engine.
//
// It executes each embedded *.sql script in ascending file-name order,
// recording the applied script names so a later run skips them. A database
// migrated before the record table existed re-runs the historical scripts,
// whose IF NOT EXISTS DDL is a no-op, and an ALTER TABLE ADD COLUMN that
// finds its column already present is tolerated, so every upgrade path
// converges. The dialect is accepted for interface symmetry but is not needed
// here, because the DDL is portable across SQLite and PostgreSQL.
type NativeMigrator struct{}

var _ Migrator = NativeMigrator{}

// Migrate reads every *.sql script from schema in name order, splits each into
// individual statements, and executes the not-yet-applied ones. It is
// idempotent.
func (NativeMigrator) Migrate(ctx context.Context, db *sql.DB, _ Dialect, schema fs.FS) error {
	if _, err := db.ExecContext(ctx,
		"CREATE TABLE IF NOT EXISTS "+nativeAppliedTable+" (script TEXT NOT NULL PRIMARY KEY)"); err != nil {
		return fmt.Errorf("create native schema record: %w", err)
	}

	entries, err := fs.ReadDir(schema, ".")
	if err != nil {
		return fmt.Errorf("read embedded schema: %w", err)
	}

	names := make([]string, 0, len(entries))
	for _, entry := range entries {
		if !entry.IsDir() && strings.HasSuffix(entry.Name(), ".sql") {
			names = append(names, entry.Name())
		}
	}
	sort.Strings(names)

	for _, name := range names {
		applied, err := scriptApplied(ctx, db, name)
		if err != nil {
			return err
		}
		if applied {
			continue
		}
		content, err := fs.ReadFile(schema, name)
		if err != nil {
			return fmt.Errorf("read schema script %s: %w", name, err)
		}
		for _, statement := range splitSQLStatements(string(content)) {
			if _, err := db.ExecContext(ctx, statement); err != nil {
				if isDuplicateColumn(err) {
					// The column was added by an earlier run that predates the
					// record table, or by another migrator. The schema already
					// holds the outcome the statement produces, so the script
					// counts as applied.
					continue
				}
				return fmt.Errorf("apply schema script %s: %w", name, err)
			}
		}
		if _, err := db.ExecContext(ctx,
			"INSERT INTO "+nativeAppliedTable+" (script) VALUES ($1)"+insertConflictClause,
			name); err != nil {
			return fmt.Errorf("record schema script %s: %w", name, err)
		}
	}
	return nil
}

// insertConflictClause tolerates concurrent initializations racing to record
// the same script; the clause is portable across SQLite and PostgreSQL.
const insertConflictClause = " ON CONFLICT DO NOTHING"

// scriptApplied reports whether the record table already names the script.
func scriptApplied(ctx context.Context, db *sql.DB, name string) (bool, error) {
	row := db.QueryRowContext(ctx,
		"SELECT COUNT(*) FROM "+nativeAppliedTable+" WHERE script = $1", name)
	var count int64
	if err := row.Scan(&count); err != nil {
		return false, fmt.Errorf("read native schema record of %s: %w", name, err)
	}
	return count > 0, nil
}

// isDuplicateColumn recognizes the add-column-already-exists failure of both
// supported databases. Neither driver exposes a portable sentinel for it, so
// the message is the only seam; the match is deliberately narrow.
func isDuplicateColumn(err error) bool {
	message := err.Error()
	return strings.Contains(message, "duplicate column name") || // SQLite
		strings.Contains(message, "SQLSTATE 42701") || // PostgreSQL via pgx
		strings.Contains(message, "already exists") // PostgreSQL textual form
}

// splitSQLStatements divides a script into individual statements on the
// semicolon boundary, discarding blank statements. The sqlbus migration
// scripts contain only plain DDL with no semicolons inside string literals or
// bodies, so a straight split is correct here. This is intentionally simple:
// the native path owns a fixed, package-controlled set of scripts, not
// arbitrary user SQL.
func splitSQLStatements(script string) []string {
	parts := strings.Split(script, ";")
	statements := make([]string, 0, len(parts))
	for _, part := range parts {
		if trimmed := strings.TrimSpace(part); trimmed != "" {
			statements = append(statements, trimmed)
		}
	}
	return statements
}
