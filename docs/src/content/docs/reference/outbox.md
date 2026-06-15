---
title: Outbox
description: The transactional outbox in gokeel — Store, the SQLite and PostgreSQL stores, Publication, Registry, Publisher, Resubmitter, and the JSON Serializer.
---

The `outbox` package implements the transactional outbox pattern: a publication
is written in the same database transaction as the business change that produced
the event, delivered through the in-process bus after that transaction commits,
and its outcome is settled back in the store. Delivery is at-least-once, so
listeners must be idempotent. The package decorates the
[Event Bus](/gokeel/reference/event-bus/) and defers delivery to after commit
through the [Transaction Manager](/gokeel/reference/transaction-manager/).

```go
import "github.com/cgardev/gokeel/outbox"
```

## Store

`Store` is the outbound port that persists publications. `Create` receives the
caller's `Querier`, so the publication joins the transaction of the business
change; every other method runs on its own connection, because outcomes are
settled independently of any business transaction.

```go
type Store interface {
    Initialize(ctx context.Context) error
    Create(ctx context.Context, querier Querier, publication Publication) error
    ClaimProcessing(ctx context.Context, id uuid.UUID) (bool, error)
    MarkCompleted(ctx context.Context, id uuid.UUID, completionDate time.Time) error
    MarkFailed(ctx context.Context, id uuid.UUID) error
    MarkResubmitted(ctx context.Context, id uuid.UUID, resubmissionDate time.Time) (bool, error)
    FindIncomplete(ctx context.Context) ([]Publication, error)
    FindIncompletePublishedBefore(ctx context.Context, reference time.Time) ([]Publication, error)
}
```

`Initialize` brings the schema up to date and is safe to call on every start.
`ClaimProcessing` reports whether the caller obtained the publication for
delivery; it returns `false` when another dispatcher already holds or completed
it, which deduplicates concurrent dispatch attempts. `MarkResubmitted` returns
`false` when another caller resubmitted or settled the publication first.
Applications rarely call `Store` directly — the `Registry` drives it — but the
interface is the seam for a custom backend.

`Querier` is the minimal execution surface the statements run against. It is
satisfied by `*sql.DB`, `*sql.Tx`, and `*sql.Conn`, and so by the querier the
transaction manager resolves from the context.

```go
type Querier interface {
    QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
    ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}
```

## NewSQLiteStore and NewPostgresStore

The two built-in stores share the same schema and queries; only the migration
dialect and the placeholder style differ. Each constructor is variadic in
`Option`.

```go
func NewSQLiteStore(database *sql.DB, completionMode CompletionMode, options ...Option) *SQLiteStore
func NewPostgresStore(database *sql.DB, completionMode CompletionMode, options ...Option) *PostgresStore
```

```go
database, err := sql.Open("sqlite", "app.db")
if err != nil {
    return err
}

store := outbox.NewSQLiteStore(database, outbox.CompletionModeUpdate)
if err := store.Initialize(ctx); err != nil {
    return err
}
```

By default the store migrates the schema with the native `database/sql`
migrator, so the core go.mod carries no migration-engine dependency.
`WithMigrator` overrides that default; pass `gowaymigrator.New()` from
`github.com/cgardev/gokeel/outbox/gowaymigrator` to apply the schema with goway
instead. See the [Schema Migrator](/gokeel/reference/migrator/) reference.

```go
import "github.com/cgardev/gokeel/outbox/gowaymigrator"

store := outbox.NewPostgresStore(database, outbox.CompletionModeUpdate,
    outbox.WithMigrator(gowaymigrator.New()))
```

## CompletionMode

`CompletionMode` selects how a completed publication is settled by
`MarkCompleted`.

```go
const (
    CompletionModeUpdate  CompletionMode = iota // keep the row, recording status and completion date
    CompletionModeDelete                         // remove the row
    CompletionModeArchive                        // move the row to the archive table
)
```

`CompletionModeUpdate` keeps every publication for auditing.
`CompletionModeDelete` discards completed rows to keep the table small.
`CompletionModeArchive` moves them into `event_publication_archive`, retaining
history without weighing down the active table.

## Publication

`Publication` is one outbox entry: the publication of a single event to a single
target listener. The `Registry` produces one per subscribed listener.

```go
type Publication struct {
    ID              uuid.UUID
    ListenerID      eventbus.ListenerID
    EventType       string
    SerializedEvent string

    // Event holds the in-memory event instance. It is populated when the
    // publication is created or deserialized for resubmission; it is never
    // persisted as such.
    Event any

    PublicationDate      time.Time
    CompletionDate       *time.Time
    Status               Status
    CompletionAttempts   int
    LastResubmissionDate *time.Time
}
```

`Status` models the lifecycle of an entry: `StatusPublished`,
`StatusProcessing`, `StatusCompleted`, `StatusFailed`, and `StatusResubmitted`.

