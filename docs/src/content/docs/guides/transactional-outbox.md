---
title: The Transactional Outbox
description: Write domain events in the same transaction as the business change, then deliver them reliably after the commit.
---

The transactional outbox solves a single problem: a business write and the
events it produces must succeed or fail together. The `outbox` package stores
each event as a row in the producing transaction, then delivers it to its
listeners only after that transaction commits. Delivery is at-least-once, so
listeners must be idempotent.

The package layers a few collaborators on top of the in-memory
[eventbus](/gokeel/guides/event-bus/) and the
[transaction](/gokeel/guides/transactions/) manager: a `Store` that persists
publications, a `Registry` that stores and delivers them, a `Publisher` that
ties delivery to the commit, and a `Resubmitter` that recovers stragglers.

## The Store

A `Store` persists publications and settles their outcomes. Two
implementations ship with the package, one per dialect:

```go
import "github.com/cgardev/gokeel/outbox"

store := outbox.NewSQLiteStore(database, outbox.CompletionModeUpdate)
// or
store := outbox.NewPostgresStore(database, outbox.CompletionModeUpdate)
```

Both take an open `*sql.DB` and a `CompletionMode`. Call `Initialize` once at
startup to bring the schema up to date; it is idempotent and safe to run on
every boot:

```go
if err := store.Initialize(ctx); err != nil {
	return err
}
```

`Initialize` applies the embedded migration scripts through a `Migrator`. The
default is a native `database/sql` migrator, so the core carries no
migration-engine dependency. To run the schema through goway instead, supply
the adapter from the optional module with `WithMigrator`:

```go
import "github.com/cgardev/gokeel/outbox/gowaymigrator"

store := outbox.NewSQLiteStore(
	database,
	outbox.CompletionModeUpdate,
	outbox.WithMigrator(gowaymigrator.New()),
)
```

## Completion modes

`CompletionMode` selects how a publication is settled once its listener
succeeds:

- `CompletionModeUpdate` keeps the row and records its status and completion
  date. This is the auditable default.
- `CompletionModeDelete` removes the row, keeping the table small.
- `CompletionModeArchive` moves the row into the `event_publication_archive`
  table.

```go
store := outbox.NewSQLiteStore(database, outbox.CompletionModeArchive)
```

## The Registry

A `Registry` coordinates the pattern. It stores one publication per subscribed
listener through the caller's querier, delivers each one through the bus, and
settles the outcome in the store. Construct it over a store, an event bus, and
a serializer:

```go
serializer := outbox.NewJSONSerializer()
if err := outbox.RegisterEventType[OrderPlaced](serializer, "order.placed"); err != nil {
	return err
}

bus := eventbus.NewBus()
registry := outbox.NewRegistry(store, bus, serializer)
```

The `JSONSerializer` decouples the stored representation from Go type names:
each event type is registered under a stable string name with
`RegisterEventType`, so a refactoring that renames the Go type does not orphan
the rows already on disk. Registering the same pair twice is allowed; binding
a name or type that is already bound differently returns
`outbox.ErrConflictingRegistration`.

Listeners subscribe on the bus exactly as they would without the outbox:

```go
err := eventbus.SubscribeTo(bus, "shipping", func(ctx context.Context, event OrderPlaced) error {
	return shipping.Schedule(ctx, event.OrderID)
})
```

## The Publisher

The `Publisher` is the bridge between a business write and the registry. It
writes the publication rows through the querier the transaction manager
resolves from the context, then defers their delivery until that transaction
commits:

```go
manager := transaction.NewManager(database)
publisher := outbox.NewPublisher(registry, manager)
```

`NewPublisher` takes the registry and a `QuerierSource`, which `*transaction.Manager`
satisfies. Inside a unit of work, call `Publish`:

```go
err := manager.Run(ctx, func(ctx context.Context) error {
	if err := orders.Insert(ctx, order); err != nil {
		return err
	}
	return publisher.Publish(ctx, OrderPlaced{OrderID: order.ID})
})
```

`Publish` writes one publication per subscribed listener through the active
transaction, so the rows and the `orders` insert commit or roll back as one. It
then registers an after-commit callback that delivers them. If the transaction
rolls back, the rows vanish with it and nothing is delivered.

When no unit of work is active, the originating write has already
auto-committed, so the publications are dispatched immediately instead. Either
way, a delivery failure does not fail the call: the affected publications stay
incomplete and are recovered later.

### Asynchronous dispatch

By default the after-commit delivery runs synchronously, so the caller waits
for the listeners. To hand committed publications to a background goroutine
instead, use `WithAsynchronousDispatch`:

```go
publisher := outbox.NewPublisher(registry, manager).WithAsynchronousDispatch()
```

The at-least-once guarantee is unchanged: publications settle only after their
listener succeeds, and incomplete ones are recovered through resubmission.

## How a publication is settled

`Registry.Dispatch` drives one publication from stored to settled. It first
claims the row with `ClaimProcessing`, an atomic status transition that
succeeds for exactly one of several concurrent dispatchers and so deduplicates
delivery. On a successful delivery the store records the outcome through
`MarkCompleted`, applying the configured completion mode:

```sql
-- CompletionModeUpdate
UPDATE event_publication SET status = ?, completion_date = ? WHERE id = ?
```

On a failed delivery the store calls `MarkFailed`, which leaves the row
incomplete for a later resubmission rather than losing the event.

## The Resubmitter

A crash or a temporarily unavailable collaborator can leave publications
delivered but unsettled, or never delivered at all. The `Resubmitter` recovers
them by re-delivering every incomplete publication on a schedule:

```go
resubmitter := outbox.NewResubmitter(registry, 30*time.Second, 1*time.Minute)
stop := resubmitter.Start()
defer stop()
```

`NewResubmitter` takes the registry, the interval between passes, and a minimum
age. Only publications older than `minimumAge` are considered, which avoids
racing against dispatches that are still in flight. `Start` runs one pass
immediately, to recover the leftovers of a previous run, then one pass per
interval; the returned `stop` function cancels the loop and waits for an
in-flight pass to finish.

Each pass restores the event from its serialized representation, transitions
the row back into delivery through `MarkResubmitted`, and dispatches it again.
Because delivery is at-least-once, a publication that was in fact delivered
before the crash is delivered a second time on resubmission, which is why
listeners must be idempotent.

To drive a single recovery pass yourself rather than running the loop, call
the registry directly:

```go
// Re-deliver every incomplete publication older than one minute.
err := registry.ResubmitIncomplete(ctx, 1*time.Minute)
```

Pass a non-positive duration to consider every incomplete publication
regardless of age.

## Error values to expect

The serializer surfaces a couple of sentinel errors worth handling:

- `outbox.ErrUnknownEventType` — an event was serialized or deserialized whose
  type was never registered with `RegisterEventType`.
- `outbox.ErrConflictingRegistration` — a second registration tried to bind an
  already used name or type to something different.

The lifecycle of a publication is modelled by the `outbox.Status` values
(`StatusPublished`, `StatusProcessing`, `StatusCompleted`, `StatusFailed`, and
`StatusResubmitted`), which the store sets as it settles each row.
