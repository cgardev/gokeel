---
title: Event Bus
description: Constructing a Bus, subscribing listeners by identifier or by event type, and publishing events synchronously in memory.
---

The `eventbus` package provides a generic, synchronous in-memory event bus. It
carries no persistence concern: libraries that need delivery guarantees, such as
the [outbox](/gokeel/reference/synchronizations/) package, build on top of it. A
`Bus` is safe for concurrent use, and no lock is held while a handler runs, so a
handler may subscribe or publish reentrantly without deadlocking.

```go
import "github.com/cgardev/gokeel/eventbus"
```

## NewBus

`NewBus` constructs an empty `Bus`. No options are required, and the zero value
behind the returned pointer is immediately ready for subscriptions.

```go
func NewBus() *Bus
```

```go
bus := eventbus.NewBus()
```

A single `Bus` is shared across the application; deliver events through its
methods rather than constructing one per call site.

## ListenerID

A `ListenerID` names a subscribed listener. Identifiers are unique within a
`Bus`, so a caller can address one listener individually or multicast an event
to every matching listener.

```go
type ListenerID string
```

```go
const billing eventbus.ListenerID = "billing"
```

## SubscribeTo

`SubscribeTo` is the generic entry point: it registers a listener that receives
every event whose dynamic type is `T`. The handler is called with the
already-typed event, so no assertion is needed inside it.

```go
func SubscribeTo[T any](bus *Bus, id ListenerID, handle func(ctx context.Context, event T) error) error
```

```go
type OrderPlaced struct {
    OrderID string
    Total   int64
}

err := eventbus.SubscribeTo(bus, "billing", func(ctx context.Context, event OrderPlaced) error {
    return charge(ctx, event.OrderID, event.Total)
})
```

Subscribing twice under the same `ListenerID` returns `ErrDuplicateListener`.
This is the registration function to reach for in almost every case; `Subscribe`
exists for listeners that match on something other than the event type.

## Subscribe

`Subscribe` is the lower-level registration function. The `matches` predicate
decides which events the listener receives, and `handle` processes each one. A
`Handler` is `func(ctx context.Context, event any) error`.

```go
func (b *Bus) Subscribe(id ListenerID, matches func(event any) bool, handle Handler) error
```

```go
err := bus.Subscribe("audit",
    func(event any) bool { return true }, // every event
    func(ctx context.Context, event any) error {
        return record(ctx, event)
    })
```

The identifier must not be empty, and both `matches` and `handle` must be
non-nil; otherwise `Subscribe` returns an error. `SubscribeTo` is implemented in
terms of `Subscribe`, supplying a type-assertion predicate and a handler that
unwraps the event to `T`.

## Publish

`Publish` multicasts the event to every matching listener, in subscription
order. The returned error joins the failures of every listener that rejected the
event, and the remaining listeners are still invoked, so one failure does not
suppress later deliveries.

```go
func (b *Bus) Publish(ctx context.Context, event any) error
```

```go
err := bus.Publish(ctx, OrderPlaced{OrderID: "A-1", Total: 4200})
```

Because delivery is synchronous, `Publish` returns only after every matching
handler has run. Inspect the joined error with `errors.Is` to detect a specific
failure mode, for example a panicking handler.

## ListenersFor

`ListenersFor` returns the identifiers of every listener subscribed to the event,
in subscription order, without delivering anything. It answers which listeners a
`Publish` would reach.

```go
func (b *Bus) ListenersFor(event any) []ListenerID
```

```go
ids := bus.ListenersFor(OrderPlaced{}) // e.g. []eventbus.ListenerID{"billing", "audit"}
```

## Deliver

`Deliver` hands the event to a single named listener rather than multicasting. A
panicking handler is recovered and reported as an error wrapping
`ErrListenerPanic`, so one misbehaving listener cannot take down the publishing
caller.

```go
func (b *Bus) Deliver(ctx context.Context, id ListenerID, event any) error
```

```go
err := bus.Deliver(ctx, "billing", OrderPlaced{OrderID: "A-1", Total: 4200})
```

Delivering to an identifier with no subscription behind it returns
`ErrUnknownListener`. `Publish` is built on `Deliver`: it resolves the matching
identifiers with `ListenersFor` and delivers to each in turn.

## Errors

The package exports three sentinel errors, each matchable with `errors.Is`:

- `ErrDuplicateListener` reports a `Subscribe` under an identifier that is
  already taken.
- `ErrUnknownListener` reports a `Deliver` towards an identifier with no
  subscription behind it.
- `ErrListenerPanic` reports a handler that panicked while processing an event;
  the panic is recovered and surfaced through this error, with the captured
  stack trace attached.

```go
if err := bus.Publish(ctx, event); errors.Is(err, eventbus.ErrListenerPanic) {
    // a handler panicked; the others still ran
}
```
