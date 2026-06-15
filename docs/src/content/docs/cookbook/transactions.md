---
title: Transactional Use Cases
description: Recipes for running a unit of work, returning a value, joining an outer transaction, rollback-only, after-commit side effects, timeouts, and isolation levels.
---

## Run a unit of work

You want several writes to commit together, or not at all.

```go
err := manager.Run(ctx, func(ctx context.Context) error {
    if err := orders.Insert(ctx, order); err != nil {
        return err // a non-nil error rolls the transaction back
    }
    return inventory.Reserve(ctx, order.SKU, order.Quantity)
}) // returning nil commits both writes together
```

Gotcha: the return value is the decision, so you never call `Commit` or
`Rollback` yourself; returning an error or panicking rolls back.

## Return a value with RunResult

You want the unit of work to hand back the order it built, not just an error.

```go
order, err := transaction.RunResult(ctx, manager,
    func(ctx context.Context) (Order, error) {
        order := draft
        order.ID = uuid.NewString()
        if err := orders.Insert(ctx, order); err != nil {
            return Order{}, err // rolled back; the returned Order is the zero value
        }
        return order, nil
    })
```

Gotcha: `RunResult[T]` is a free function (Go methods cannot be generic); trust
the returned value only when `err` is nil, since a rollback yields the zero value.

## Nested service calls that join the outer transaction

You want one use case to span several stores and commit as a single transaction.

```go
err := manager.Run(ctx, func(ctx context.Context) error {
    if err := orders.Insert(ctx, order); err != nil { // its own inner Run JOINS this one
        return err
    }
    return inventory.Reserve(ctx, order.SKU, order.Quantity) // also JOINS, no second BEGIN
})
```

Gotcha: the default propagation is `Required`, so an inner `Run` joins the
active transaction; the store resolves its executor with `manager.Querier(ctx)`,
never a threaded `*sql.Tx`.

## Mark the current transaction rollback-only

You want a fraud check deep in the call tree to doom the transaction without
threading an error up every layer.

```go
func reject(ctx context.Context) {
    if marked := transaction.MarkRollbackOnly(ctx); !marked {
        // false means no unit of work was active on this context
        slog.Warn("no transaction to mark rollback-only")
    }
}
```

Gotcha: marking only steers the outcome; the outermost `Run` still rolls back
and returns `transaction.ErrRollbackOnly` even when `work` returned nil. From
within `work` you can equivalently call `status.SetRollbackOnly()` on a
`TransactionStatus` obtained via `transaction.StatusFromContext(ctx)`.

## Schedule an after-commit side effect

You want to send the confirmation email exactly once, only after the rows are
durably on disk.

```go
err := manager.Run(ctx, func(ctx context.Context) error {
    if err := orders.Insert(ctx, order); err != nil {
        return err
    }
    scheduled := transaction.RegisterAfterCommit(ctx, func(ctx context.Context) {
        _ = notifier.SendConfirmation(ctx, order.CustomerEmail, order.ID)
    })
    if !scheduled {
        // No unit of work was active (the auto-commit path): do it now.
        _ = notifier.SendConfirmation(ctx, order.CustomerEmail, order.ID)
    }
    return nil
})
```

Gotcha: `RegisterAfterCommit` reports `false` on the auto-commit path, so handle
that branch; the callback runs on a context whose transaction is detached, and a
panic in it is recovered and logged rather than failing the committed transaction.

## Set a timeout

You want one slow order to stop holding the single writer indefinitely.

```go
err := manager.Run(ctx, placeOrderWork,
    transaction.WithName("place-order"),
    transaction.WithTimeout(2*time.Second))

if errors.Is(err, transaction.ErrTransactionTimedOut) {
    // the transaction blew its own deadline and was rolled back
}
```

Gotcha: a negative duration fails fast with `transaction.ErrInvalidTimeout`
before any work runs; on expiry the returned error wraps
`transaction.ErrTransactionTimedOut`, stable across SQLite and PostgreSQL.

## Choose an isolation level

You want a brand-new transaction to run at serializable isolation.

```go
err := manager.Run(ctx, transferFunds,
    transaction.WithIsolation(sql.LevelSerializable))
```

Gotcha: `WithIsolation` (like `ReadOnly` and `WithTimeout`) only affects the
outermost `Run` that actually begins a transaction; requesting a different
isolation while joining an active one fails with `transaction.ErrIncompatibleJoin`.

---

For the full walk-through of propagation, rollback rules, and the
synchronization phases, see [Propagation & Synchronizations](/gokeel/guides/propagation-and-synchronizations/)
and the [Transaction Manager](/gokeel/reference/transaction-manager/) reference.
