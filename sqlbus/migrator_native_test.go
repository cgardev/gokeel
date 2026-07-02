package sqlbus

import (
	"testing"
)

// legacyListenerTable is the pre-V2 shape of the listener table, as an
// already-initialized database created before the ordering column existed
// would hold it, with no native schema record table.
const legacyListenerTable = `
CREATE TABLE event_message_listener
(
    listener_id   TEXT NOT NULL PRIMARY KEY,
    delivery_mode TEXT NOT NULL
);`

func TestNativeMigratorUpgradesADatabaseCreatedBeforeTheOrderingColumn(t *testing.T) {
	database := openSQLiteDatabase(t, newSQLitePath(t))
	if _, err := database.ExecContext(t.Context(), legacyListenerTable); err != nil {
		t.Fatalf("create legacy table: %v", err)
	}

	store := NewSQLiteStore(database)
	if err := store.Initialize(t.Context()); err != nil {
		t.Fatalf("initialize over the legacy schema: %v", err)
	}

	// The ordering column must exist and carry the default for legacy rows.
	if _, err := database.ExecContext(t.Context(),
		"INSERT INTO event_message_listener (listener_id, delivery_mode) VALUES ('legacy', 'COMPETING')"); err != nil {
		t.Fatalf("insert into upgraded table: %v", err)
	}
	row := database.QueryRowContext(t.Context(),
		"SELECT ordering FROM event_message_listener WHERE listener_id = 'legacy'")
	var ordering string
	if err := row.Scan(&ordering); err != nil {
		t.Fatalf("read ordering column: %v", err)
	}
	if Ordering(ordering) != OrderingUnordered {
		t.Fatalf("ordering default = %s, want %s", ordering, OrderingUnordered)
	}
}

func TestNativeMigratorIsIdempotentAcrossRepeatedInitializations(t *testing.T) {
	database := openSQLiteDatabase(t, newSQLitePath(t))
	store := NewSQLiteStore(database)
	for round := 0; round < 3; round++ {
		if err := store.Initialize(t.Context()); err != nil {
			t.Fatalf("initialization round %d: %v", round, err)
		}
	}
}
