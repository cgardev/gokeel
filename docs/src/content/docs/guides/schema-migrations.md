---
title: Schema Migrations
description: How the outbox brings its schema up to date through a pluggable Migrator, the zero-dependency NativeMigrator default, and the optional goway-backed adapter.
---

The outbox needs two tables before it can persist a publication. It creates
them through a single seam, the `Migrator` interface, so the choice of
migration engine stays out of the query code. The default implementation uses
only `database/sql`, which is what keeps the outbox core free of any
migration-engine dependency.

## The Migrator interface

A `Migrator` brings the outbox schema up to date against an open database. The
store calls it exactly once, from `Store.Initialize`:

```go
type Migrator interface {
	// Migrate applies schema to db for the given dialect.
	Migrate(ctx context.Context, db *sql.DB, dialect outbox.Dialect, schema fs.FS) error
}
```

Each argument is supplied by the store, so an implementation never has to know
where the schema lives:

| Argument | Meaning |
| --- | --- |
| `db` | The open `*sql.DB`, a concrete handle, because some engines require one. |
| `dialect` | The target database, either `outbox.DialectSQLite` or `outbox.DialectPostgres`. |
| `schema` | The embedded migration scripts, rooted at the directory that holds the SQL files. |

Implementations must be idempotent. `Initialize` may run on every start, so
calling `Migrate` against an already-migrated database must be a no-op.

## The native default

The core ships `NativeMigrator`, a `database/sql`-only implementation that is
the zero-configuration default. Both store constructors wire it automatically,
so the common case needs no `Migrator` and the core `go.mod` carries no
migration-engine dependency:

```go
store := outbox.NewPostgresStore(db, outbox.CompletionModeUpdate)

if err := store.Initialize(ctx); err != nil {
	return err
}
```

`NativeMigrator` executes each embedded `*.sql` script in ascending file-name
order, splitting each into individual statements on the semicolon boundary. The
scripts use `CREATE TABLE` / `CREATE INDEX IF NOT EXISTS`, so re-running them is
a no-op and the native path keeps no schema-history table of its own. The same
DDL is portable across SQLite and PostgreSQL, which is why the dialect is
accepted for interface symmetry but never inspected here.

You can also name it explicitly, though the result is identical to the default:

```go
store := outbox.NewSQLiteStore(db, outbox.CompletionModeUpdate,
	outbox.WithMigrator(outbox.NativeMigrator{}))
```

## The embedded schema

The exact scripts both the native migrator and any adapter apply are exposed by
`outbox.Schema`, a read-only file system rooted at the migration directory:

```go
schema := outbox.Schema()
// Entries are "V1__create_event_publication_tables.sql" and any successors,
// not "migration/V1__...".
```

The first script creates the publication and archive tables together with their
indexes:

```sql
CREATE TABLE IF NOT EXISTS event_publication
(
    id                     TEXT    NOT NULL PRIMARY KEY,
    listener_id            TEXT    NOT NULL,
    event_type             TEXT    NOT NULL,
    serialized_event       TEXT    NOT NULL,
    publication_date       TEXT    NOT NULL,
    completion_date        TEXT,
    status                 TEXT    NOT NULL,
    completion_attempts    INTEGER NOT NULL DEFAULT 0,
    last_resubmission_date TEXT
);
-- ... indexes and the event_publication_archive table
```

Because every migrator reads these exact scripts, the schema has a single
source: an adapter never duplicates the SQL.

## Opting into goway

An engine-backed `Migrator` records applied migrations in a schema-history
table, the Flyway-style contract some teams already rely on. The outbox provides
a goway-backed adapter for this, in its own module so that only clients who opt
in pull goway into their build:

```sh
go get github.com/cgardev/gokeel/outbox/gowaymigrator
```

Pass the adapter to `outbox.WithMigrator`:

```go
import (
	"github.com/cgardev/gokeel/outbox"
	"github.com/cgardev/gokeel/outbox/gowaymigrator"
)

store := outbox.NewPostgresStore(db, outbox.CompletionModeUpdate,
	outbox.WithMigrator(gowaymigrator.New()))
```

The adapter maps the core dialect onto goway's own dialect and runs the embedded
schema as a versioned migration, recording history in the
`outbox.SchemaHistoryTable` table:

```go
const SchemaHistoryTable = "event_publication_schema_history"
```

Writing to that exact table preserves the on-disk migration-history contract for
databases that were previously migrated by goway. Importing the
`gowaymigrator` package is the single action that pulls goway into a build; the
outbox core itself continues to depend only on `database/sql`.

## A custom Migrator

Any function with the right signature is a `Migrator` through `MigratorFunc`, so
a one-off strategy needs no new type. The following adapter logs before
delegating to the native implementation:

```go
logging := outbox.MigratorFunc(func(
	ctx context.Context, db *sql.DB, dialect outbox.Dialect, schema fs.FS,
) error {
	slog.Info("applying outbox schema", "dialect", dialect)
	return outbox.NativeMigrator{}.Migrate(ctx, db, dialect, schema)
})

store := outbox.NewSQLiteStore(db, outbox.CompletionModeUpdate,
	outbox.WithMigrator(logging))
```

A custom `Migrator` can also ignore the supplied schema entirely and apply the
tables through a migration system you already run, as long as the resulting
`event_publication` and `event_publication_archive` tables match the columns the
store reads. Keep the implementation idempotent, since `Initialize` may run on
every start.

## Where the seam pays off

The native default means a fresh project gets a working outbox with no extra
modules to install, while `WithMigrator` lets a team fold the outbox schema into
the migration engine it already operates. Both paths read the same
`outbox.Schema`, so the SQL never diverges. For the full surface, see the
[Schema Migrator reference](/gokeel/reference/migrator/); to see the store these
migrations support, see [The Transactional Outbox](/gokeel/guides/transactional-outbox/).
