---
title: The SQL Bus
description: What kind of bus sqlbus is, the ordering it does and does not guarantee, how many listeners it supports across nodes, and what happens when a delivery fails.
---

`sqlbus` extends the in-process event bus across application nodes, using a
shared SQL database — PostgreSQL or SQLite — as the only transport. An event
published on one node reaches listeners attached on any node of the cluster,
with no broker or message queue in the infrastructure: the database the
application already writes to is the bus.

It is the distributed sibling of the [transactional
outbox](/gokeel/guides/transactional-outbox/), and it makes the same core
promise in a wider scope: where the outbox guarantees that the listeners of one
process eventually handle an event, sqlbus guarantees that an event reaches its
listeners wherever in the cluster they run.

## What kind of bus it is

sqlbus is a **store-and-forward, at-least-once, claim-based bus**. Publishing
writes the event as one durable message row — inside the caller's business
transaction when a unit of work is active — and every node runs a `Dispatcher`
that materializes one delivery row per message and listener it hosts, claims
each delivery with a guarded update the database arbitrates, hands the event to
the local in-process bus, and settles the outcome.

Two consequences follow from that design:

- **Delivery is at-least-once, never exactly-once.** A crash between invoking a
  listener and recording its success leads to a redelivery once the claim lease
  expires. Listeners must be idempotent; the [outbox
  guide](/gokeel/guides/transactional-outbox/) makes the same demand for the
  same reason.
- **Latency differs by locality.** Listeners attached on the publishing node
  run synchronously right after the commit, exactly like outbox listeners.
  Listeners on other nodes are reached asynchronously by their dispatcher's
  next poll, so remote delivery arrives within roughly one poll interval
  (one second by default), or sooner when a wake signal is wired.

## Is it FIFO?

**Each listener chooses.** An ordered listener is a strict FIFO queue,
cluster-wide; an unordered listener trades order for throughput. The choice is
declared at attachment and arbitrated durably with first-attach-wins
semantics, so two nodes cannot silently disagree about it — the losing
attachment fails with an error wrapping `sqlbus.ErrConflictingOrdering`.

**Ordered listeners** attach with `WithOrderedDelivery()`:

```go
err := sqlbus.AttachCompetingListener(ctx, bridge, "ledger",
	func(ctx context.Context, event OrderPlaced) error {
		return ledger.Append(ctx, event.OrderID)
	},
	sqlbus.WithOrderedDelivery())
```

The listener processes its events strictly in publication order — the total
order is the pair (publication date, message identifier), deterministic across
the cluster — and strictly one at a time, no matter how many nodes host it.
Three mechanisms uphold the order:

- **Only the head of the queue is claimable.** A delivery with an earlier
  incomplete sibling waits, so a failing event blocks its successors while it
  retries: head-of-line blocking is what FIFO means under failure. An event
  that exhausts its budget parks as a dead letter and the queue continues.
- **Deliveries wait below a watermark.** An ordered delivery runs only once
  its message is older than the materialization grace, so a publication still
  sitting in an open transaction can never commit late and slot in front of an
  already-delivered successor. Order costs that latency — configure the grace
  as low as the longest publishing transaction allows.
- **Execution is serial even against operators.** A dead letter revived with
  `Resubmit` re-enters at its original position and waits until the delivery
  in flight settles or its claim lease expires; within a live lease, two
  deliveries of one ordered listener never run concurrently.

**Unordered listeners** — the default — keep the old behavior: due deliveries
are claimed oldest-first as a best effort, retries do not block the queue,
several nodes process concurrently, and the publishing node's local fast path
runs right after commit. Design unordered listeners so correctness does not
depend on order; at-least-once already forces them to be idempotent, and
order-independence is the natural companion.

## How many listeners can subscribe?

**Any number — two and far beyond.** Like the in-process bus, sqlbus
multicasts: every listener attached to an event type receives its own delivery
of every message, tracked as its own row with its own state machine. Two
listeners never share an outcome; one of them failing does not affect the
other's delivery.

What makes the distributed bus richer is that "two subscribers" can mean two
different things, and each is declared explicitly:

**Two different listeners** both receive every event, wherever they run:

