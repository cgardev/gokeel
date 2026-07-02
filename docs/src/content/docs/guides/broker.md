---
title: The Broker
description: One consumer contract over two engines — FIFO queues per consumer, independent retries with dead letters, in memory or durable over SQL — and how to choose and combine them.
---

A gokeel `Broker` is a message bus defined by a contract, not by an engine.
Every published event reaches **every matching consumer exactly once per
consumer**; every consumer owns **its own queue** and **its own retries**; the
queue is **FIFO by default** and concurrent on request; an event that exhausts
its retry budget parks as an inspectable **dead letter** instead of vanishing.

Two engines implement the contract behind the same interface:

| | `eventbus.NewMemoryBroker()` | `sqlbus.NewBroker(...)` |
| --- | --- | --- |
| Lives in | the process memory | PostgreSQL or SQLite |
| Survives a restart | no | yes |
| Reaches other nodes | no | yes — the database is the transport |
| Publishing joins your transaction | no | yes |
| Exactly one delivery per consumer | for the lifetime of the process | settled exactly once; a crash can re-run a handler¹ |

¹ Durability has one honest price: after a crash or an expired claim, the SQL
engine runs the handler again before settling, so its handlers must be
idempotent. The interface, the ordering, the retries, and the dead letters are
identical either way — code written against the contract does not change when
the engine does.

## The contract in one example

`Consume[T]` registers a typed consumer; `Publish` hands the event to the
queue of every consumer whose type matches. Publishing reports only validation
and persistence failures — handler outcomes settle asynchronously through each
consumer's retries and never surface through `Publish`:

```go
import "github.com/cgardev/gokeel/eventbus"

type OrderPlaced struct {
	OrderID string
}

broker := eventbus.NewMemoryBroker()
defer broker.Stop()

err := eventbus.Consume(ctx, broker, "send-confirmation",
	func(ctx context.Context, event OrderPlaced) error {
		return mailer.SendConfirmation(ctx, event.OrderID)
	})

err = broker.Publish(ctx, OrderPlaced{OrderID: "A-100"})
// Returns immediately; "send-confirmation" processes A-100 exactly once.
```

`Publish` reports only validation and persistence failures. A handler error is
not the publisher's concern: it is the beginning of that consumer's retry
story, and nobody else's.

## FIFO, and what it holds under failure

By default a consumer processes its events **strictly in publication order,
one at a time**. That promise is easy to keep on the happy path; the broker
keeps it under failure too — a failing event *blocks its successors* while it
retries, because delivering them first would break the order:

```go
var seen []string
_ = eventbus.Consume(ctx, broker, "ledger",
	func(ctx context.Context, event OrderPlaced) error {
		if event.OrderID == "A-2" && !ledger.Ready() {
			return errors.New("ledger not ready") // A-2 will retry...
		}
		seen = append(seen, event.OrderID)
		return nil
	})

for _, id := range []string{"A-1", "A-2", "A-3"} {
	_ = broker.Publish(ctx, OrderPlaced{OrderID: id})
}
// seen is always [A-1 A-2 A-3] — A-3 waited for A-2's retries.
```

Head-of-line blocking is the definition of FIFO, not a defect: order and
"failures never delay anyone" cannot both hold. When an event exhausts its
budget, the queue does not stay hostage — the event parks as a dead letter and
the successors continue.

## Unordered, when throughput wins

Consumers whose events are independent opt out of ordering and process
concurrently. A failing event retries on its own schedule while the others
keep flowing:

```go
err := eventbus.Consume(ctx, broker, "thumbnails",
	func(ctx context.Context, event ImageUploaded) error {
		return thumbnails.Render(ctx, event.Path)
	},
	eventbus.WithUnorderedDelivery(),
	eventbus.WithWorkers(16))
```

Both kinds coexist on one broker: the ledger above stays FIFO while the
thumbnails fan out — ordering is a property of each consumer, not of the bus.

## Retries and dead letters

Every consumer carries its own attempt budget (5 by default) and backoff
schedule (5 seconds doubling to 5 minutes), tunable per consumer:

```go
err := eventbus.Consume(ctx, broker, "sync-crm",
	func(ctx context.Context, event OrderPlaced) error {
		return crm.Upsert(ctx, event.OrderID)
	},
	eventbus.WithMaximumAttempts(8),
	eventbus.WithRetryDelay(func(attempt int) time.Duration {
		return time.Duration(attempt) * time.Second
	}))
```

An event that consumes its whole budget becomes a dead letter: parked,
inspectable, and revivable. The queue moves on without it.

```go
letters, err := broker.FindExhausted(ctx, 100)
for _, letter := range letters {
	slog.Warn("delivery exhausted",
		"consumer", letter.ListenerID, "cause", letter.LastError)
	revived, err := broker.Resubmit(ctx, letter.Reference)
	// revived == true: the event re-enters its queue with a fresh budget.
}
```

## The same code, durable and multi-node

Nothing above named an engine. To make the events survive restarts and reach
other nodes, construct the broker over sqlbus instead — the consumers and the
publishes stay exactly as written:

```go
import "github.com/cgardev/gokeel/sqlbus"

broker := sqlbus.NewBroker(bridge, publisher)

err := eventbus.Consume(ctx, broker, "send-confirmation",
	func(ctx context.Context, event OrderPlaced) error {
		return mailer.SendConfirmation(ctx, event.OrderID)
	})
```

The SQL engine adds what only durability can offer:

- **Publishing joins the business transaction.** A rollback takes the event
  with it; a commit guarantees eventual delivery. See
  [The SQL Bus](/gokeel/guides/sql-bus/) for the machinery underneath.
- **Consumers compete across nodes.** A FIFO consumer stays strictly serial
  cluster-wide: one event at a time, in publication order, no matter how many
  nodes host it. An unordered consumer shares its work across the cluster.
- **Broadcast delivery.** `eventbus.WithBroadcastDelivery()` requests one
  delivery per node instead of one per consumer — the natural shape for
  node-local concerns such as invalidating an in-memory cache. The memory
  engine treats it as regular consumption, because process and node coincide.

Ordered deliveries on the SQL engine wait below a watermark — the
materialization grace, 10 minutes by default and tunable with
`sqlbus.WithMaterializationGrace` — before they run, so a publication still
inside an open transaction can never slot in front of an already-delivered
successor. Order costs that latency; unordered consumers do not pay it.

## Choosing an engine

- **Memory** — events that only coordinate the current process: refreshing
  caches, notifying WebSocket sessions, decoupling modules in tests. Zero
  dependencies, zero infrastructure, exactly-once for the process lifetime,
  and everything is lost on shutdown by design.
- **SQL** — events that represent business facts: they must survive a crash,
  join the transaction that produced them, and reach every node. The cost is
  idempotent handlers and polling latency.

Route each event type through one broker. The contract is the same on both,
so promoting an event type from memory to durable is a constructor swap, not
a rewrite.

## Where to go next

The broker rides on two lower-level pieces that remain available on their own:
[The Event Bus](/gokeel/guides/event-bus/) is the synchronous, in-process
primitive the engines deliver through, and [The SQL
Bus](/gokeel/guides/sql-bus/) documents the durable machinery — claims,
leases, the FIFO watermark, retention, and multi-node operations. The full API
surface is in the [Broker reference](/gokeel/reference/broker/).
