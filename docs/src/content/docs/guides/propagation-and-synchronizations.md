---
title: Propagation & Synchronizations
description: Select how a Run relates to an active transaction, tune the transaction it begins, and hook into the commit and rollback phases.
---

A unit of work is opened with `Manager.Run`, and its behavior is shaped by the
`Option` functions you pass it. Propagation answers "what should this `Run` do if
a transaction is already bound to the context?", the construction options tune the
transaction it begins, and the synchronization phases let work hook into the
moment it settles. Everything on this page lives in
`github.com/cgardev/gokeel/transaction`.

## Propagation

`WithPropagation` selects the propagation behavior. The default is `Required`:

```go
manager.Run(ctx, work, transaction.WithPropagation(transaction.Nested))
```

There are five behaviors, each defined as a `Propagation` constant. They differ
only in how a `Run` reacts to an active transaction:

```go
transaction.Required  // join an active transaction, or begin one (default)
transaction.Supports  // join one if present, otherwise run without a transaction
transaction.Mandatory // join one, or fail with ErrTransactionRequired
transaction.Never     // fail with ErrTransactionNotAllowed when one is active
transaction.Nested    // take a savepoint of one, or begin a transaction
```

`Required` is the propagation almost every caller wants: an inner `Run` joins the
transaction the outer `Run` bound to the context, so several stores commit as one.
`Mandatory` suits a helper that must never run outside a use case. `Supports` and
`Never` suit a read that works with or without an ambient transaction.

`REQUIRES_NEW` and `NOT_SUPPORTED` are deliberately absent: they would need a
second concurrent transaction, which deadlocks against the single SQLite writer.

## Joining versus a savepoint

Under `Required`, `Supports`, or `Mandatory`, a nested `Run` *joins*: it neither
begins nor commits, and a fatal error marks the shared unit rollback only so the
outermost `Run` aborts the whole transaction.

`Nested` is different. It opens a `SAVEPOINT` of the active transaction, so a
rolling-back error inside it undoes back to that savepoint and the outer
transaction survives. The inner `Run` still returns the error to you:

```go
err := manager.Run(ctx, func(ctx context.Context) error {
	return giftWrap(ctx) // returns ErrWrapUnavailable
}, transaction.WithPropagation(transaction.Nested))
// ROLLBACK TO SAVEPOINT undoes the nested work; the outer transaction lives on.
```

When no transaction is active, `Nested` begins a new one, exactly like `Required`.

## Construction options

With propagation as the anchor, the remaining attributes of `@Transactional` fall
out as a small set of `Option` functions:

```go
transaction.WithIsolation(sql.LevelSerializable) // isolation of a new transaction
transaction.ReadOnly()                           // begin the transaction read only
transaction.WithTimeout(2 * time.Second)         // cancel its context after the duration
transaction.WithName("place-order")              // label it, surfaced on TransactionStatus
```

`WithIsolation`, `ReadOnly`, and `WithTimeout` only matter for a brand-new
transaction — the outermost `Run` that actually begins one. They do nothing when
the call *joins* an existing transaction. If you join with an isolation or
read-only request the running transaction cannot honor, `Run` fails with
`ErrIncompatibleJoin`:

```go
return manager.Run(ctx, placeOrderWork,
	transaction.WithName("place-order"),
	transaction.WithTimeout(2*time.Second))
```

A zero timeout, the default, means no timeout; a negative one fails fast with
`ErrInvalidTimeout` before any work runs. When the timeout elapses, the error
`Run` returns wraps `ErrTransactionTimedOut`.

## Rollback rules

By default every error work returns rolls the transaction back. The rollback
rules let an error commit anyway — and the error still comes back to you:

```go
return manager.Run(ctx, work,
	transaction.NoRollbackForError(ErrLoyaltyServiceDown), // commit despite this error
	transaction.RollbackForError(ErrLoyaltyFraud),         // but force a rollback for this
)
```

A rollback rule wins over a no-rollback rule, so an error matched by both still
rolls back:

