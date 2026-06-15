---
title: Transaction Manager
description: Constructing a Manager, running a context-bound unit of work, resolving the querier, and the Querier and Transactor interfaces.
---

A `Manager` owns the database transaction lifecycle: it begins, commits, and
rolls back the transactions a unit of work runs in. Work is supplied as a
function bound to a `context.Context`, and stores resolve the executor they run
against from that context rather than receiving a `*sql.Tx` through their
signatures. The package depends only on `database/sql`.

```go
import "github.com/cgardev/gokeel/transaction"
```

## NewManager

`NewManager` constructs a `Manager` over an open `*sql.DB`. The variadic
listeners are optional and observe the begin, commit, and rollback steps of
every new transaction the `Manager` drives.

```go
func NewManager(database *sql.DB, listeners ...ExecutionListener) *Manager
```

```go
database, err := sql.Open("sqlite", "app.db")
if err != nil {
    return err
}

manager := transaction.NewManager(database)
```

A `Manager` is immutable after construction and safe for concurrent use, so a
single instance is shared across the application.

### Execution listeners

An `ExecutionListener` observes the lifecycle of the physical transaction. Every
hook is optional: a nil field is skipped. Listeners are meant for stateless
observation — logging, metrics, tracing — not for taking part in the
transaction. Their fields are:

- `BeforeBegin(ctx, status)` — runs before the transaction is begun.
- `AfterBegin(ctx, status, beginErr)` — runs after the begin step, with the
  error it produced or nil on success.
- `BeforeCommit(ctx, status)` — runs inside the transaction, just before commit.
- `AfterCommit(ctx, status, commitErr)` — runs after the commit step.
- `BeforeRollback(ctx, status)` — runs just before the rollback.
- `AfterRollback(ctx, status, rollbackErr)` — runs after the rollback step.

```go
logging := transaction.ExecutionListener{
    AfterCommit: func(ctx context.Context, status transaction.TransactionStatus, commitErr error) {
        slog.Info("transaction committed", "name", status.Name(), "error", commitErr)
    },
}

manager := transaction.NewManager(database, logging)
```

Hooks fire only around the physical begin, commit, and rollback of a new
transaction; they do not fire for a `Run` that joins an active transaction nor
for the savepoint operations of `Nested` propagation. A panic raised by a hook
is recovered and logged, never propagated.

## Manager.Run

`Run` executes `work` as a unit of work configured by `opts`.

```go
func (manager *Manager) Run(
    ctx context.Context,
    work func(ctx context.Context) error,
    opts ...Option,
) error
```

With no options it uses `Required` propagation at the database default
isolation: it joins an active transaction or begins one, commits when `work`
returns nil, and rolls back on error or panic.

```go
err := manager.Run(ctx, func(ctx context.Context) error {
    if err := accounts.Debit(ctx, from, amount); err != nil {
        return err
    }
    return accounts.Credit(ctx, to, amount)
})
```

Both store calls run in the same transaction. Returning an error from either one
rolls the whole transaction back; returning nil commits it.

Options are the programmatic equivalent of the attributes of Spring's
`@Transactional`. `WithPropagation` selects how the call relates to an active
transaction, `WithIsolation` requests an isolation level, `ReadOnly` marks the
transaction read only, `WithTimeout` bounds its duration, and `WithName` labels
it for monitoring.

```go
err := manager.Run(ctx, func(ctx context.Context) error {
    return reports.Generate(ctx)
}, transaction.ReadOnly(), transaction.WithName("nightly-report"))
```

A nested `Run` joins the transaction already bound to the context, so a use case
that spans several stores runs them in one transaction without threading it by
hand. To run within a savepoint of the active transaction instead, pass
`WithPropagation(Nested)`.

```go
err := manager.Run(ctx, func(ctx context.Context) error {
    return manager.Run(ctx, func(ctx context.Context) error {
        return audit.Record(ctx, entry)
    }, transaction.WithPropagation(transaction.Nested))
})
```

The full set of propagation behaviors, isolation, timeout, and rollback rules is
covered in [Propagation and options](/gokeel/reference/propagation-and-options/).

## RunResult

`RunResult` runs `work` as a unit of work like `Manager.Run`, but lets `work`
return a value alongside its error. It is a free function rather than a method
because Go methods cannot be generic.

```go
func RunResult[T any](
    ctx context.Context,
    transactor Transactor,
    work func(ctx context.Context) (T, error),
    opts ...Option,
) (T, error)
```

The value `work` produced is returned with the error `Run` reports. On a failed
or rolled-back transaction the caller should consult the error rather than the
value.

```go
order, err := transaction.RunResult(ctx, manager, func(ctx context.Context) (Order, error) {
    return orders.Create(ctx, request)
})
if err != nil {
    return Order{}, err
}
```

The `transactor` argument is any `Transactor`, so `*Manager` is passed directly.

## Manager.Querier

`Querier` resolves the executor for the current context: the active transaction
when a unit of work is in progress, otherwise the database for an auto-commit
statement.

```go
func (manager *Manager) Querier(ctx context.Context) Querier
```

Stores pass the result as the final querier argument of their terminal calls, so
the same store method participates in a transaction when called inside `Run` and
runs auto-commit when called outside one.

```go
func (s *AccountStore) Balance(ctx context.Context, id int64) (int64, error) {
    return gooq.Select1(db.Account.Balance).
        From(db.Account).
        Where(db.Account.Id.EQ(id)).
        FetchSingle(ctx, s.manager.Querier(ctx))
}
```

## Querier

`Querier` is the execution surface the stores run their statements against. It
is satisfied by `*sql.DB`, `*sql.Tx`, and `*sql.Conn`, and its method set
matches the minimal querier a query builder accepts, so the resolved value can
be passed straight to one.

```go
type Querier interface {
    QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
    ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}
```

## Transactor

`Transactor` is the slice of the `Manager` that stores depend on: it runs a unit
of work and resolves the querier the store executes against. It is satisfied by
`*Manager`.

```go
type Transactor interface {
    Run(ctx context.Context, work func(ctx context.Context) error, opts ...Option) error
    Querier(ctx context.Context) Querier
}
```

Depending on `Transactor` rather than the concrete `*Manager` keeps a store
testable in isolation and is also the type `RunResult` accepts.

```go
type AccountStore struct {
    transactor transaction.Transactor
}

func NewAccountStore(transactor transaction.Transactor) *AccountStore {
    return &AccountStore{transactor: transactor}
}
```

To inspect the live state of the active unit of work or to register
synchronization callbacks, see
[Transaction status and synchronizations](/gokeel/reference/synchronizations/).
