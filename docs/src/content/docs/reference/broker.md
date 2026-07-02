---
title: Broker
description: The engine-independent Broker contract — Consume, Publish, consumer options, dead letters — and the constructors of the memory and SQL engines.
---

The `Broker` interface is the engine-independent consumer contract of gokeel:
exactly one delivery per consumer, an independent retry budget per consumer,
FIFO ordering by default with an unordered opt-out, and dead letters for
exhausted events. The `eventbus` package defines the contract and ships the
in-memory engine; the `sqlbus` module implements the same contract with
persistence and cross-node consumption.

```go
import "github.com/cgardev/gokeel/eventbus"
```

## Broker

```go
type Broker interface {
    Publish(ctx context.Context, event any) error
    Subscribe(ctx context.Context, registration ConsumerRegistration) error
    FindExhausted(ctx context.Context, limit int) ([]DeadLetter, error)
    Resubmit(ctx context.Context, reference string) (bool, error)
}
```

`Publish` hands the event to the queue of every matching consumer and reports
only validation and persistence failures; handler outcomes settle
asynchronously through the per-consumer retries and surface as dead letters.
In-process engines deliver exactly once per consumer for the lifetime of the
process; persistent engines settle each delivery exactly once but may execute
a handler again after a crash or an expired claim, so handlers must be
idempotent when durability is in play.

## Consume

`Consume` is the typed front door of the contract: it registers a consumer of
every event of type `T`, with FIFO ordering and the default retry budget
unless options say otherwise.

```go
func Consume[T any](ctx context.Context, broker Broker, id ListenerID,
    handle func(ctx context.Context, event T) error, options ...ConsumerOption) error
```

```go
err := eventbus.Consume(ctx, broker, "send-confirmation",
    func(ctx context.Context, event OrderPlaced) error {
        return mailer.SendConfirmation(ctx, event.OrderID)
    },
    eventbus.WithMaximumAttempts(8))
```

The identifier must be unique within the broker. On the SQL engine the event
type of the consumer must be registered on the serializer of the bridge.

## Consumer options

| Option | Effect | Default |
| --- | --- | --- |
| `WithUnorderedDelivery()` | Process events concurrently; a failing event retries without delaying the others. | FIFO |
| `WithWorkers(n)` | Concurrency of an unordered consumer. The SQL engine does not interpret it: its concurrency is governed by the dispatcher batch size and the hosting nodes. | 8 |
| `WithMaximumAttempts(n)` | Delivery attempts one event may consume before it parks as a dead letter. | 5 |
| `WithRetryDelay(f)` | Backoff schedule; `f` receives the attempt count just spent. | 5 s doubling to 5 min |
| `WithBroadcastDelivery()` | One delivery per application node instead of one per consumer. In-process engines treat it as regular consumption. | competing |

A FIFO consumer processes one event at a time, in publication order; a failing
event blocks its successors while it retries, and an exhausted event parks as
a dead letter while the queue continues.

## DeadLetter

A `DeadLetter` describes an event whose delivery consumed its attempt budget
for one consumer.

```go
type DeadLetter struct {
    Reference       string          // opaque handle for Resubmit
    ListenerID      ListenerID      // the consumer whose delivery exhausted
    Event           any             // the parked event, when restorable
    Attempts        int             // delivery attempts consumed
    LastError       string          // failure of the final attempt
    PublicationDate time.Time
}
```

`FindExhausted` returns dead letters oldest first, up to the limit — by
publication date on the SQL engine, by the order they parked on the memory
engine. `Resubmit` gives the referenced dead letter a fresh attempt budget and
reports false when the reference is unknown or was already resubmitted. A
revived event re-enters its queue at the position its original publication
order dictates.

## The memory engine

```go
func NewMemoryBroker() *MemoryBroker
func (b *MemoryBroker) Stop()
```

`NewMemoryBroker` constructs the in-process engine: one queue per consumer,
nothing persisted, exactly-once per consumer for the lifetime of the process.
`Stop` cancels the workers, waits for in-flight deliveries to return, and
drops queued events and pending retries. `Publish`, `Subscribe`, and
`Resubmit` after `Stop` return `ErrBrokerStopped`; `FindExhausted` keeps
returning the recorded dead letters for inspection.

## The SQL engine

```go
// package sqlbus
func NewBroker(bridge *Bridge, publisher *Publisher) *Broker
```

`sqlbus.NewBroker` adapts the durable machinery to the same contract:
`Publish` goes through the transactional `Publisher`, so the event joins the
caller's unit of work, and consumers attach durably through the `Bridge` —
competing cluster-wide by default, broadcast per node on request. The caller
keeps running one `Dispatcher` per node, exactly as with the lower-level API;
see the [SQL Bus guide](/gokeel/guides/sql-bus/).

Ordered consumers on the SQL engine claim only the head of their queue, below
the materialization watermark, so their delivery latency is at least the
materialization grace of the bridge.

## Errors

| Error | Meaning |
| --- | --- |
| `eventbus.ErrDuplicateListener` | The consumer identifier is already registered. |
| `eventbus.ErrBrokerStopped` | The memory broker was stopped. |
| `sqlbus.ErrConflictingDeliveryMode` | Another node registered the listener under a different delivery mode. |
| `sqlbus.ErrConflictingOrdering` | Another node registered the listener under a different ordering. |

## Conformance

The suite under `eventbus/bustest` exercises any `Broker` implementation
against the contract — fan-out, FIFO under retries, unordered concurrency,
independent retries, dead letters, and resubmission. Both shipped engines run
it; an alternative engine should too.