```go
// error returned by work
//   matches a RollbackForError / RollbackForFunc rule?   -> ROLLBACK (wins)
//   matches a NoRollbackForError / NoRollbackForFunc rule? -> COMMIT (error still returned)
//   otherwise                                            -> ROLLBACK (default)
```

`NoRollbackForError` and `RollbackForError` match through `errors.Is`. The
predicate forms `NoRollbackForFunc` and `RollbackForFunc` take a `func(error) bool`
when a sentinel is not enough:

```go
transaction.RollbackForFunc(func(err error) bool {
	var conflict *ConflictError
	return errors.As(err, &conflict)
})
```

## Synchronization phases

Work registers callbacks for the phases of the `Run` it runs inside. Each
`Register` function reports `false` when no unit of work is active, so the
auto-commit path can fall back to immediate execution. They run in this order:

```go
// work() -> [before-commit: may VETO] -> [before-completion]
//                                              |
//                                  COMMIT --------------- ROLLBACK
//                                     |                       |
//                              [after-commit]    (after-commit skipped)
//                                     |-----------------------|
//                                          [after-completion]   (always, gets the Status)
```

`RegisterBeforeCommit` runs inside the still-open transaction; returning an error
vetoes the commit and forces a rollback, exactly as if work had failed:

```go
transaction.RegisterBeforeCommit(ctx, func(ctx context.Context) error {
	return orderTotalsBalance(ctx) // a non-nil error vetoes the commit
})
```

`RegisterAfterCommit` runs only after a durable commit, on a context whose
transaction has been *detached*, so database work there auto-commits against the
database. A panic from it is recovered and logged, never propagated:

```go
scheduled := transaction.RegisterAfterCommit(ctx, func(ctx context.Context) {
	_ = notifier.SendConfirmation(ctx, order.CustomerEmail, order.ID)
})
if !scheduled {
	_ = notifier.SendConfirmation(ctx, order.CustomerEmail, order.ID) // no transaction: do it now
}
```

`RegisterBeforeCompletion` runs just before commit or rollback, while the
transaction is still bound. It cannot veto by returning, having no error result,
but a panic forces a rollback and is re-raised once the transaction has settled.
`RegisterAfterCompletion` always runs, on commit and rollback alike, receiving the
final `Status`:

```go
transaction.RegisterAfterCompletion(ctx,
	func(ctx context.Context, status transaction.Status) {
		slog.Info("transaction settled", "outcome", status.String())
	})
```

The `Status` is `StatusCommitted`, `StatusRolledBack`, or `StatusUnknown`.

## Savepoint callbacks

`Nested` propagation has its own pair. `RegisterSavepoint` fires just after a
savepoint is created, and `RegisterSavepointRollback` just before a rollback to
one. Both receive the savepoint name, and a panic from either rolls the whole
transaction back:

```go
transaction.RegisterSavepoint(ctx, func(ctx context.Context, savepoint string) {
	slog.Info("savepoint taken", "savepoint", savepoint)
})
transaction.RegisterSavepointRollback(ctx, func(ctx context.Context, savepoint string) {
	slog.Info("rolling back to savepoint", "savepoint", savepoint)
})
```

## Ordering within a phase

By default callbacks within one phase run in registration order. `WithOrder` sets
an explicit order: lower runs first, equal orders keep registration order. The
default order is zero, so an unordered callback runs after any negative-order one
and before any positive-order one:

```go
transaction.RegisterBeforeCommit(ctx, validateTotals, transaction.WithOrder(-1)) // runs first
transaction.RegisterBeforeCommit(ctx, recordAudit, transaction.WithOrder(1))     // runs later
```

`WithOrder` is a `RegisterOption` and works on every `Register` function above.

Continue to [The Event Bus](/gokeel/guides/event-bus/) to publish in-process
events, or to [The Transactional Outbox](/gokeel/guides/transactional-outbox/) to
publish them only after the transaction commits.
