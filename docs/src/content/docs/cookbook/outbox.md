---
title: The Outbox
description: Recipes for initializing a store, writing events in a transaction, dispatching after commit, resubmitting stragglers, completion modes, and custom serializers.
---

## Initialize a store and write an event in a transaction

You want a business write and the events it produces to commit or roll back together.

```go
store := outbox.NewSQLiteStore(database, outbox.CompletionModeUpdate)
if err := store.Initialize(ctx); err != nil {
    return err
}

serializer := outbox.NewJSONSerializer()
if err := outbox.RegisterEventType[OrderPlaced](serializer, "order.placed"); err != nil {
    return err
}

bus := eventbus.NewBus()
registry := outbox.NewRegistry(store, bus, serializer)
manager := transaction.NewManager(database)
publisher := outbox.NewPublisher(registry, manager)

err := manager.Run(ctx, func(ctx context.Context) error {
    if err := orders.Insert(ctx, order); err != nil {
        return err
    }
    return publisher.Publish(ctx, OrderPlaced{OrderID: order.ID})
})
```

```sql
INSERT INTO event_publication
    (id, listener_id, event_type, serialized_event, publication_date, status, completion_attempts)
VALUES (?, ?, ?, ?, ?, ?, ?)
```

Gotcha: `Publish` writes through the querier the manager resolves from the
context, so call it inside `manager.Run`; outside a unit of work the rows
auto-commit and are dispatched at once.

## Publish pending events after commit

You want listeners to run only once the producing transaction is durable.

```go
// Publish stores one row per subscribed listener, then registers an
// after-commit callback that delivers them. A rollback discards both the
// business change and its publications, so nothing is delivered.
err := manager.Run(ctx, func(ctx context.Context) error {
    return publisher.Publish(ctx, OrderPlaced{OrderID: order.ID})
})

// Hand committed publications to a background goroutine instead of waiting
// for slow listeners on the request path.
publisher = outbox.NewPublisher(registry, manager).WithAsynchronousDispatch()
```

Gotcha: a delivery failure does not fail `Publish`; the affected publications
stay incomplete and are recovered by the resubmitter, so listeners must be
idempotent.

## Resubmit unfinished entries on an interval

You want delivery to recover by itself after a crash or a temporarily unavailable collaborator.

```go
resubmitter := outbox.NewResubmitter(registry, 30*time.Second, 1*time.Minute)
stop := resubmitter.Start()
defer stop()
```

Gotcha: only publications older than the minimum age are considered, which
avoids racing in-flight dispatches; `Start` runs one pass immediately and
`stop` waits for an in-flight pass to finish.

## Drive a single recovery pass yourself

You want to re-deliver incomplete publications once, without running the loop.

```go
// Re-deliver every incomplete publication older than one minute.
err := registry.ResubmitIncomplete(ctx, 1*time.Minute)
```

Gotcha: pass a non-positive duration to consider every incomplete publication
regardless of age; the returned error joins the failures of each publication
that could not be re-delivered.

## Choose a completion mode

You want to control what happens to a row once its listener succeeds.

```go
// Keep the row, recording status and completion date (the auditable default).
store := outbox.NewSQLiteStore(database, outbox.CompletionModeUpdate)

// Remove the row, keeping the table small.
store = outbox.NewSQLiteStore(database, outbox.CompletionModeDelete)

// Move the row into the event_publication_archive table.
store = outbox.NewSQLiteStore(database, outbox.CompletionModeArchive)
```

```sql
-- CompletionModeUpdate
UPDATE event_publication SET status = ?, completion_date = ? WHERE id = ?
```

Gotcha: the mode is fixed at construction and applies to every publication the
store settles through `MarkCompleted`; there is no per-event override.

## Run the schema through goway

You want migrations recorded in a Flyway-style history table instead of the native migrator.

```go
import "github.com/cgardev/gokeel/outbox/gowaymigrator"

store := outbox.NewPostgresStore(
    database,
    outbox.CompletionModeUpdate,
    outbox.WithMigrator(gowaymigrator.New()),
)
```

Gotcha: importing `gowaymigrator` is the single action that pulls goway into the
build; the default `NativeMigrator` keeps the outbox core on `database/sql`
alone.

## Plug a custom Serializer

You want to store events in a representation other than the bundled JSON one.

```go
type ProtoSerializer struct{}

func (ProtoSerializer) Serialize(event any) (eventType string, payload string, err error) {
    // Render the event to its persistent form and return its stable type name.
}

func (ProtoSerializer) Deserialize(eventType string, payload string) (event any, err error) {
    // Reconstruct the event registered under eventType from payload.
}

registry := outbox.NewRegistry(store, bus, ProtoSerializer{})
```

Gotcha: `Deserialize` must return the same concrete type the bus listeners
expect; on an unregistered type the `JSONSerializer` returns
`outbox.ErrUnknownEventType`, and a conflicting `RegisterEventType` returns
`outbox.ErrConflictingRegistration`.

For the full walkthrough of how these collaborators fit together, see
[The Transactional Outbox](/gokeel/guides/transactional-outbox/).
