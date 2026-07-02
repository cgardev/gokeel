---
title: The Event Bus
description: Register typed listeners, publish events, and rely on ordered delivery with panic isolation and per-listener identity in the in-process synchronous event bus.
---

A gokeel `Bus` is an in-process, synchronous event bus. Publishing an event
invokes every matching listener in the calling goroutine, one after another, and
returns once they have all run. The bus carries no persistence concern: it never
writes to a database and offers no delivery guarantee beyond the lifetime of the
process. Libraries that need durable delivery, such as the outbox, build on top
of it.

The bus depends only on the Go standard library, and a `Bus` is safe for
concurrent use. No lock is held while a handler runs, so a handler may subscribe
or publish reentrantly without deadlocking.

## Constructing a bus

`NewBus` returns an empty bus with no listeners:

```go
import "github.com/cgardev/gokeel/eventbus"

bus := eventbus.NewBus()
```

## Registering a typed listener

`SubscribeTo[T]` registers a listener that receives every event whose dynamic
type is `T`. The listener is identified by a `ListenerID`, and the handler
receives the event already converted to `T`:

```go
type OrderPlaced struct {
	OrderID string
	Total   int64
}

err := eventbus.SubscribeTo(bus, "send-confirmation-email",
	func(ctx context.Context, event OrderPlaced) error {
		return mailer.SendConfirmation(ctx, event.OrderID)
	})
```

The identifier must be unique. Subscribing twice under the same `ListenerID`
returns an error wrapping `eventbus.ErrDuplicateListener`. An empty identifier,
or a nil handler, is also rejected.

## Publishing an event

`Publish` multicasts the event to every matching listener. The dynamic type of
the value decides which listeners match, so an `OrderPlaced` value reaches every
listener registered with `SubscribeTo[OrderPlaced]`:

```go
err := bus.Publish(ctx, OrderPlaced{OrderID: "A-100", Total: 4999})
```

`Publish` is synchronous. It returns only after every matching listener has run.
Listeners with no matching type are not invoked, and an event that matches no
listener is a no-op that returns a nil error.

## Ordered delivery

Listeners are invoked in subscription order: the listener registered first runs
first. `ListenersFor` exposes that order without delivering anything, returning
the identifiers of every listener that would match the event:

```go
_ = eventbus.SubscribeTo(bus, "reserve-stock",  reserveStock)
_ = eventbus.SubscribeTo(bus, "send-receipt",   sendReceipt)

ids := bus.ListenersFor(OrderPlaced{OrderID: "A-100"})
// ids == []eventbus.ListenerID{"reserve-stock", "send-receipt"}
```

`Publish` walks exactly this list. If a listener returns an error, the remaining
listeners are still invoked; `Publish` collects every failure and returns them
joined with `errors.Join`, so a single call surfaces all the errors at once.

## Panic isolation

A handler that panics cannot take down the publishing caller. The bus recovers
the panic, captures the stack trace, and surfaces it as an error wrapping
`eventbus.ErrListenerPanic`. Because the panic becomes an ordinary error, the
remaining listeners still run and the caller observes the failure through the
returned error rather than a crash:

```go
err := bus.Publish(ctx, OrderPlaced{OrderID: "A-100"})
if errors.Is(err, eventbus.ErrListenerPanic) {
	// A listener panicked; the recovered value and stack are in err.
}
```

## Listener identity

Every subscription is keyed by a `ListenerID`, a named string type:

```go
type ListenerID string
```

The identifier does more than guard against duplicates. It lets a caller address
one listener directly with `Deliver`, bypassing the type-based multicast that
`Publish` performs:

```go
err := bus.Deliver(ctx, "send-receipt", OrderPlaced{OrderID: "A-100"})
```

`Deliver` invokes only the named listener and runs it through the same panic
recovery as `Publish`. Delivering to an identifier with no subscription behind it
returns an error wrapping `eventbus.ErrUnknownListener`.

## Low-level subscription

`SubscribeTo[T]` is a thin wrapper over `Subscribe`, which is the primitive the
bus exposes. `Subscribe` takes a `matches` predicate that decides which events
the listener receives, and a `Handler` that receives the raw `any` event:

```go
err := bus.Subscribe("audit-everything",
	func(event any) bool { return true }, // match every event
	func(ctx context.Context, event any) error {
		return audit.Record(ctx, event)
	})
```

`SubscribeTo[T]` simply supplies a predicate that matches on the dynamic type and
a handler that performs the type conversion before calling your typed function.
Reach for `Subscribe` directly only when a listener must span several event types
or match on something other than the type alone.

## Where to go next

The `Bus` is the synchronous primitive of gokeel's eventing story: the caller
waits for every handler and observes every failure. When consumers should own
queues of their own — processed FIFO, retried independently, parked as dead
letters when they keep failing — subscribe them through
[The Broker](/gokeel/guides/broker/), whose in-memory engine is built on this
bus and whose SQL engine adds durability behind the same interface. For events
that must survive a commit and be delivered at least once, see
[The Transactional Outbox](/gokeel/guides/transactional-outbox/), which writes
events inside the same transaction and publishes them onto a bus after the
commit succeeds. To bind event handling to transaction lifecycle hooks, see
[Propagation & Synchronizations](/gokeel/guides/propagation-and-synchronizations/).
