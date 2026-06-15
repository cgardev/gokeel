package outbox

import (
	"embed"
	"fmt"
	"io/fs"
)

//go:embed migration/*.sql
var migrationScripts embed.FS

// SchemaHistoryTable is the name of the schema-history table that engine-backed
// Migrator adapters use to record applied migrations. It is exported so an
// adapter (such as the goway adapter) writes to the same table the outbox has
// always used, preserving the on-disk migration-history contract for databases
// that were previously migrated by goway.
const SchemaHistoryTable = "event_publication_schema_history"

// Schema returns the embedded outbox migration scripts as a read-only file
// system rooted at the migration directory, so its entries are
// "V1__create_event_publication_tables.sql" and any successors, not
// "migration/V1__...". Both the native Migrator and any external adapter read
// these exact scripts, so the schema has a single source.
//
// The returned value is an immutable view over the package embedded files;
// callers cannot mutate them.
func Schema() fs.FS {
	sub, err := fs.Sub(migrationScripts, "migration")
	if err != nil {
		// Unreachable: the migration directory is embedded at build time.
		panic(fmt.Sprintf("outbox: embed migration sub-filesystem: %v", err))
	}
	return sub
}