```go
// On any node: both listeners get every OrderPlaced, independently.
err := sqlbus.AttachCompetingListener(ctx, bridge, "billing",
	func(ctx context.Context, event OrderPlaced) error {
		return billing.Invoice(ctx, event.OrderID)
	})
err = sqlbus.AttachCompetingListener(ctx, bridge, "analytics",
	func(ctx context.Context, event OrderPlaced) error {
		return analytics.Record(ctx, event.OrderID)
	})
```

**The same listener on two nodes** shares or fans out the work, depending on
its delivery mode:

- `AttachCompetingListener` registers the listener as one cluster-wide
  consumer: each event is handled **exactly once somewhere**, by whichever
  hosting node claims it first. This is the safe default for homogeneous
  replicas — scaling from one node to three must not send three invoices.
- `AttachBroadcastListener` registers one consumer per node: each event is
  handled **once on every hosting node**, which suits node-local concerns such
  as invalidating an in-memory cache.

The delivery mode is arbitrated in the database with first-attach-wins
semantics. Two nodes cannot silently run one `ListenerID` under different
modes: the losing attachment fails with an error wrapping
`sqlbus.ErrConflictingDeliveryMode`, so a misconfigured deployment announces
itself at startup instead of double-processing every event.

## What happens when a listener fails?

A failing listener affects exactly one delivery: its own. The other listeners
of the same event, and the other events of the same listener, proceed
untouched. The failed delivery then walks an explicit state machine:

1. **The failure is recorded.** The delivery moves to `FAILED`, carrying the
   listener's error text in its `last_error` column, so a stuck delivery is
   diagnosable with one `SELECT` rather than log archaeology. A panicking
   listener is recovered and treated as an ordinary failure — one exploding
   handler cannot take down the dispatcher, or the publishing caller.
2. **It is retried with backoff, by any hosting node.** The delivery becomes
   due again after a delay that doubles per attempt (5 seconds up to 5 minutes
   by default, tunable per bridge with `WithRetryDelay` or per listener with
   `WithListenerRetryDelay`), and whichever node hosting the listener claims
   it first runs the retry — a listener broken by one node's local conditions
   can succeed on another.
3. **After the attempt budget, it becomes a dead letter.** Once the configured
   attempts are spent (5 by default, `WithMaximumAttempts` per bridge or
   `WithListenerMaximumAttempts` per listener), the delivery moves
   to the terminal `EXHAUSTED` state and stops consuming resources. Dead
   letters pin their message in the store — the payload stays available — and
   are listed for the operator:

   ```go
   deadLetters, err := bridge.FindExhausted(ctx, 100)
   for _, dead := range deadLetters {
   	// dead.Delivery.LastError holds the final failure cause.
   	revived, err := bridge.Resubmit(ctx, dead.Delivery.Key)
   	// revived == true: the delivery re-enters the queue with a fresh budget.
   }
   ```

4. **A crashed node cannot strand a delivery.** A delivery claimed by a node
   that dies mid-processing stays protected only for the claim lease (5
   minutes by default, `WithLeaseDuration`); past it, any other hosting node
   steals the claim and redelivers. This recovery path is also why delivery is
   at-least-once: the crash may have happened after the listener ran but
   before its success was recorded.

Failures never fail the publisher. `Publisher.Publish` returns an error only
when the event cannot be stored; a listener rejecting the event afterwards is
reported through the logs and the delivery table, and handled by the retry
machinery rather than by the code path that produced the event.

## The Broker interface

Everything above is also reachable through the engine-independent contract
described in [The Broker](/gokeel/guides/broker/): `sqlbus.NewBroker(bridge,
publisher)` satisfies `eventbus.Broker`, so consumers written against the
contract run unchanged on the in-memory engine and on this one. The consumer
options map directly — FIFO is `WithOrderedDelivery`, broadcast is the
delivery mode, and the retry options become per-listener attachment options.

## Where to go next

The consumer contract this module implements — and the in-memory engine that
shares it — is described in [The Broker](/gokeel/guides/broker/). The
in-process half of the story — the bus sqlbus delivers through on each
node — is described in [The Event Bus](/gokeel/guides/event-bus/). For the
single-process ancestor of the same store-deliver-settle pattern, see
[The Transactional Outbox](/gokeel/guides/transactional-outbox/); route each
event type through one of the two, never both, or its listeners process it
twice. The schema both modules manage through the same migration seam is
covered in [Schema Migrations](/gokeel/guides/schema-migrations/).
