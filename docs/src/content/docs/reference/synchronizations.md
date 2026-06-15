---
title: Synchronizations and Listeners
description: Registering commit synchronizations, reading transaction status, and observing the transaction lifecycle with execution listeners in gokeel.
---

A unit of work often needs to do more than read and write rows: it may need to run
a callback once the transaction commits, veto a commit that should not happen, or
observe the begin and commit of the transaction for logging. This page covers the
synchronization callbacks work registers from inside a transaction, the status it
can read from the context, and the execution listeners a `Manager` fires around
the physical transaction.

## Registering synchronizations

The `Register*` functions schedule a callback for a phase of the active
transaction. Each takes the context, the callback, and optional
`RegisterOption` values. Callbacks belong to the outermost transaction, so a
nested or joining `Run` registers against the same unit of work.

Every function reports a `bool`: `true` when a unit of work was active and the
callback was scheduled, `false` on a non-transactional path (for example
`Supports` or `Never` without an active transaction). The `false` result lets the
caller fall back to running the work immediately.

```go
import "github.com/cgardev/gokeel/transaction"

scheduled := transaction.RegisterAfterCommit(ctx, func(ctx context.Context) {
    metrics.Increment("order.placed")
})
if !scheduled {
    // No transaction was active, so there is nothing to wait for.
    metrics.Increment("order.placed")
}
```

Registration is multiplicity-preserving: the same callback registered twice runs
twice.

## The synchronization phases

`RegisterBeforeCommit` schedules a callback that runs inside the transaction,
just before the outermost commit. Returning an error vetoes the commit and
forces a rollback. The callback receives the in-transaction context, so any
database work it performs runs on the transaction.

```go
transaction.RegisterBeforeCommit(ctx, func(ctx context.Context) error {
    return validateInvariants(ctx)
})
```

`RegisterBeforeCompletion` schedules a callback that runs just before the
outermost transaction commits or rolls back, while the transaction is still
bound. It has no error result, so it cannot veto by returning, but a panic forces
a rollback and is re-raised to the caller once the transaction has been settled.

```go
transaction.RegisterBeforeCompletion(ctx, func(ctx context.Context) {
    releaseAdvisoryLock(ctx)
})
```

`RegisterAfterCommit` schedules a callback that runs after the outermost
transaction commits successfully. The callback receives a context whose
transaction has been detached, so any database work it performs runs on the
database, never on the closed transaction. A panic from the callback is recovered
and logged, not propagated: the transaction is already durably committed.

```go
transaction.RegisterAfterCommit(ctx, func(ctx context.Context) {
    eventBus.Publish(ctx, OrderPlaced{ID: orderID})
})
```

`RegisterAfterCompletion` schedules a callback that runs after the outermost
transaction commits or rolls back, receiving the final `Status` so it can branch
on the outcome.

```go
transaction.RegisterAfterCompletion(ctx, func(ctx context.Context, status transaction.Status) {
    if status == transaction.StatusRolledBack {
        log.Warn("order placement rolled back")
    }
})
```

## Savepoint synchronizations

`RegisterSavepoint` schedules a callback that runs just after a savepoint is
created, that is when a `Nested` `Run` takes a savepoint of the active
transaction. `RegisterSavepointRollback` schedules a callback that runs just
before a rollback to a savepoint. Both callbacks receive the savepoint name and
run inside the open transaction, so a panic rolls the whole transaction back.

```go
transaction.RegisterSavepoint(ctx, func(ctx context.Context, savepoint string) {
    log.Debug("savepoint taken", "name", savepoint)
})

transaction.RegisterSavepointRollback(ctx, func(ctx context.Context, savepoint string) {
    log.Debug("rolling back to savepoint", "name", savepoint)
})
```

## Ordering callbacks

`WithOrder` is the `RegisterOption` that sets a callback's order within its
phase. Lower orders run first; callbacks with equal orders run in registration
order. The default order is zero, so an unordered callback runs before any
positive-order callback and after any negative-order one.

```go
transaction.RegisterAfterCommit(ctx, flushOutbox, transaction.WithOrder(-10))
transaction.RegisterAfterCommit(ctx, notifyMetrics)            // order 0
transaction.RegisterAfterCommit(ctx, sendEmail, transaction.WithOrder(10))
// Runs flushOutbox, then notifyMetrics, then sendEmail.
```

