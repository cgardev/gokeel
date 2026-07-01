package sqlbus

import (
	"embed"
	"fmt"
	"io/fs"
)

//go:embed migration/*.sql
var migrationScripts embed.FS

// SchemaHistoryTable is the name of the schema-history table that
// engine-backed Migrator adapters use to record applied migrations. It is
// exported so an adapter (such as the goway adapter) writes to the same table
// across upgrades, preserving the on-disk migration-history contract for
// databases that were previously migrated by that engine.
const SchemaHistoryTable = "event_message_schema_history"

// Schema returns the embedded sqlbus migration scripts as a read-only file
// system rooted at the migration directory, so its entries are
// "V1__create_event_message_tables.sql" and any successors, not
// "migration/V1__...". Both the native Migrator and any external adapter read
// these exact scripts, so the schema has a single source.
//
// The returned value is an immutable view over the package embedded files;
// callers cannot mutate them.
func Schema() fs.FS {
	sub, err := fs.Sub(migrationScripts, "migration")
	if err != nil {
		// Unreachable: the migration directory is embedded at build time.
		panic(fmt.Sprintf("sqlbus: embed migration sub-filesystem: %v", err))
	}
	return sub
}
