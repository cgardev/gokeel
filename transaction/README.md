# transaction

You know the dance. A use case needs to touch two tables that must change
together — place an order *and* reserve its stock — so you reach for a
transaction. With raw `database/sql` that means opening a `*sql.Tx`, handing it
down to every store method, and remembering to roll back on every single early
return. Miss one and you don't get a compile error; you get a transaction left
open, holding the database, until the driver reaps the connection.

This package deletes that bookkeeping. It's a **context-bound unit of work** —
Go's take on Spring's `@Transactional`, for a single database. You open a
transaction once, your stores pick it up from the `context.Context` on their own,
and nested calls join the same transaction. A use case that spans several stores
commits as one, and you never pass a `*sql.Tx` around again.

We'll build one running example the whole way through — placing an order in a
small online shop — and add one idea at a time until you've seen everything the
package does and *why* each piece exists.

---

## Where it sits (a little C4)

Before the code, one picture to place the package. A **use case** opens a
transaction with `Run`; each **store**, instead of taking a `*sql.Tx`, asks the
**Manager** for its executor with `Querier(ctx)`; the Manager owns the real
`*sql.Tx` over the one database. Because `Run` binds the transaction to the
context, a store's own inner `Run` *joins* the use case's transaction instead of
opening a second one.

```
 use case                              stores
 ┌──────────────────────┐              ┌───────────────────────────┐
 │ manager.Run(ctx, fn) │──── ctx ────▶│ store.Run(ctx, fn)         │
 │   begins ONE tx and  │  carries the │   sees the unit on ctx,    │
 │   binds it to ctx    │  unit of work│   JOINS it (no new tx)     │
 └──────────┬───────────┘              │ store.Querier(ctx)         │
            │                          │   → the live *sql.Tx       │
            ▼                          └─────────────┬─────────────┘
 ┌────────────────────────────────────────────────  ▼ ───────────┐
 │ transaction.Manager                                  │
 │   Run     : begin / join / commit / rollback                   │
 │   Querier : the *sql.Tx while a unit is active, else *sql.DB   │
 └────────────────────────────────┬──────────────────────────────┘
                                  ▼
                           ┌─────────────┐
                           │  database   │  one SQLite (or PostgreSQL)
                           └─────────────┘
```

Everything below is just that picture, one detail at a time.

---

## The problem, by hand

Let's be honest about the code you already write, because the whole reason this
package exists is to delete it. Placing an order inserts the `orders` row and
reserves stock for each line, and these two writes **must** land together — you
can never end up with an order whose stock was never reserved. So you `BeginTx`,
and right away the transaction leaks into your design: it has to reach the stores,
so it becomes a parameter.

```go
// Insert takes a *sql.Tx because the use case is the only one that knows a
// transaction is in flight. Same story for every other write method.
func (s OrderStore) Insert(ctx context.Context, tx *sql.Tx, order Order) error {
	_, err := tx.ExecContext(ctx,
		`INSERT INTO orders (id, customer_email, total_cents, placed_at) VALUES (?, ?, ?, ?)`,
		order.ID, order.CustomerEmail, order.TotalCents, order.PlacedAt.Format(time.RFC3339))
	return err
}

// PlaceOrder owns the whole transaction lifecycle by hand.
func PlaceOrder(ctx context.Context, db *sql.DB, orders OrderStore, inventory InventoryStore, order Order) error {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return err
	}
	if err := orders.Insert(ctx, tx, order); err != nil {
		_ = tx.Rollback() // remembered here...
		return err
	}
	for _, line := range order.Lines {
		if err := inventory.Reserve(ctx, tx, line.SKU, line.Quantity); err != nil {
			return err // ...and forgotten here: the transaction is left OPEN.
		}
	}
	return tx.Commit()
}
```

Spot the bug? The second early return — the one inside the loop — forgot
`tx.Rollback()`. It walks away leaving the transaction open, and on a
single-writer database that open writer blocks the *next* order. Every store
method carries a `*sql.Tx`, every error path has to remember to clean up, and the
compiler won't catch the one that doesn't. That forgotten line is the ache. The
rest of this README is the cure.

