---
title: Propagation and Options
description: The propagation behaviors and the options that configure a unit of work in gokeel.
---

A unit of work is configured by the options passed to `Run`. Each option is an
`Option` value, the programmatic equivalent of an attribute of Spring's
`@Transactional`: propagation, isolation, read-only, timeout, a name, and the
rollback rules. This page lists the `Propagation` constants and every `Option`
function declared in the `transaction` package.

```go
import "github.com/cgardev/gokeel/transaction"
```

## Propagation

`Propagation` selects how a `Run` relates to a transaction that may already be
bound to the context, mirroring Spring's propagation behaviors. There are five
constants; `Required` is the default and the value `Run` resolves to when
`WithPropagation` is not supplied.

```go
transaction.Required  // join an active transaction or begin a new one
transaction.Supports  // join an active transaction, otherwise run with no transaction
transaction.Mandatory // join an active transaction, fail when none is active
transaction.Never     // run with no transaction, fail when one is active
transaction.Nested    // run within a savepoint of the active transaction
```

The two Spring behaviors that suspend the active transaction or open a second
concurrent one, `REQUIRES_NEW` and `NOT_SUPPORTED`, are intentionally omitted:
on a single-writer SQLite database a second concurrent transaction would
deadlock against the write lock the first one already holds.

`Propagation` implements `String`, so it renders as `Required`, `Supports`,
`Mandatory`, `Never`, or `Nested` in logs and test failures.

### Required

`Required` joins an active transaction or begins a new one. It is the
propagation most callers need, and the one selected when no `WithPropagation`
option is given.

```go
err := manager.Run(ctx, func(ctx context.Context) error {
    return store.Save(ctx, order)
})
```

### Supports

`Supports` joins an active transaction when one exists, and otherwise runs the
work with no transaction. Synchronization callbacks are not maintained on the
non-transactional path, so the `Register*` functions report `false` there.

```go
err := manager.Run(ctx, work, transaction.WithPropagation(transaction.Supports))
```

### Mandatory

`Mandatory` joins an active transaction and fails with `ErrTransactionRequired`
when none is active. It is the way to assert that a function must always be
called from within an existing unit of work.

```go
err := manager.Run(ctx, work, transaction.WithPropagation(transaction.Mandatory))
// err is ErrTransactionRequired when no transaction is active.
```

### Never

`Never` runs the work with no transaction and fails with
`ErrTransactionNotAllowed` when a transaction is already active.

```go
err := manager.Run(ctx, work, transaction.WithPropagation(transaction.Never))
// err is ErrTransactionNotAllowed when a transaction is active.
```

### Nested

`Nested` runs within a savepoint of the active transaction, so its work can roll
back to the savepoint without aborting the outer transaction. It begins a new
transaction when none is active.

```go
err := manager.Run(ctx, func(ctx context.Context) error {
    // SAVEPOINT transaction_savepoint_1
    return store.AttemptOptionalStep(ctx)
    // an error here rolls back to the savepoint, not the whole transaction
}, transaction.WithPropagation(transaction.Nested))
```

See [Propagation and Synchronizations](/gokeel/guides/propagation-and-synchronizations/)
for a walk-through of each behavior in a service graph.

## The options

Every option below is a function that returns an `Option`. They are passed as
the variadic tail of `Run`; later options override earlier ones for the same
field. The default unit of work uses `Required` propagation at the database
default isolation, with no read-only hint, no timeout, no name, and the default
rollback rule.

### WithPropagation

```go
func WithPropagation(propagation Propagation) Option
```

Selects the propagation behavior. The default is `Required`.

```go
transaction.WithPropagation(transaction.Mandatory)
```

### WithIsolation

```go
func WithIsolation(level sql.IsolationLevel) Option
```

Sets the isolation level a newly begun transaction requests of the driver. It
has no effect when the call joins an existing transaction. A call that joins an
active transaction while requesting a different explicit isolation level fails
with `ErrIncompatibleJoin`.

```go
transaction.WithIsolation(sql.LevelSerializable)
```

### ReadOnly

```go
func ReadOnly() Option
```

