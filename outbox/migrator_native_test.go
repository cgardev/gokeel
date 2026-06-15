package outbox

import (
	"database/sql"
	"path/filepath"
	"testing"

	_ "modernc.org/sqlite"
)

// TestNativeMigratorIsIdempotent pins the contract the default Migrator relies
// on: Initialize may run on every start, so applying the embedded schema twice
// must succeed and leave the tables and indexes in place.
func TestNativeMigratorIsIdempotent(t *testing.T) {
	path := filepath.Join(t.TempDir(), "native.db")
	dataSourceName := "file:" + path +
		"?_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	database, err := sql.Open("sqlite", dataSourceName)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	for run := 1; run <= 2; run++ {
		if err := (NativeMigrator{}).Migrate(t.Context(), database, DialectSQLite, Schema()); err != nil {
			t.Fatalf("migrate (run %d): %v", run, err)
		}
	}

	for _, table := range []string{"event_publication", "event_publication_archive"} {
		var name string
		if err := database.QueryRow(
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table,
		).Scan(&name); err != nil {
			t.Fatalf("expected table %q to exist: %v", table, err)
		}
	}

	for _, index := range []string{
		"event_publication_by_status_idx",
		"event_publication_by_publication_date_idx",
	} {
		var name string
		if err := database.QueryRow(
			`SELECT name FROM sqlite_master WHERE type = 'index' AND name = ?`, index,
		).Scan(&name); err != nil {
			t.Fatalf("expected index %q to exist: %v", index, err)
		}
	}
}