> **Remember** — by hand, the transaction becomes a parameter in every signature
> and *you* are its lifecycle owner; one forgotten rollback leaks it.

---

## The fix in two moves: Run and Querier

Here's the whole idea: stop carrying the transaction as a parameter and let it
become a fact about the `context.Context` you're already passing everywhere.
There are exactly two players.

1. The caller writes `manager.Run(ctx, func(ctx) error { … })` to **open** a unit
   of work.
2. The store writes `manager.Querier(ctx)` to **ask** what it should execute
   against — the live `*sql.Tx` when a unit of work is in flight, or the plain
   `*sql.DB` for an auto-commit statement when none is.

Here's the same `OrderStore`, rewritten. Notice what its `Insert` signature does
*not* contain (a `*sql.Tx`) and what you never call (`Commit` or `Rollback`):

```go
type OrderStore struct {
	transactions transaction.Transactor // just Run + Querier
}

func NewOrderStore(transactions transaction.Transactor) *OrderStore {
	return &OrderStore{transactions: transactions}
}

func (s *OrderStore) Insert(ctx context.Context, order Order) error {
	return s.transactions.Run(ctx, func(ctx context.Context) error {
		querier := s.transactions.Querier(ctx) // the live *sql.Tx, resolved from ctx
		_, err := querier.ExecContext(ctx,
			`INSERT INTO orders (id, customer_email, total_cents, placed_at) VALUES (?, ?, ?, ?)`,
			order.ID, order.CustomerEmail, order.TotalCents, order.PlacedAt.UTC().Format(time.RFC3339))
		return err // nil commits this transaction; a non-nil error rolls it back
	})
}
```

And you wire it once, injecting the Manager as the store's `Transactor`:

```go
manager := transaction.NewManager(database)
orders := NewOrderStore(manager)
```

The way your function *leaves* is the decision — there are exactly three exits:

```
work returns nil    → COMMIT
work returns error  → ROLLBACK   (the error is still handed back to you)
work panics         → ROLLBACK, then the panic is re-raised once it's safe
```

That last one matters: a panic rolls back and is re-raised only *after* the
transaction has settled, so a half-open transaction can never escape.

> **Remember** — the transaction rides the context, not your signatures. `Run`
> opens the unit of work, `Querier(ctx)` finds the executor, and your return value
> is the commit/rollback decision. You never call `Commit` or `Rollback`.

---

## How a store gets its connection

Here's the question that trips everyone up the first time: when your store runs an
`UPDATE`, what is it running it *against* — the database, or the open transaction?
In a lot of codebases the answer is "whichever `*sql.Tx` the caller remembered to
thread down," and that threading is exactly the misery we just deleted. Here it's
calmer: the store asks `Querier(ctx)`, and that one call hands back the live
`*sql.Tx` when a unit of work is in progress, or the plain `*sql.DB` (auto-commit)
when there isn't one.

So this `Reserve` has no `*sql.Tx` in sight, yet it does the right thing whether
you call it on its own *or* from inside another `Run`:

```go
func (s *InventoryStore) Reserve(ctx context.Context, sku string, quantity int64) error {
	return s.transactions.Run(ctx, func(ctx context.Context) error {
		// In a Run this is the live *sql.Tx; called bare it is the *sql.DB.
		result, err := s.transactions.Querier(ctx).ExecContext(ctx,
			`UPDATE inventory SET available = available - ? WHERE sku = ? AND available >= ?`,
			quantity, sku, quantity)
		if err != nil {
			return err
		}
		affected, err := result.RowsAffected()
		if err != nil {
			return err
		}
		if affected == 0 {
			return ErrOutOfStock // the WHERE matched no row: refusing to oversell
		}
		return nil
	})
}
```

A *read* is the same idea with the training wheels off — it doesn't even open a
`Run`, it just resolves the querier and reads on whatever the context carries:

```go
// Available reads on the open transaction when called inside one, the database otherwise.
func (s *InventoryStore) Available(ctx context.Context, sku string) (int64, error) {
	rows, err := s.transactions.Querier(ctx).QueryContext(ctx,
		`SELECT available FROM inventory WHERE sku = ?`, sku)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	if !rows.Next() {
		return 0, rows.Err() // no row → zero stock
	}
	var available int64
	return available, rows.Scan(&available)
}
```

Think of `Querier(ctx)` as Spring's `DataSourceUtils.getConnection`: you don't
*decide* whether you're in a transaction, you *ask*, and the framework already
knows.

> **Remember** — stores resolve their executor, they never thread it. The `Querier`
> interface is just `QueryContext` / `ExecContext`, so it's whatever a query
> builder is happy to receive.

---

## The payoff: many stores, one transaction

This is the moment the package pays off. Both `OrderStore.Insert` and
`InventoryStore.Reserve` are honestly written — each opens its own `Run`, so on
its own each one commits. Great in isolation, but `PlaceOrder` needs the order row
*and* the stock decrement to land together. If you're coming from Spring, the
worry bites here: "if each store starts its own transaction, won't I get two
commits?"

No — and this is the heart of it. The transaction lives on the context, so when
the use case opens **one** outer `Run` and calls both stores inside it, their
inner `Run`s see the unit of work already there and simply **join** it. (That's
the default propagation, `Required`.) Nobody passes a `*sql.Tx`.

```go
func (uc *PlaceOrder) Do(ctx context.Context, order Order) error {
	return uc.manager.Run(ctx, func(ctx context.Context) error {
		if err := uc.orders.Insert(ctx, order); err != nil {
			return err // rolls back: no inventory was touched
		}
		for _, line := range order.Lines {
			if err := uc.inventory.Reserve(ctx, line.SKU, line.Quantity); err != nil {
				return err // rolls back the WHOLE transaction, including the order
			}
		}
		return nil // commits order + every reservation together
	})
}
```

```
PlaceOrder.Do
└─ manager.Run            BEGIN  (one transaction, bound to ctx)
   ├─ orders.Insert
   │   └─ Run  ──────────▶ JOINS  (no BEGIN, no COMMIT)
   ├─ inventory.Reserve
   │   └─ Run  ──────────▶ JOINS  (no BEGIN, no COMMIT)
   └─ return nil          COMMIT  (order + reservations together)
        return error      ROLLBACK (both undone)
```

> **Remember** — one outer `Run` turns several self-transactional store calls into
> a single atomic transaction. The inner `Run`s join it instead of starting their
> own.

---

## When things go wrong

Here's the bug every hand-rolled transaction eventually grows: deep in
`PlaceOrder`, a step fails, you log it, *swallow the error* so the flow reads
nicely, and carry on — and now you've committed an order whose stock was never
reserved. With this package you can't accidentally do that, and that's the whole
point of this section.

When `Reserve` runs inside the outer `Run`, it's a *joining* call. The moment its
work returns a rolling-back error, the package marks the shared unit of work
**rollback-only** before handing the error back to you. So even if you swallow it,
the outermost `Run` refuses to commit: it rolls back and returns `ErrRollbackOnly`
to the top.

```go
return manager.Run(ctx, func(ctx context.Context) error {
	if err := orders.Insert(ctx, order); err != nil {
		return err
	}
	for _, line := range order.Lines {
		err := inventory.Reserve(ctx, line.SKU, line.Quantity)
		if errors.Is(err, ErrOutOfStock) {
			// Tempting: swallow it and "let the rest go through".
			// It will NOT go through — the unit is already doomed.
			slog.Warn("line out of stock, skipping", "sku", line.SKU)
			continue
		}
		if err != nil {
			return err
		}
	}
	return nil // asks for a commit...
})
// ...but if any Reserve hit ErrOutOfStock, Run rolled back and returns
// ErrRollbackOnly instead. The order row never reaches disk.
```

