package sqlbus

import (
	"context"
	"database/sql"
	"fmt"
	"io/fs"
	"sort"
	"strings"
)

// NativeMigrator applies the sqlbus schema using only database/sql. It is the
// default Migrator wired by the store constructors, so the common case pulls
// in no third-party migration engine.
//
// It executes each embedded *.sql script in ascending file-name order. The
// scripts use CREATE TABLE / CREATE INDEX IF NOT EXISTS, so re-running them is
// a no-op; the native path therefore keeps no schema-history table of its own.
// The dialect is accepted for interface symmetry but is not needed here,
// because the IF NOT EXISTS DDL is portable across SQLite and PostgreSQL.
type NativeMigrator struct{}

var _ Migrator = NativeMigrator{}

// Migrate reads every *.sql script from schema in name order, splits each into
// individual statements, and executes them. It is idempotent.
func (NativeMigrator) Migrate(ctx context.Context, db *sql.DB, _ Dialect, schema fs.FS) error {
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
		content, err := fs.ReadFile(schema, name)
		if err != nil {
			return fmt.Errorf("read schema script %s: %w", name, err)
		}
		for _, statement := range splitSQLStatements(string(content)) {
			if _, err := db.ExecContext(ctx, statement); err != nil {
				return fmt.Errorf("apply schema script %s: %w", name, err)
			}
		}
	}
	return nil
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
