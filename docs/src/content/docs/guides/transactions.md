---
title: Transactions
description: Run a context-bound unit of work with the Manager, resolve the querier from the context, and let the return value drive the commit or rollback.
---

A transaction in gokeel is a context-bound unit of work. A `Manager` owns the
database transaction lifecycle, and your stores resolve the querier they execute
against from the `context.Context` instead of receiving a `*sql.Tx` through their
signatures. You open a transaction once, the work inside it commits or rolls back
as one, and you never thread a transaction handle by hand.

## The Manager

A `Manager` begins, commits, and rolls back the transactions its units of work
run in. Construct one over an open `*sql.DB` with `NewManager`:

```go
import "github.com/cgardev/gokeel/transaction"

manager := transaction.NewManager(database)
```

`NewManager` also accepts optional `ExecutionListener` values that observe the
begin, commit, and rollback of every new transaction the Manager drives. A
Manager is immutable after construction and safe for concurrent use:

```go
func NewManager(database *sql.DB, listeners ...ExecutionListener) *Manager
```

## Running a unit of work

`Run` executes a function as a unit of work. With no options it begins a
transaction (or joins one already bound to the context), runs the function, and
settles the transaction based on how the function returns:

```go
err := manager.Run(ctx, func(ctx context.Context) error {
	// ... read and write through the resolved querier ...
	return nil
})
```

The signature threads the context through to your function so the transaction
travels with it:

```go
func (manager *Manager) Run(
	ctx context.Context,
	work func(ctx context.Context) error,
	opts ...Option,
) error
```

The way your function returns is the decision. There are exactly three exits:

```text
work returns nil    → COMMIT
work returns error  → ROLLBACK   (the error is still returned to you)
work panics         → ROLLBACK, then the panic is re-raised once it is safe
```

A panic rolls back and is re-raised only after the transaction has settled, so a
half-open transaction can never escape. You never call `Commit` or `Rollback`
yourself.

## Resolving the querier

Inside the unit of work, a store does not decide whether a transaction is in
flight — it asks. `Querier(ctx)` returns the executor for the current context:
the live `*sql.Tx` when a unit of work is in progress, or the plain `*sql.DB` for
an auto-commit statement when none is.

```go
func (s *OrderStore) Insert(ctx context.Context, order Order) error {
	return s.transactions.Run(ctx, func(ctx context.Context) error {
		querier := s.transactions.Querier(ctx) // the live *sql.Tx, resolved from ctx
		_, err := querier.ExecContext(ctx,
			`INSERT INTO orders (id, customer_email, total_cents) VALUES (?, ?, ?)`,
			order.ID, order.CustomerEmail, order.TotalCents)
		return err // nil commits this transaction; an error rolls it back
	})
}
```

A read is the same idea without opening a `Run`: it resolves the querier and
reads on whatever the context carries.

```go
func (s *OrderStore) Total(ctx context.Context, id string) (int64, error) {
	rows, err := s.transactions.Querier(ctx).QueryContext(ctx,
		`SELECT total_cents FROM orders WHERE id = ?`, id)
	if err != nil {
		return 0, err
	}
	defer rows.Close()
	if !rows.Next() {
		return 0, rows.Err()
	}
	var total int64
	return total, rows.Scan(&total)
}
```

## The Querier interface

`Querier` is the minimal execution surface a store runs its statements against.
It is satisfied by `*sql.DB`, `*sql.Tx`, and `*sql.Conn`, so the resolved value
can be passed straight to a query builder:

```go
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}
```

Because the interface is exactly `QueryContext` and `ExecContext`, the value
`Querier(ctx)` hands back is whatever a query builder is happy to receive,
without this package importing the builder.

## Depending on the Transactor

A store rarely needs the whole `Manager`. It needs the slice of it that runs a
unit of work and resolves the querier — the `Transactor` interface, which
`*Manager` satisfies:

```go
type Transactor interface {
	Run(ctx context.Context, work func(ctx context.Context) error, opts ...Option) error
	Querier(ctx context.Context) Querier
}
```

Depend on `Transactor` in your stores and inject the Manager once at wiring time:

```go
type OrderStore struct {
	transactions transaction.Transactor // just Run + Querier
}

func NewOrderStore(transactions transaction.Transactor) *OrderStore {
	return &OrderStore{transactions: transactions}
}

manager := transaction.NewManager(database)
orders := NewOrderStore(manager)
```

## Many stores, one transaction

The transaction lives on the context, so when a use case opens one outer `Run`
and calls several stores inside it, their inner `Run`s see the unit of work
already there and join it. No store passes a `*sql.Tx`, and the whole sequence
commits — or rolls back — together.

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
		return nil // commits the order and every reservation together
	})
}
```

```text
PlaceOrder.Do
└─ manager.Run            BEGIN  (one transaction, bound to ctx)
   ├─ orders.Insert
   │   └─ Run  ──────────▶ JOINS  (no BEGIN, no COMMIT)
   ├─ inventory.Reserve
   │   └─ Run  ──────────▶ JOINS  (no BEGIN, no COMMIT)
   └─ return nil          COMMIT  (order + reservations together)
        return error      ROLLBACK (both undone)
```

When a joining call returns an error the rollback rules treat as fatal, the
shared unit is marked rollback-only before the error is handed back. Even if a
caller swallows that error, the outermost `Run` refuses to commit: it rolls back
and returns `transaction.ErrRollbackOnly` to the top.

## Returning a value

`Run`'s function returns only an `error`, which is limiting when you need the
value the work produced — a generated identifier, say. Reach for `RunResult[T]`,
a free function (Go methods cannot be generic) that runs exactly like `Run` but
lets the work return `(T, error)`:

```go
order, err := transaction.RunResult(ctx, manager,
	func(ctx context.Context) (Order, error) {
		built := draft
		built.ID = uuid.NewString()
		if err := orders.Insert(ctx, built); err != nil {
			return Order{}, err // rolled back; the returned Order is the zero value
		}
		return built, nil
	})
```

The value the work produced is returned alongside the error `Run` reports. On a
failed or rolled-back transaction, consult the error rather than the value:

```go
func RunResult[T any](
	ctx context.Context,
	transactor Transactor,
	work func(ctx context.Context) (T, error),
	opts ...Option,
) (T, error)
```

## Tuning a Run

`Run` and `RunResult` accept the same `Option` values, which configure the
transaction that begins:

| Option | What it does |
| --- | --- |
| `WithPropagation(p)` | how the call relates to an active transaction |
| `WithIsolation(level)` | isolation level for a brand-new transaction |
| `ReadOnly()` | begin the transaction read-only |
| `WithTimeout(d)` | cancel the transaction's context after `d`; on expiry the error wraps `ErrTransactionTimedOut`. A negative `d` fails with `ErrInvalidTimeout` |
| `WithName(name)` | label the unit of work, handy for logging |

```go
err := manager.Run(ctx, placeOrderWork,
	transaction.WithName("place-order"),
	transaction.WithTimeout(2*time.Second))
```

Options configure the transaction that begins; a `Run` that joins an existing
transaction inherits whatever the outermost one decided.

See [Propagation & Synchronizations](/gokeel/guides/propagation-and-synchronizations/)
for how a call relates to an active transaction and how to hook into the commit
and rollback lifecycle.
