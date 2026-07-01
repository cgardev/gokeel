package gowaymigrator_test

import (
	"database/sql"
	"path/filepath"
	"testing"

	"github.com/cgardev/gokeel/sqlbus"
	"github.com/cgardev/gokeel/sqlbus/gowaymigrator"
	_ "modernc.org/sqlite"
)

// TestMigrateCreatesSchemaOnSQLite checks that the goway-backed Migrator
// applies the sqlbus-owned embedded schema, records its history table, and is
// idempotent.
func TestMigrateCreatesSchemaOnSQLite(t *testing.T) {
	path := filepath.Join(t.TempDir(), "sqlbus.db")
	dataSourceName := "file:" + path +
		"?_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	database, err := sql.Open("sqlite", dataSourceName)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Logf("close database: %v", err)
		}
	})

	migrator := gowaymigrator.New()

	// First run creates the schema; a second run must be a no-op (goway
	// records the applied migration in its history table).
	for run := 1; run <= 2; run++ {
		if err := migrator.Migrate(t.Context(), database, sqlbus.DialectSQLite, sqlbus.Schema()); err != nil {
			t.Fatalf("migrate (run %d): %v", run, err)
		}
	}

	for _, table := range []string{
		"event_message",
		"event_message_listener",
		"event_message_consumer",
		"event_message_delivery",
		sqlbus.SchemaHistoryTable,
	} {
		var name string
		err := database.QueryRow(
			`SELECT name FROM sqlite_master WHERE type = 'table' AND name = ?`, table,
		).Scan(&name)
		if err != nil {
			t.Fatalf("expected table %q to exist: %v", table, err)
		}
	}
}

// TestUnsupportedDialectIsRejected checks that an unknown dialect is reported
// rather than silently ignored.
func TestUnsupportedDialectIsRejected(t *testing.T) {
	database, err := sql.Open("sqlite", "file:"+filepath.Join(t.TempDir(), "x.db"))
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Logf("close database: %v", err)
		}
	})

	err = gowaymigrator.New().Migrate(t.Context(), database, sqlbus.Dialect("oracle"), sqlbus.Schema())
	if err == nil {
		t.Fatal("expected an error for an unsupported dialect, got nil")
	}
}