## Serializer and NewJSONSerializer

`Serializer` converts events to and from their persisted representation.
`JSONSerializer` is the standard implementation, backed by `encoding/json`.

```go
type Serializer interface {
    Serialize(event any) (eventType string, payload string, err error)
    Deserialize(eventType string, payload string) (event any, err error)
}

func NewJSONSerializer() *JSONSerializer
```

Event types are registered with the generic `RegisterEventType` under a stable
name, which decouples the persisted representation from Go type names across
refactorings.

```go
func RegisterEventType[T any](serializer *JSONSerializer, name string) error
```

```go
serializer := outbox.NewJSONSerializer()
if err := outbox.RegisterEventType[OrderPlaced](serializer, "order.placed"); err != nil {
    return err
}
```

Serializing or deserializing an unregistered type returns
`outbox.ErrUnknownEventType`. Binding a name or a type that is already bound to
something different returns `outbox.ErrConflictingRegistration`; registering the
same pair again is allowed.

## Registry and NewRegistry

`Registry` coordinates the pattern: it stores one publication per subscribed
listener through the caller's querier and delivers each publication through the
bus. It does not own the transaction — the `Publisher` writes inside a unit of
work and defers delivery to after commit. A `Registry` is immutable after
construction and safe for concurrent use.

```go
func NewRegistry(store Store, bus EventBus, serializer Serializer) *Registry
```

`EventBus` is the slice of the in-process bus the registry relies on, satisfied
by `*eventbus.Bus`.

```go
serializer := outbox.NewJSONSerializer()
registry := outbox.NewRegistry(store, bus, serializer)
```

`Publish` stores one publication of the event for every subscribed listener,
writing through the provided querier so the publications join the business
transaction; the returned publications are pending and must be handed to
`Dispatch` after the transaction commits. `Dispatch` delivers each publication
and settles the outcome — completed on success, failed otherwise.
`ResubmitIncomplete` re-delivers every incomplete publication, restoring the
event from its serialized form; a positive `olderThan` considers only
publications published before that age, which avoids racing against dispatches
that are still in flight.

```go
func (r *Registry) Publish(ctx context.Context, querier Querier, event any) ([]Publication, error)
func (r *Registry) Dispatch(ctx context.Context, publications ...Publication) error
func (r *Registry) ResubmitIncomplete(ctx context.Context, olderThan time.Duration) error
```

## Publisher and NewPublisher

`Publisher` is the bridge between a business write and the registry: it stores
the publications inside the current unit of work and delivers them only after
that unit commits. When no unit of work is active the originating write has
already committed, so the publications are dispatched immediately. A `Publisher`
is immutable after construction and safe for concurrent use.

```go
func NewPublisher(registry *Registry, querier QuerierSource) *Publisher
```

`QuerierSource` resolves the querier the rows are written through; it is
satisfied by `*transaction.Manager`, whose `Querier` returns the active
transaction when a unit of work is in progress.

```go
publisher := outbox.NewPublisher(registry, manager)

err := manager.Run(ctx, func(ctx context.Context) error {
    if err := placeOrder(ctx, order); err != nil {
        return err
    }
    // The publication joins this transaction; delivery is deferred to commit.
    return publisher.Publish(ctx, OrderPlaced{ID: order.ID})
})
```

`Publish` does not fail the call on a delivery error: the affected publications
stay incomplete and are recovered through `ResubmitIncomplete`.
`WithAsynchronousDispatch` returns a `Publisher` that hands committed
publications to a background goroutine, so callers in a request path do not wait
for slow listeners; the at-least-once guarantee is unchanged.

```go
func (p *Publisher) WithAsynchronousDispatch() *Publisher
```

```go
publisher := outbox.NewPublisher(registry, manager).WithAsynchronousDispatch()
```

## Resubmitter and NewResubmitter

`Resubmitter` periodically re-delivers incomplete publications, so a delivery
that failed against a temporarily unavailable collaborator is retried while the
application runs instead of waiting for a restart.

```go
func NewResubmitter(registry *Registry, interval, minimumAge time.Duration) *Resubmitter
```

It runs one resubmission pass per `interval`, considering only publications
older than `minimumAge` so it does not race against dispatches still in flight.
`Start` launches the background loop — one pass immediately, which recovers the
leftovers of a previous run, then one pass per interval — and returns a stop
function that cancels the loop and waits for an in-flight pass to finish.

```go
func (r *Resubmitter) Start() (stop func())
```

```go
resubmitter := outbox.NewResubmitter(registry, 30*time.Second, time.Minute)
stop := resubmitter.Start()
defer stop()
```

For an end-to-end walkthrough that wires these pieces together, see the
[Transactional Outbox](/gokeel/guides/transactional-outbox/) guide.