Marks a newly begun transaction read only, a hint the driver may use to optimize
or to refuse writes. It has no effect when the call joins an existing
transaction, and a read-only call that joins a read-write transaction fails with
`ErrIncompatibleJoin`. The flag is observable through
`TransactionStatus.IsReadOnly`.

```go
err := manager.Run(ctx, report, transaction.ReadOnly())
```

### WithTimeout

```go
func WithTimeout(timeout time.Duration) Option
```

Bounds the duration of a newly begun transaction: its context is cancelled once
the timeout elapses, so a statement that overruns fails and the transaction
rolls back. It has no effect when the call joins an existing transaction. A zero
duration, the default, means no timeout; a negative duration is invalid and
makes `Run` fail with `ErrInvalidTimeout`. When the timeout elapses, the error
`Run` returns wraps `ErrTransactionTimedOut`.

```go
err := manager.Run(ctx, work, transaction.WithTimeout(2*time.Second))
if errors.Is(err, transaction.ErrTransactionTimedOut) {
    // the transaction overran its own deadline and rolled back
}
```

A cancellation of the caller's own context is not reported as a timeout.

### WithName

```go
func WithName(name string) Option
```

Labels the unit of work, surfaced through `TransactionStatus.Name` and
`CurrentTransactionName` for logging and monitoring. It has no effect on the
transaction itself.

```go
err := manager.Run(ctx, work, transaction.WithName("place-order"))
```

## Rollback rules

By default every non-nil error rolls the transaction back. The rollback rules
narrow or restore that behavior. A rule comes in two forms: an `Error` form that
matches sentinels through `errors.Is`, and a `Func` form that takes a predicate.
A rollback rule takes precedence over a no-rollback rule, so an error matched by
both still rolls back.

### RollbackForError

```go
func RollbackForError(targets ...error) Option
```

Forces a rollback when the work error matches, through `errors.Is`, any of the
given sentinels, overriding any no-rollback rule that would otherwise excuse it.
It is redundant with the default, which rolls back on every error, and is only
useful to re-include an error a broader `NoRollbackForError` or
`NoRollbackForFunc` rule would have committed.

```go
transaction.RollbackForError(ErrInventoryConflict)
```

### RollbackForFunc

```go
func RollbackForFunc(predicate func(error) bool) Option
```

Forces a rollback when `predicate` reports `true` for the work error, overriding
any no-rollback rule that would otherwise excuse it.

```go
transaction.RollbackForFunc(func(err error) bool {
    var conflict *ConflictError
    return errors.As(err, &conflict)
})
```

### NoRollbackForError

```go
func NoRollbackForError(targets ...error) Option
```

Keeps the transaction committable when the work error matches, through
`errors.Is`, any of the given sentinels, unless a `RollbackForError` or
`RollbackForFunc` rule also matches. The error is still returned to the caller.

```go
err := manager.Run(ctx, charge, transaction.NoRollbackForError(ErrReceiptDeferred))
// the transaction commits, yet err is ErrReceiptDeferred
```

### NoRollbackForFunc

```go
func NoRollbackForFunc(predicate func(error) bool) Option
```

Keeps the transaction committable when `predicate` reports `true` for the work
error. The error is still returned to the caller.

```go
transaction.NoRollbackForFunc(func(err error) bool {
    return errors.Is(err, ErrAlreadyProcessed)
})
```

## Combining options

Options are independent and accumulate, so a single `Run` can set the
propagation, request an isolation level, and add a rollback rule at once. The
rollback rules in particular append rather than replace, so several may apply to
one unit of work.

```go
err := manager.Run(ctx, placeOrder,
    transaction.WithName("place-order"),
    transaction.WithIsolation(sql.LevelSerializable),
    transaction.WithTimeout(5*time.Second),
    transaction.NoRollbackForError(ErrReceiptDeferred),
    transaction.RollbackForError(ErrInventoryConflict),
)
```

The same options are accepted by `RunResult`, the generic free function that
lets the work return a value alongside its error. For the lifecycle that drives
these options, see the [Transaction Manager](/gokeel/reference/transaction-manager/);
for the callbacks a unit of work can register, see
[Synchronizations and Listeners](/gokeel/reference/synchronizations/).
