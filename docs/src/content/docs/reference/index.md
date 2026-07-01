---
title: Reference Overview
description: A systematic reference for the gokeel building blocks, organized by module.
---

This reference documents the full feature set of gokeel, the family of building
blocks for a modular monolith in Go. Each page covers one area of the API in
detail, with real Go examples and the behavior each call drives. For
task-oriented snippets, see the [Cookbook](/gokeel/cookbook/).

## How the reference is organized

- [Transaction Manager](/gokeel/reference/transaction-manager/) — the
  `Manager`, `Run` and the generic `RunResult`, the `Querier` resolved from the
  context, and the `Transactor` interface stores depend on.
- [Propagation and options](/gokeel/reference/propagation-and-options/) — the
  `Propagation` modes (`Required`, `Supports`, `Mandatory`, `Never`, `Nested`),
  savepoints, and the `Option` values: `WithPropagation`, `WithIsolation`,
  `ReadOnly`, `WithTimeout`, `WithName`, and the rollback rules.
- [Synchronizations and listeners](/gokeel/reference/synchronizations/) — the
  before-commit, before-completion, after-commit, and after-completion
  callbacks registered with `RegisterBeforeCommit`, `RegisterBeforeCompletion`,
  `RegisterAfterCommit`, and `RegisterAfterCompletion`, and the
  `ExecutionListener` seam for logging, metrics, and tracing.
- [Event Bus](/gokeel/reference/event-bus/) — the synchronous, in-process
  `Bus`, typed registration with `SubscribeTo`, addressed delivery with
  `Deliver`, multicast `Publish`, and panic isolation.
- [Outbox](/gokeel/reference/outbox/) — the transactional outbox: the `Store`,
  the `SQLiteStore` and `PostgresStore` constructors, the `Registry`,
  `Publisher`, `Resubmitter`, the `JSONSerializer`, and the `Publication`
  lifecycle.
- [Schema Migrator](/gokeel/reference/migrator/) — the `Migrator` seam, the
  zero-dependency `NativeMigrator` default, the exported `Schema` and
  `SchemaHistoryTable`, and the optional goway-backed adapter.
- [Logging](/gokeel/reference/logging/) — the level `Manager`, hierarchical
  name inheritance, the `Configuration` document with `ParseConfiguration` and
  `ParseLevels`, runtime `SetLevel` and `ResetLevel`, and the classic `log`
  bridge.
- [Configuration](/gokeel/reference/conf/) — the `Loader` over layered JSON
  sources, the `${NAME:default}` placeholder grammar, relaxed struct binding,
  and `GenerateSchema` for editor completion.

## The example setup

Every example shares one setup: an open `*sql.DB` named `database`, a
`transaction.Manager` named `manager` built over it, a `context.Context` named
`ctx`, and an `eventbus.Bus` named `bus` for the event-driven pages.

```go
import (
	"context"
	"database/sql"

	"github.com/cgardev/gokeel/eventbus"
	"github.com/cgardev/gokeel/transaction"
	_ "modernc.org/sqlite" // your own driver, blank-imported
)

database, _ := sql.Open("sqlite", ":memory:")
manager := transaction.NewManager(database)

bus := eventbus.NewBus()
ctx := context.Background()
```

Inside a unit of work, stores resolve the executor they run against from the
context rather than receiving a `*sql.Tx` through their signatures.
`manager.Querier(ctx)` returns the active transaction while a unit of work is in
progress, and falls back to `database` for an auto-commit statement, so the same
store code runs in either setting:

```go
_ = manager.Run(ctx, func(ctx context.Context) error {
	querier := manager.Querier(ctx) // the active transaction, bound to ctx
	_, err := querier.ExecContext(ctx, `INSERT INTO widgets (id) VALUES (?)`, "w1")
	return err // returning an error rolls the whole unit back
})
```

Throughout the reference, `manager` is a `*transaction.Manager`, `database` is
the underlying `*sql.DB`, `ctx` is a `context.Context`, and `bus` is an
`*eventbus.Bus`. The outbox pages build a `Store`, a `Registry`, and a
`Publisher` on top of these. SQLite is used for the runnable examples; the same
code runs against PostgreSQL by opening a PostgreSQL `database` and choosing the
PostgreSQL store constructor. If you are starting from scratch, the
[Getting Started](/gokeel/getting-started/) page walks through the first unit of
work end to end.
