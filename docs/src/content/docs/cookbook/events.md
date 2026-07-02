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

## Queue a consumer with retries

You want a consumer that processes events from its own queue and retries
failures on its own, without the publisher waiting or noticing.

```go
broker := eventbus.NewMemoryBroker()
defer broker.Stop()

err := eventbus.Consume(ctx, broker, "sync-crm",
    func(ctx context.Context, event OrderPlaced) error {
        return crm.Upsert(ctx, event.OrderID)
    },
    eventbus.WithMaximumAttempts(8))

err = broker.Publish(ctx, OrderPlaced{OrderID: "A-100", Total: 4999})
```

Gotcha: `Publish` returns immediately and never reports handler errors — they
drive the consumer's retries and, past the budget, surface as dead letters.

## Keep events in order under failure

You want a consumer to process events strictly in publication order, even
while one of them is failing and retrying.

```go
err := eventbus.Consume(ctx, broker, "ledger",
    func(ctx context.Context, event OrderPlaced) error {
        return ledger.Append(ctx, event.OrderID)
    })
```

Gotcha: consumers are FIFO by default, so nothing to configure — a failing
event blocks its successors while it retries, which is exactly what order
means. Consumers whose events are independent opt out with
`eventbus.WithUnorderedDelivery()`.

## Revive a dead letter

You want to inspect the events that exhausted their retry budget and give one
a fresh start after fixing its consumer.

```go
letters, err := broker.FindExhausted(ctx, 100)
for _, letter := range letters {
    // letter.LastError holds the final failure cause.
    revived, err := broker.Resubmit(ctx, letter.Reference)
    _ = revived // true: the event re-entered its queue with a fresh budget.
    _ = err
}
```

Gotcha: a revived event re-enters at the position its original publication
order dictates, so an ordered consumer stays ordered.

For the full surface — `Subscribe`, `Deliver`, and listener identity — see
[The Event Bus](/gokeel/guides/event-bus/); for the consumer contract and its
two engines, see [The Broker](/gokeel/guides/broker/). For events that must
survive a commit, see
[The Transactional Outbox](/gokeel/guides/transactional-outbox/).
