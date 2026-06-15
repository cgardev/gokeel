---
title: Schema Migrations
description: Recipes for the native default migrator, the goway adapter, and a custom Migrator over the embedded outbox schema.
---

## Run with the native default migrator

You want the outbox tables created with no migration engine and no extra
dependency.

```go
store := outbox.NewSQLiteStore(database, outbox.CompletionModeUpdate)

if err := store.Initialize(ctx); err != nil {
    return fmt.Errorf("initialize outbox: %w", err)
}
```

`Initialize` applies the embedded schema through the configured `Migrator`. With
no option supplied the store uses `outbox.NativeMigrator{}`, which executes each
embedded `*.sql` script with `database/sql` only:

```sql
-- CREATE TABLE IF NOT EXISTS event_publication ( ... )
-- CREATE TABLE IF NOT EXISTS event_publication_archive ( ... )
```

Gotcha: the native scripts use `CREATE TABLE`/`CREATE INDEX IF NOT EXISTS` and
keep no history table, so calling `Initialize` on every start is a safe no-op;
the core `go.mod` carries no migration-engine dependency as a result.

## Opt into the goway adapter

You want Flyway-style versioned migrations recorded in a schema-history table.

```sh
go get github.com/cgardev/gokeel/outbox/gowaymigrator
```

```go
import (
    "github.com/cgardev/gokeel/outbox"
    "github.com/cgardev/gokeel/outbox/gowaymigrator"
)

store := outbox.NewPostgresStore(database, outbox.CompletionModeUpdate,
    outbox.WithMigrator(gowaymigrator.New()))

if err := store.Initialize(ctx); err != nil {
    return fmt.Errorf("initialize outbox: %w", err)
}
```

`gowaymigrator.New()` returns an `outbox.Migrator` backed by goway. It applies
the same outbox-owned embedded schema and records applied versions in the
`outbox.SchemaHistoryTable` table (`event_publication_schema_history`).

Gotcha: importing `outbox/gowaymigrator` is the single action that pulls goway
into your build, so only clients who pass `WithMigrator(gowaymigrator.New())`
take on that dependency.

## Write a custom Migrator

You want to wire your own migration step while still applying the canonical
outbox schema.

```go
custom := outbox.MigratorFunc(func(
    ctx context.Context, db *sql.DB, dialect outbox.Dialect, schema fs.FS,
) error {
    // schema is outbox.Schema(): an fs.FS rooted at the migration directory,
    // so its entries are "V1__create_event_publication_tables.sql" and any
    // successors, never "migration/V1__...".
    return myEngine.ApplyEmbedded(ctx, db, schema)
})

store := outbox.NewSQLiteStore(database, outbox.CompletionModeUpdate,
    outbox.WithMigrator(custom))
```

`MigratorFunc` adapts a plain function to the `Migrator` interface, so no named
type is needed. The store passes the open `*sql.DB`, the target `outbox.Dialect`
(`DialectSQLite` or `DialectPostgres`), and `outbox.Schema()` as the `fs.FS`, so
your adapter never duplicates the SQL.

Gotcha: a `Migrator` must be idempotent, because `Initialize` may run on every
start; reuse `outbox.Schema()` rather than copying the DDL so the schema keeps a
single source.

See [Getting Started](/gokeel/getting-started/) for opening a database and
constructing a store.
