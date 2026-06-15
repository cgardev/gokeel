---
title: In-Process Events
description: Recipes for registering typed listeners, publishing events, multicasting to several listeners, and isolating a panicking handler.
---

## Register a typed listener

You want a function to run whenever an event of a given type is published.

```go
import "github.com/cgardev/gokeel/eventbus"

type OrderPlaced struct {
    OrderID string
    Total   int64
}

bus := eventbus.NewBus()

err := eventbus.SubscribeTo(bus, "send-confirmation-email",
    func(ctx context.Context, event OrderPlaced) error {
        return mailer.SendConfirmation(ctx, event.OrderID)
    })
```

Gotcha: the `ListenerID` ("send-confirmation-email") must be unique, so subscribing
twice under the same identifier returns an error wrapping `eventbus.ErrDuplicateListener`.

## Publish an event

You want to deliver a value to every listener registered for its type.

```go
err := bus.Publish(ctx, OrderPlaced{OrderID: "A-100", Total: 4999})
```

Gotcha: `Publish` is synchronous and returns only after every matching listener has
run; an event that matches no listener is a no-op that returns a nil error.

## Register multiple listeners for one type

You want several independent reactions to the same event, in a known order.

```go
_ = eventbus.SubscribeTo(bus, "reserve-stock", reserveStock)
_ = eventbus.SubscribeTo(bus, "send-receipt", sendReceipt)

ids := bus.ListenersFor(OrderPlaced{OrderID: "A-100"})
// ids == []eventbus.ListenerID{"reserve-stock", "send-receipt"}

err := bus.Publish(ctx, OrderPlaced{OrderID: "A-100", Total: 4999})
```

Gotcha: listeners run in subscription order; if one returns an error the rest still
run, and `Publish` returns their failures joined with `errors.Join`.

## Isolate a panicking listener

You want a handler that panics to fail in place rather than crash the publisher.

```go
err := bus.Publish(ctx, OrderPlaced{OrderID: "A-100", Total: 4999})
if errors.Is(err, eventbus.ErrListenerPanic) {
    // A listener panicked; the recovered value and stack are in err.
}
```

Gotcha: the bus recovers the panic, captures the stack, and surfaces it as an error
wrapping `eventbus.ErrListenerPanic`, so the remaining listeners still run.

For the full surface — `Subscribe`, `Deliver`, and listener identity — see
[The Event Bus](/gokeel/guides/event-bus/). For events that must survive a commit,
see [The Transactional Outbox](/gokeel/guides/transactional-outbox/).