On the caller side you can tell the safety net apart from a genuine error:

```go
switch err := placeOrder(...); {
case errors.Is(err, transaction.ErrRollbackOnly):
	slog.Info("order rolled back: a line could not be reserved")
case err != nil:
	slog.Error("order failed", "error", err)
default:
	slog.Info("order placed")
}
```

It's Spring's `globalRollbackOnly`: once any participant dooms the unit, no later
code can un-doom it.

> **Remember** — returning an error (or panicking) *is* the rollback decision. A
> joining call's fatal error dooms the whole unit, so swallowing it still yields a
> rollback and `ErrRollbackOnly`, never a half-committed order.

---

## Propagation

You've now *used* the default propagation without naming it: "join an active
transaction, or begin one." That's `Required`. Propagation is simply your answer
to the question **"what should I do if there's already a transaction running?"**

```go
manager.Run(ctx, work, transaction.WithPropagation(transaction.Nested))
```

| Propagation             | A transaction is already running     | None is running                    |
| ----------------------- | ------------------------------------ | ---------------------------------- |
| `Required` *(default)*  | join it                              | begin a new one                    |
| `Supports`              | join it                              | run without a transaction          |
| `Mandatory`             | join it                              | fail with `ErrTransactionRequired` |
| `Never`                 | fail with `ErrTransactionNotAllowed` | run without a transaction          |
| `Nested`                | take a **savepoint** of it           | begin a new one                    |

`Mandatory` is handy for a low-level helper that must never run outside a use case
(it fails fast instead of silently auto-committing). `Supports` / `Never` suit a
read that should work with or without an ambient transaction. `Nested` gets its
own section next.

