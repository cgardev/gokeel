---
title: Getting Started
description: Install the gokeel modules, open a database, and run your first declarative transaction.
---

This guide walks through installing the gokeel modules, opening a database,
and running a first unit of work end to end.

## Requirements

- Go 1.26 or newer.
- A database driver of your choice. gokeel imports no driver itself, so you
  blank-import the one that matches your database. SQLite is the primary
  backend; PostgreSQL is supported as well.

## Installation

gokeel is published as several independent modules, so you take only the pieces
you need. Add them to your module:

```sh
go get github.com/cgardev/gokeel/transaction
go get github.com/cgardev/gokeel/eventbus
go get github.com/cgardev/gokeel/outbox
```

The `transaction` and `eventbus` modules have zero third-party dependencies and
build on the standard library alone. The `outbox` module composes the other two
and adds `google/uuid`. The only package you must supply yourself is a database
driver, which you import for its side effects so that it registers itself with
`database/sql`:

```go
import (
	"database/sql"

	_ "modernc.org/sqlite" // SQLite driver, blank-imported
)
```

## Opening a connection

gokeel executes against the standard `database/sql` types. Any `*sql.DB`,
`*sql.Tx`, or `*sql.Conn` satisfies the `transaction.Querier` interface that
stores execute their statements against.

```go
database, err := sql.Open("sqlite", "file:app.db")
if err != nil {
	return err
}
defer database.Close()
```

## Creating a Manager

A `transaction.Manager` owns the database transaction lifecycle: it begins,
commits, and rolls back the transactions your units of work run in. Construct
one over the open database with `NewManager`:

```go
manager := transaction.NewManager(database)
```

The Manager is immutable after construction and safe for concurrent use. You
wire it once and inject it wherever a store or use case needs to open a
transaction.

## Your first transaction

With the Manager in hand, `Run` executes a unit of work: it joins an active
transaction or begins one, commits when your function returns `nil`, and rolls
back when it returns an error or panics. Inside the function, `Querier(ctx)`
resolves the executor to run against — the live `*sql.Tx` while a unit of work
is in flight, or the plain `*sql.DB` otherwise.

```go
package main

import (
	"context"
	"database/sql"

	_ "modernc.org/sqlite"

	"github.com/cgardev/gokeel/transaction"
)

func main() {
	ctx := context.Background()

	database, err := sql.Open("sqlite", "file:app.db")
	if err != nil {
		panic(err)
	}
	defer database.Close()

	manager := transaction.NewManager(database)

	err = manager.Run(ctx, func(ctx context.Context) error {
		querier := manager.Querier(ctx) // the active transaction, bound to ctx
		_, err := querier.ExecContext(ctx,
			`INSERT INTO widgets (id, name) VALUES (?, ?)`, "w1", "keel")
		return err // returning nil commits; an error rolls the whole unit back
	})
	if err != nil {
		panic(err)
	}
}
```

The way your function leaves is the decision: return `nil` to commit, return an
error to roll back (the error is still handed back to you), or panic to roll
back and have the panic re-raised once the transaction has settled. You never
call `Commit` or `Rollback` yourself.

When several stores each open their own `Run`, an outer `Run` makes them join a
single transaction rather than committing independently. This propagation —
join an active transaction, or begin a new one — is the default, `Required`. See
[Transactions](/gokeel/guides/transactions/) for the full surface.

## A taste of the event bus

The `eventbus` module is a small, synchronous, in-process bus that delivers
events to subscribed listeners. You register a typed listener with `SubscribeTo`
and multicast an event with `Publish`; delivery is ordered, and a panicking
handler is recovered rather than allowed to take down the publisher.

```go
bus := eventbus.NewBus()
_ = eventbus.SubscribeTo(bus, "send-receipt",
	func(ctx context.Context, event OrderPlaced) error {
		return mailer.SendReceipt(ctx, event.CustomerEmail)
	})
_ = bus.Publish(ctx, OrderPlaced{CustomerEmail: "buyer@example.com"})
```

## A taste of the outbox

The `outbox` module ties the two together to give the transactional outbox
pattern: events are written into the same transaction as the business change
and delivered only after that transaction commits. You build a `Registry` over a
`Store`, the bus, and a serializer, then wrap it in a `Publisher` that defers
delivery to the after-commit phase.

```go
store := outbox.NewSQLiteStore(database, outbox.CompletionModeUpdate)
_ = store.Initialize(ctx) // creates the publication tables

registry := outbox.NewRegistry(store, bus, outbox.NewJSONSerializer())
publisher := outbox.NewPublisher(registry, manager)

_ = manager.Run(ctx, func(ctx context.Context) error {
	// ... write the business change through manager.Querier(ctx) ...
	return publisher.Publish(ctx, OrderPlaced{CustomerEmail: "buyer@example.com"})
})
```

The publication rows are written through the unit of work, so they commit
atomically with the order. Delivery happens once the transaction is durably on
disk, and any entry that fails to deliver stays incomplete for resubmission.

## Next steps

- [Transactions](/gokeel/guides/transactions/) covers `Run`, `Querier`, and how
  several stores commit as one unit of work.
- [Propagation & Synchronizations](/gokeel/guides/propagation-and-synchronizations/)
  explains the propagation modes, savepoints, rollback rules, and the commit
  callbacks.
- [The Event Bus](/gokeel/guides/event-bus/) describes typed subscriptions and
  ordered, panic-isolated delivery.
- [The Transactional Outbox](/gokeel/guides/transactional-outbox/) shows how to
  publish events after commit and recover incomplete deliveries.
- [Schema Migrations](/gokeel/guides/schema-migrations/) explains the native
  migrator and the optional goway-backed one.
