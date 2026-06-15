---
title: Schema Migrator
description: Bringing the outbox schema up to date with the native Migrator or the optional goway adapter.
---

The outbox stores write their publications to a small, fixed schema, and a
`Migrator` is the single seam each store uses to bring that schema up to date.
The store calls it once, from `Store.Initialize`, so the choice of migration
engine stays out of the query code. The core ships a native `database/sql`
implementation as the zero-configuration default, so the common case needs no
`Migrator` and the core `go.mod` carries no migration-engine dependency.

```go
import "github.com/cgardev/gokeel/outbox"
```

## The Migrator interface

A `Migrator` applies the embedded schema to an open database for a given
dialect.

```go
type Migrator interface {
    Migrate(ctx context.Context, db *sql.DB, dialect Dialect, schema fs.FS) error
}
```

Implementations receive the open `*sql.DB` (a concrete handle, because some
engines require one rather than a narrower interface), the target `Dialect`,
and the embedded schema scripts as an `fs.FS` rooted at the directory that
holds the SQL files, so an adapter never duplicates the SQL. Implementations
must be idempotent: `Initialize` may run on every start, so calling `Migrate`
against an already-migrated database must be a no-op.

## MigratorFunc

`MigratorFunc` adapts an ordinary function to the `Migrator` interface, so a
caller can supply a one-off migration strategy without declaring a type.

```go
type MigratorFunc func(ctx context.Context, db *sql.DB, dialect Dialect, schema fs.FS) error
```

```go
var logging outbox.Migrator = outbox.MigratorFunc(
    func(ctx context.Context, db *sql.DB, dialect outbox.Dialect, schema fs.FS) error {
        log.Printf("migrating outbox schema for %s", dialect)
        return outbox.NativeMigrator{}.Migrate(ctx, db, dialect, schema)
    },
)
```

## Dialect

`Dialect` identifies the target database without leaking any third-party type
into the outbox core. It is a closed string enum: the core only ever produces
the two constants below, each set by the matching store constructor.

```go
type Dialect string

const (
    DialectSQLite   Dialect = "sqlite"
    DialectPostgres Dialect = "postgres"
)
```

A `Migrator` adapter switches on the `Dialect` to choose its own dialect
representation. The native path ignores it, because its DDL is portable.

## NativeMigrator

`NativeMigrator` applies the outbox schema using only `database/sql`. It is the
default `Migrator` wired by the store constructors, so the common case pulls in
no third-party migration engine.

```go
type NativeMigrator struct{}

func (NativeMigrator) Migrate(ctx context.Context, db *sql.DB, _ Dialect, schema fs.FS) error
```

It executes each embedded `*.sql` script in ascending file-name order,
splitting every script into individual statements on the semicolon boundary.
The scripts use `CREATE TABLE` / `CREATE INDEX IF NOT EXISTS`, so re-running
them is a no-op; the native path therefore keeps no schema-history table of its
own. The dialect is accepted for interface symmetry but is not needed, because
the `IF NOT EXISTS` DDL is portable across SQLite and PostgreSQL.

Because it is the default, a store created without any option already uses it:

```go
store := outbox.NewSQLiteStore(database, outbox.CompletionModeUpdate)

// Initialize applies the schema through NativeMigrator.
if err := store.Initialize(ctx); err != nil {
    return err
}
```

## Schema and SchemaHistoryTable

`Schema` returns the embedded outbox migration scripts as a read-only file
system rooted at the migration directory, so its entries are
`V1__create_event_publication_tables.sql` and any successors, not
`migration/V1__...`. Both the native `Migrator` and any external adapter read
these exact scripts, so the schema has a single source. The returned value is
an immutable view over the package embedded files; callers cannot mutate them.

```go
func Schema() fs.FS
```

`SchemaHistoryTable` is the name of the schema-history table that engine-backed
adapters use to record applied migrations. It is exported so an adapter writes
to the same table the outbox has always used, preserving the on-disk
migration-history contract for databases that were previously migrated by an
engine.

```go
const SchemaHistoryTable = "event_publication_schema_history"
```

The store supplies `Schema()` to the `Migrator` itself; application code rarely
calls either symbol directly, but both are exported for adapters that drive the
schema through their own engine.

## Overriding the migrator

`WithMigrator` is the construction-time option that replaces the default
`NativeMigrator`. It is passed to either store constructor, and a `nil`
migrator is ignored, so the default stays in place.

```go
func WithMigrator(m Migrator) Option
```

```go
store := outbox.NewPostgresStore(
    database,
    outbox.CompletionModeUpdate,
    outbox.WithMigrator(myMigrator),
)
```

## The goway adapter

When a project already manages its schema with a Flyway-style versioned
migration engine, the optional adapter at
`github.com/cgardev/gokeel/outbox/gowaymigrator` provides a goway-backed
`Migrator`. Importing this package is the single action that pulls goway into a
build; the outbox core itself depends only on `database/sql`.

```go
import "github.com/cgardev/gokeel/outbox/gowaymigrator"
```

`New` returns the adapter. It applies the outbox-owned embedded schema as a
versioned migration, recording history in the `SchemaHistoryTable`. Pass the
result to `WithMigrator`:

```go
func New() outbox.Migrator
```

```go
store := outbox.NewPostgresStore(
    database,
    outbox.CompletionModeUpdate,
    outbox.WithMigrator(gowaymigrator.New()),
)

if err := store.Initialize(ctx); err != nil {
    return err
}
```

The adapter maps the core `Dialect` onto goway's own dialect type, runs the
schema supplied by the store, and never lets a goway type cross into the outbox
core or an outbox type cross into goway.

See [Getting Started](/gokeel/getting-started/) for the full setup, and the
[Transaction Manager](/gokeel/reference/transaction-manager/) reference for the
unit of work the stores run within.
