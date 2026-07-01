package sqlbus

import (
	"context"
	"database/sql"
	"io/fs"
)

// Dialect identifies the target database without leaking any third-party type
// into the sqlbus core. It is a closed string enum: the core only ever
// produces DialectSQLite and DialectPostgres, set by the respective store
// constructors. A Migrator adapter switches on it to choose its own dialect
// representation.
type Dialect string

const (
	// DialectSQLite identifies a SQLite database.
	DialectSQLite Dialect = "sqlite"
	// DialectPostgres identifies a PostgreSQL database.
	DialectPostgres Dialect = "postgres"
)

// Migrator brings the sqlbus schema up to date against an open database. It
// is the single seam the store uses for schema initialization, so the choice
// of migration engine stays out of the query code. The store calls Migrate
// once, from Store.Initialize.
//
// Implementations receive the open *sql.DB (a concrete handle, because some
// engines require one rather than the narrower Querier), the target Dialect,
// and the embedded schema scripts as an fs.FS rooted at the directory that
// holds the SQL files, so an adapter never duplicates the SQL.
//
// Implementations must be idempotent: Initialize may run on every start, so
// calling Migrate against an already-migrated database must be a no-op.
//
// The core ships a native database/sql implementation (NativeMigrator) as the
// zero-configuration default, so the common case needs no Migrator and the
// core go.mod carries no migration-engine dependency. A goway-backed
// implementation lives in its own module
// (github.com/cgardev/gokeel/sqlbus/gowaymigrator) so that only clients who
// opt in pull goway into their build.
type Migrator interface {
	// Migrate applies schema to db for the given dialect.
	Migrate(ctx context.Context, db *sql.DB, dialect Dialect, schema fs.FS) error
}

// MigratorFunc adapts an ordinary function to the Migrator interface, so a
// caller can supply a one-off migration strategy without declaring a type.
type MigratorFunc func(ctx context.Context, db *sql.DB, dialect Dialect, schema fs.FS) error

// Migrate calls f.
func (f MigratorFunc) Migrate(ctx context.Context, db *sql.DB, dialect Dialect, schema fs.FS) error {
	return f(ctx, db, dialect, schema)
}