## Reading transaction status

`TransactionStatus` exposes the live state of the unit of work bound to the
current context. It is obtained through `StatusFromContext`, which returns the
status and `true`, or a zero status and `false` when no transaction is active.

```go
status, active := transaction.StatusFromContext(ctx)
if active && status.IsNewTransaction() {
    log.Debug("running in a freshly begun transaction", "name", status.Name())
}
```

The status reports on the transaction without mutating it:

- `Name()` returns the label set through `WithName`, or the empty string.
- `IsNewTransaction()` reports whether this `Run` began the transaction rather
  than joining or nesting within an existing one.
- `HasSavepoint()` reports whether this `Run` runs inside a savepoint, that is
  under `Nested` propagation.
- `IsReadOnly()` reports whether the transaction was begun read only.
- `IsCompleted()` reports whether the transaction has settled.
- `IsRollbackOnly()` reports whether the transaction has been marked rollback
  only.

`SetRollbackOnly` marks the transaction so the outermost `Run` rolls it back even
when work returns nil. It has no effect once the transaction has completed.

```go
status, _ := transaction.StatusFromContext(ctx)
status.SetRollbackOnly()
```

## Completion statuses

The `Status` an after-completion callback receives is one of three values:

- `StatusCommitted` — the transaction committed.
- `StatusRolledBack` — the transaction rolled back.
- `StatusUnknown` — the outcome could not be determined, such as a commit that
  failed midway.

`Status` satisfies `fmt.Stringer`, so it renders as `Committed`, `RolledBack`, or
`Unknown` in logs.

## Free-function helpers

Two helpers read or mark the active transaction without holding a
`TransactionStatus`, which is convenient in logging or monitoring code far from
the `Run` call.

`CurrentTransactionName` returns the name of the active transaction and `true`,
or the empty string and `false` when no transaction is active.

```go
if name, active := transaction.CurrentTransactionName(ctx); active {
    log.Info("handling request", "transaction", name)
}
```

`MarkRollbackOnly` marks the active transaction so the outermost `Run` rolls it
back even when work returns nil. It reports `false` when no transaction is
active. It is the free-function form of `TransactionStatus.SetRollbackOnly`.

```go
if !transaction.MarkRollbackOnly(ctx) {
    return errors.New("no transaction to abort")
}
```

## Execution listeners

`ExecutionListener` observes the lifecycle of the physical database transaction a
`Manager` drives: the begin, commit, and rollback of a newly begun transaction.
It is meant for stateless observation — logging, metrics, tracing — not for
taking part in the transaction; use the synchronization phases above for that.
Listeners are passed to `NewManager` and apply to every transaction the `Manager`
drives.

```go
listener := transaction.ExecutionListener{
    BeforeBegin: func(ctx context.Context, status transaction.TransactionStatus) {
        log.Debug("beginning transaction", "name", status.Name())
    },
    AfterCommit: func(ctx context.Context, status transaction.TransactionStatus, commitErr error) {
        if commitErr != nil {
            log.Error("commit failed", "error", commitErr)
        }
    },
}

manager := transaction.NewManager(database, listener)
```

Each field is a hook, and every hook is optional: a nil field is skipped. The
`Before*` hooks run just before their step; the `After*` hooks run just after it,
receiving the error the step produced or nil on success.

- `BeforeBegin` / `AfterBegin` fire around the physical begin. On a failed begin
  no transaction is active and the `Run` returns that same error.
- `BeforeCommit` / `AfterCommit` fire around the commit. The after hook runs
  after the after-commit and after-completion synchronizations.
- `BeforeRollback` / `AfterRollback` fire around the rollback. The after hook
  runs after the after-completion synchronizations.

Hooks fire only around the physical begin, commit, and rollback of a new
transaction. They do not fire for a `Run` that joins an active transaction, nor
for the savepoint operations of `Nested` propagation. A panic raised by a hook is
recovered and logged, never propagated, so an observation callback can never
disturb the transaction lifecycle.

For the propagation modes and the options that shape each `Run`, see
[Propagation and Options](/gokeel/reference/propagation-and-options/). For an
end-to-end walkthrough, see the
[Propagation and Synchronizations](/gokeel/guides/propagation-and-synchronizations/)
guide.