And one honest absence: you won't find `REQUIRES_NEW` or `NOT_SUPPORTED` here.
They'd need a *second* transaction running at the same time, and on a single-writer
database that second one would deadlock against the write lock the first already
holds. Leaving them out is a deliberate fit to the datasource, not an oversight —
more in [Scope](#scope--design-notes).

> **Remember** — `Required` (join-or-begin) is the default and what you want
> almost always. The rest are for the cases where "already in a transaction?"
> needs a different answer.

---

## The optional step (Nested)

Every store call so far has been all-or-nothing: under `Required`, any error rolls
the whole unit of work back. That's exactly right for the order and the stock
decrement. But now the customer ticks "gift wrap this," and that's a different
kind of work — a nice-to-have. If the wrap desk is out of ribbon, you still want
the order to go through, just without the bow.

With plain `Required`, an error from the wrap step would drag the order down with
it. This is the one moment where a failure must be *contained*, and that's what
`Nested` is for: it takes a **savepoint** before the sub-step, so a rolling-back
error inside it only undoes back to that savepoint — the outer transaction keeps
living.

```go
func (s *GiftWrapStore) Wrap(ctx context.Context, orderID string, wrap GiftWrap) error {
	return s.transactions.Run(ctx, func(ctx context.Context) error {
		if wrap.Style == "" { // out of ribbon, desk closed, ...
			return ErrWrapUnavailable
		}
		_, err := s.transactions.Querier(ctx).ExecContext(ctx,
			`INSERT INTO gift_wraps (order_id, style, price_cents) VALUES (?, ?, ?)`,
			orderID, wrap.Style, wrap.PriceCents)
		return err
	}, transaction.WithPropagation(transaction.Nested))
}
```

Here's the part that trips everyone up: **nesting changes what gets rolled back,
not what gets returned.** The inner `Run` still hands you `ErrWrapUnavailable`; the
savepoint just means the outer transaction is still healthy. So you catch that
error and let `PlaceOrder` carry on — if you propagated it, you'd abort the very
order you were trying to save:

```go
if wrap != nil {
	if err := giftWraps.Wrap(ctx, order.ID, *wrap); err != nil &&
		!errors.Is(err, ErrWrapUnavailable) {
		return err // an *unexpected* wrap failure still aborts the order
	}
}
return nil // order + inventory commit; the gift wrap was quietly skipped
```

```
Run (Required) ── BEGIN ─────────────────────────── COMMIT  (order + inventory survive)
  ├─ orders.Insert
  ├─ inventory.Reserve
  └─ giftWraps.Wrap (Nested) ─ SAVEPOINT ─ err ─ ROLLBACK TO SAVEPOINT
                                                 └─ returns ErrWrapUnavailable (you swallow it)
```

> **Remember** — `Nested` is for an optional sub-step: its failure rolls back only
> to a savepoint and the outer transaction still commits — but the inner `Run`
> still returns the error, so catch it.

---

## Tuning a transaction

With propagation as the anchor, the rest of `@Transactional`'s attributes fall out
as a small menu of options:

| Option                 | What it does                                                          |
| ---------------------- | -------------------------------------------------------------------- |
| `WithIsolation(level)` | isolation level for a brand-new transaction                          |
| `ReadOnly()`           | begin the transaction read-only                                      |
| `WithTimeout(d)`       | cancel the transaction's context after `d`; on expiry the error wraps `ErrTransactionTimedOut`. A negative `d` fails with `ErrInvalidTimeout` |
| `WithName(name)`       | label the unit of work, handy for logging                            |

```go
// Don't let one slow order hold the single writer forever.
return manager.Run(ctx, placeOrderWork,
	transaction.WithName("place-order"),
	transaction.WithTimeout(2*time.Second))
```

One gotcha worth internalizing: `WithIsolation`, `ReadOnly`, and `WithTimeout`
only matter for a **brand-new** transaction — the outermost `Run` that actually
begins one. They do nothing when the call *joins* an existing transaction (you
can't change the isolation of a transaction that's already running). The exception
is the negative-`WithTimeout` check, which is a programming error and fails fast
regardless. And if you join with an isolation or read-only request the running
transaction can't honor, you get `ErrIncompatibleJoin`.

> **Remember** — options configure the transaction that *begins*; a joining `Run`
> inherits whatever the outermost one decided.

---

## Rollback rules: committing despite an error

You know the default: return an error, `Run` rolls back. That's exactly right for
the order and the stock. But real use cases also touch things that simply aren't
worth aborting a paid order over. Awarding loyalty points, say: if the loyalty
service is down, the order is already valid and the stock is already reserved, so
throwing the whole transaction away would be the wrong cure.

This is Spring's `@Transactional(noRollbackFor = …)`. You tell `Run` "this error
isn't fatal, commit anyway" — and the error *still* comes back to you, so the
caller can log it or retry later:

```go
return manager.Run(ctx, func(ctx context.Context) error {
	// ... write the order, reserve stock (these failing IS fatal) ...
	return loyalty.Award(ctx, order.CustomerEmail, order.TotalCents) // best-effort
},
	// Commit the order even if loyalty points couldn't be awarded...
	transaction.NoRollbackForError(ErrLoyaltyServiceDown),
	// ...but a fraud flag wins and forces a rollback, even though it
	// also matches ErrLoyaltyServiceDown above.
	transaction.RollbackForError(ErrLoyaltyFraud),
)
```

```
error returned by work
   ├─ matches a RollbackForError rule?   ── yes ─▶ ROLLBACK  (wins)
   ├─ matches a NoRollbackForError rule? ── yes ─▶ COMMIT  (error still returned)
   └─ otherwise ───────────────────────────────▶ ROLLBACK  (default)
```

There are predicate forms too (`NoRollbackForFunc`, `RollbackForFunc`) when a
sentinel isn't enough.

> **Remember** — `NoRollbackForError` commits despite a returned error (you still
> get the error back); `RollbackForError` forces a rollback and wins when both
> match.

---

## Hooking into the lifecycle

Sometimes you want to do something *exactly* when the transaction settles. Picture
the life of one `Run` as a platform the train passes through, in order:

```
work() → [before-commit: may VETO] → [before-completion]
                                          │
                              ┌───────────┴───────────┐
                          COMMIT                   ROLLBACK
                              │                       │
                       [after-commit]                 │   (after-commit skipped)
                              └───────────┬───────────┘
                                  [after-completion]  ← always, gets the Status
```

The workhorse is **after-commit**. Here's a bug it prevents: your `PlaceOrder`
commits, then sends the "Your order is confirmed" email. Send it *inside* the
`Run` and a failed commit means you emailed a customer about an order that never
existed. Send it right *after* `Run` returns and a crash in that hair-thin gap
loses the email with no record it was owed. The email (and the warehouse pick job)
must happen exactly once, only once the rows are truly on disk — that's
`RegisterAfterCommit`:

```go
return s.transactions.Run(ctx, func(ctx context.Context) error {
	if err := s.orders.Insert(ctx, order); err != nil {
		return err
	}
	// ... reserve stock ...

	scheduled := transaction.RegisterAfterCommit(ctx, func(ctx context.Context) {
		// The tx is DETACHED from this ctx, so DB work here auto-commits against
		// the *sql.DB. A panic here is recovered and logged — a committed order
		// must never look failed to the caller.
		_ = s.notifier.SendConfirmation(ctx, order.CustomerEmail, order.ID)
		_ = s.notifier.EnqueuePick(ctx, order.ID)
	})
	if !scheduled {
		// No unit of work was active (the auto-commit path): nothing to wait for,
		// so do it now. Register* returns false here — don't forget this branch.
		_ = s.notifier.SendConfirmation(ctx, order.CustomerEmail, order.ID)
		_ = s.notifier.EnqueuePick(ctx, order.ID)
	}
	return nil
}, transaction.WithName("place-order"))
```

The other phases solve what after-commit can't:

- **`RegisterBeforeCommit`** runs inside the still-open transaction, your last
  chance to inspect the pending writes and *veto* — return an error and the whole
  thing rolls back, exactly as if your work had failed.
- **`RegisterAfterCompletion`** runs on commit *and* rollback, handed the final
  `Status` — the right place to record "settled as Committed / RolledBack", never
  the place to send the email (it fires on failure too).

```go
transaction.RegisterBeforeCommit(ctx, func(ctx context.Context) error {
	return orderTotalsBalance(ctx) // non-nil vetoes the commit
}, transaction.WithOrder(-1)) // lower order runs first within a phase

transaction.RegisterAfterCompletion(ctx,
	func(ctx context.Context, status transaction.Status) {
		slog.Info("order transaction settled", "outcome", status.String())
	})
```

`Nested` propagation has its own pair, `RegisterSavepoint` /
`RegisterSavepointRollback`, that fire around a savepoint.

> **Remember** — `before-commit` is your last veto, `after-commit` fires only on
> success (and after a detached, panic-safe boundary), `after-completion` always
> runs with the final `Status`, and `WithOrder` (not registration order) sequences
> a phase.

---

## Getting a value back

`Run`'s work function only returns an `error`, which is a little stingy when you
want the `Order` you built — with its generated ID and timestamp — handed back to
the HTTP layer. The tempting move is to declare an `Order` outside the closure and
assign it inside. Resist it: if the transaction rolls back, that variable holds a
half-built order for a row that never landed.

Use the honest tool, `RunResult[T]` — a free function (Go methods can't be
generic) that runs exactly like `Run` but lets your work return `(T, error)`:

```go
return transaction.RunResult(ctx, transactions,
	func(ctx context.Context) (Order, error) {
		order := draft
		order.ID = uuid.NewString()
		order.PlacedAt = time.Now().UTC()
		if err := orders.Insert(ctx, order); err != nil {
			return Order{}, err // rolled back; the returned Order is the zero value
		}
		for _, line := range order.Lines {
			if err := inventory.Reserve(ctx, line.SKU, line.Quantity); err != nil {
				return Order{}, fmt.Errorf("reserve %s: %w", line.SKU, err)
			}
		}
		return order, nil
	},
	transaction.WithName("place-order"))
```

> **Remember** — need a value out of a unit of work? Reach for `RunResult[T]`
> instead of `Run`, and only trust the value when the error is `nil`.

---

## Peeking and steering

Your inner store calls now join the outer transaction — but how does code deep in
that call tree *know* which situation it's in? `StatusFromContext` is the read-only
mirror of Spring's `TransactionStatus`. Hand it a context and it tells you whether
a unit of work is bound to it and, if so, its name and whether *this* call began
the transaction or merely joined one:

```go
func logOrderStep(ctx context.Context, step string) {
	status, active := transaction.StatusFromContext(ctx)
	if !active {
		slog.Info("order step (no transaction)", "step", step) // auto-commit path
		return
	}
	verb := "joining"
	if status.IsNewTransaction() {
		verb = "starting"
	}
	slog.Info("order step", "step", step, "transaction", status.Name(), "phase", verb)
}
```

The bool is the whole point: `false` means "no transaction here," so check it
before you trust anything on the status. Other handles: `status.IsReadOnly()`,
`status.IsCompleted()`, `status.HasSavepoint()`, and the free functions
`CurrentTransactionName(ctx)` (for logging code that doesn't hold a status) and
`MarkRollbackOnly(ctx)` — which lets a fraud check deep in the call tree *doom* the
transaction without unwinding an error up every layer. The outermost `Run` then
returns `ErrRollbackOnly`.

> **Remember** — `StatusFromContext` reads the live unit of work; check its bool
> first. `MarkRollbackOnly` vetoes the commit from anywhere, no error-threading
> required.

---

## Watching every transaction

Once you've placed orders for a while, you'll want to *see* them — how long each
held the writer, how many committed versus rolled back, a trace span each. Your
instinct, fresh off the last section, is `RegisterAfterCommit`. Wrong tool, and
here's the distinction worth tattooing on your wrist:

- **Synchronizations** (the `Register*` family) **participate**: registered from
  inside the work, they belong to one unit of work, and they can do business side
  effects.
- An **`ExecutionListener`** **observes**: handed to `NewManager` once at wiring
  time, stateless, firing around the *physical* begin/commit/rollback of every
  transaction the Manager drives — knowing nothing about the order being placed.

```go
observer := transaction.ExecutionListener{
	BeforeBegin: func(ctx context.Context, status transaction.TransactionStatus) {
		slog.InfoContext(ctx, "transaction begin", "name", status.Name())
	},
	AfterCommit: func(ctx context.Context, status transaction.TransactionStatus, err error) {
		metrics.RecordOutcome(status.Name(), "committed", err)
	},
	AfterRollback: func(ctx context.Context, status transaction.TransactionStatus, err error) {
		metrics.RecordOutcome(status.Name(), "rolled_back", err)
	},
}
manager := transaction.NewManager(database, observer)
```

That "once, at construction" is the point: you don't sprinkle logging into every
use case; you attach one listener and every transaction is measured for free. Two
consequences newcomers trip on: a listener fires only for a *physically* begun
transaction, so an inner `Run` that JOINS (or a `Nested` savepoint step) produces
no callback — one event per real transaction, not per `Run`. And a panic in a
listener is recovered, never propagated — a committed order must not look failed
because your metrics counter blew up (this diverges from Spring, where a listener
exception escapes).

```
Manager (1 listener, attached at NewManager)
└─ Run "place-order"  ── physical BEGIN ──▶ BeforeBegin fires
     ├─ orders.Insert    (inner Run JOINS) ── no listener fire
     ├─ inventory.Reserve(inner Run JOINS) ── no listener fire
     └─ COMMIT / ROLLBACK ────────────────▶ AfterCommit / AfterRollback fires
```

> **Remember** — watch with listeners (once, at the Manager); take part with
> synchronizations (per unit of work). Listeners never fire for a join or a
> savepoint.

---

## Errors, with names

The reassuring part of error handling here: you never squint at a driver error or
compare message strings. Every way a `Run` can fail has a name, and you tell them
apart with `errors.Is`. The wrapping ones keep the original driver error reachable
underneath (via `errors.As`).

| Error                      | You get it when                                                              |
| -------------------------- | --------------------------------------------------------------------------- |
| `ErrRollbackOnly`          | a joined call doomed the unit, even if its error was swallowed              |
| `ErrTransactionRequired`   | `Mandatory` propagation with no transaction running                         |
| `ErrTransactionNotAllowed` | `Never` propagation while a transaction is running                          |
| `ErrIncompatibleJoin`      | you joined with an isolation/read-only request the running tx can't honor   |
| `ErrInvalidTimeout`        | `WithTimeout` got a negative duration (caught before any work runs)         |
| `ErrTransactionTimedOut`   | the transaction blew its own deadline and rolled back (wraps the work error)|
| `ErrBeginFailed`           | the transaction couldn't be opened (wraps the driver error)                 |
| `ErrTransactionSystem`     | a commit, rollback, or savepoint step failed (wraps the driver error)       |

The two clock-related ones are worth a closer look. `ErrTransactionTimedOut` is a
*stable* target: it matches whether the driver reported "context canceled",
"context deadline exceeded", or anything else, so one `errors.Is` works across
SQLite and PostgreSQL. And a cancellation of *your own* upstream context is
deliberately **not** reported as a transaction timeout, so the check stays
meaningful. A worked example:

```go
err := manager.Run(ctx, placeOrderWork,
	transaction.WithName("place-order"),
	transaction.WithTimeout(2*time.Second))

switch {
case err == nil:
	return nil
case errors.Is(err, transaction.ErrInvalidTimeout):
	return fmt.Errorf("place-order is misconfigured: %w", err) // a wiring bug
case errors.Is(err, transaction.ErrTransactionTimedOut):
	return fmt.Errorf("place-order timed out and was rolled back: %w", err)
case errors.Is(err, transaction.ErrTransactionSystem):
	return fmt.Errorf("place-order hit a database system failure: %w", err)
case errors.Is(err, transaction.ErrRollbackOnly):
	return fmt.Errorf("place-order was vetoed and rolled back: %w", err)
default:
	return err // a plain business error, e.g. ErrOutOfStock — handle on its merits
}
```

> **Remember** — every `Run` failure is a named sentinel; match it with
> `errors.Is`. `ErrInvalidTimeout` is a config bug caught early;
> `ErrTransactionTimedOut` is the runtime deadline, stable across drivers.

---

## Scope & design notes

A few things worth knowing about the boundaries:

- **One database, one transaction at a time.** The main backend is SQLite (a
  single writer); it's also tested against PostgreSQL. The propagations that
  suspend a transaction or open a second concurrent one (`REQUIRES_NEW`,
  `NOT_SUPPORTED`) are left out on purpose — on a single writer they'd deadlock.
- **Context, not thread-locals.** The unit of work lives on the `context.Context`,
  not a thread-local like in Spring. A `*sql.Tx` can't be shared across goroutines,
  so the deal is: `work` and its callbacks run on the goroutine that called `Run`.
- **A panic won't leak a transaction.** If something panics while a transaction is
  open, it rolls back first and the panic is re-raised only once everything has
  settled — no connection left dangling.
- **`database/sql` and nothing more.** The package knows nothing about whatever
  query builder your stores use; `Querier` is just the minimal `QueryContext` /
  `ExecContext` surface a builder is happy with.

### If you come from Spring

You'll feel at home: `Manager.Run` ↔ `@Transactional`, the same propagation
behaviors, the same rollback rules, the four synchronization phases,
`ExecutionListener` ↔ `TransactionExecutionListener`, and typed errors. Where it
differs — and exactly why — is catalogued in
[`SPRING_PARITY_AUDIT.md`](./SPRING_PARITY_AUDIT.md).
