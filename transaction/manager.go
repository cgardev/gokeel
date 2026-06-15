// Package transaction provides a context-bound unit of work: the Go
// analog of a Spring @Transactional method over a single datasource. A Manager
// owns the database transaction lifecycle, and stores resolve the querier they
// execute against from the context instead of receiving a *sql.Tx through their
// signatures. Nested Run calls join the transaction already bound to the
// context, so a use case that spans several stores runs them in one
// transaction without threading it by hand.
//
// Run accepts options that mirror the attributes of @Transactional:
// propagation (Required, Supports, Mandatory, Never, Nested), isolation level,
// read-only, timeout, and rollback rules. Work may also register callbacks for
// the before-commit, before-completion, after-commit, and after-completion
// synchronization phases.
//
// A Manager may also be constructed with ExecutionListeners that observe the
// begin, commit, and rollback steps of each new transaction, the seam for
// logging, metrics, and tracing.
//
// The synchronization and savepoint callbacks require an active unit of work:
// the Register functions report false on a non-transactional path (Supports or
// Never without an active transaction), so callbacks are not maintained there,
// and registration is multiplicity-preserving, so the same callback registered
// twice runs twice.
//
// The package depends only on database/sql; it knows nothing of the query
// builder the stores use.
package transaction

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"log/slog"
)

// Querier is the execution surface the stores run their statements against. It
// is satisfied by *sql.DB, *sql.Tx, and *sql.Conn, and its method set matches
// the minimal querier a query builder accepts, so the resolved value can be
// passed straight to one without this package importing the builder.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Manager begins, commits, and rolls back the database transactions its units
// of work run in. It is immutable after construction and safe for concurrent
// use; concurrency is bounded by the single SQLite writer.
type Manager struct {
	database  *sql.DB
	listeners []ExecutionListener
}

// NewManager constructs a Manager over an open database. The optional listeners
// observe the begin, commit, and rollback steps of every new transaction the
// Manager drives; see ExecutionListener.
func NewManager(database *sql.DB, listeners ...ExecutionListener) *Manager {
	return &Manager{database: database, listeners: listeners}
}

// Run executes work as a unit of work configured by opts. With no options it
// uses Required propagation at the database default isolation: it joins an
// active transaction or begins one, commits when work returns nil, and rolls
// back on error or panic. The propagation option selects a different relation
// to an active transaction; see Propagation.
func (manager *Manager) Run(
	ctx context.Context, work func(ctx context.Context) error, opts ...Option,
) error {
	def := newDefinition(opts)
	if def.timeout < 0 {
		return ErrInvalidTimeout
	}
	unit := fromContext(ctx)

	switch def.propagation {
	case Required:
		if unit != nil {
			return participate(ctx, unit, work, def)
		}
		return manager.runNew(ctx, work, def)
	case Supports:
		if unit != nil {
			return participate(ctx, unit, work, def)
		}
		return work(ctx)
	case Mandatory:
		if unit == nil {
			return ErrTransactionRequired
		}
		return participate(ctx, unit, work, def)
	case Never:
		if unit != nil {
			return ErrTransactionNotAllowed
		}
		return work(ctx)
	case Nested:
		if unit != nil {
			return manager.runNested(ctx, unit, work, def)
		}
		return manager.runNew(ctx, work, def)
	default:
		return fmt.Errorf("unknown propagation %s", def.propagation)
	}
}

// participate runs work as part of an already active transaction: it neither
// begins nor commits. A failure the rollback rules treat as fatal marks the
// unit rollback only, so the outermost Run aborts the whole transaction.
func participate(
	ctx context.Context, unit *unitOfWork, work func(ctx context.Context) error, def definition,
) error {
	if err := validateJoin(def, unit); err != nil {
		return err
	}
	status := &TransactionStatus{unit: unit, name: def.name}
	if err := work(withStatus(ctx, status)); err != nil {
		if def.shouldRollback(err) {
			unit.markRollbackOnly()
		}
		return err
	}
	return nil
}

// validateJoin rejects options a call cannot obtain by joining an active
// transaction: a different explicit isolation level, or a read-only guarantee
// over a read-write transaction.
func validateJoin(def definition, unit *unitOfWork) error {
	if def.isolation != sql.LevelDefault && def.isolation != unit.isolation {
		return fmt.Errorf("%w: isolation %s requested while joining a transaction at %s",
			ErrIncompatibleJoin, def.isolation, unit.isolation)
	}
	if def.readOnly && !unit.readOnly {
		return fmt.Errorf("%w: read-only requested while joining a read-write transaction",
			ErrIncompatibleJoin)
	}
	return nil
}

// runNew begins a transaction, binds it into the context, runs work, and drives
// the full synchronization lifecycle around the commit or rollback.
func (manager *Manager) runNew(
	ctx context.Context, work func(ctx context.Context) error, def definition,
) error {
	transactionContext := ctx
	if def.timeout > 0 {
		var cancel context.CancelFunc
		transactionContext, cancel = context.WithTimeoutCause(ctx, def.timeout, ErrTransactionTimedOut)
		defer cancel()
	}

	unit := &unitOfWork{isolation: def.isolation, readOnly: def.readOnly}
	status := &TransactionStatus{unit: unit, name: def.name, newTransaction: true}

	manager.fireBeforeBegin(transactionContext, *status)
	transaction, err := manager.database.BeginTx(transactionContext, def.transactionOptions())
	if err != nil {
		manager.fireAfterBegin(transactionContext, *status, err)
		return fmt.Errorf("%w: %w", ErrBeginFailed, err)
	}
	unit.transaction = transaction
	manager.fireAfterBegin(transactionContext, *status, nil)

	bound := withStatus(transactionContext, status)
	completed := withoutUnit(ctx)

	// Every phase that runs while the transaction is still open is guarded, so a
	// panic in work or in a before-commit or before-completion callback can
	// never escape with the transaction left open: the panic forces a rollback
	// and is re-raised only after the transaction has been settled.
	workErr, workPanic := runGuarded(bound, work)
	if workErr != nil && timedOut(transactionContext) {
		workErr = fmt.Errorf("%w: %w", ErrTransactionTimedOut, workErr)
	}
	rollback := workPanic != nil || unit.isRollbackOnly() ||
		(workErr != nil && def.shouldRollback(workErr))

	var beforeCommitPanic any
	if !rollback {
		vetoErr, panicValue := runGuarded(bound, unit.runBeforeCommit)
		switch {
		case panicValue != nil:
			beforeCommitPanic = panicValue
			rollback = true
		case vetoErr != nil:
			workErr = errors.Join(workErr, vetoErr)
			rollback = true
		}
	}

	beforeCompletionPanic := runGuardedVoid(bound, unit.runBeforeCompletion)
	if beforeCompletionPanic != nil {
		rollback = true
	}

	if rollback {
		manager.fireBeforeRollback(bound, *status)
		rollbackErr := ignoreTxDone(transaction.Rollback())
		unit.markCompleted()
		unit.runAfterCompletion(completed, StatusRolledBack)
		manager.fireAfterRollback(completed, *status, rollbackErr)
		reraise(workPanic, beforeCommitPanic, beforeCompletionPanic)
		settleErr := wrapSystem(rollbackErr)
		switch {
		case workErr != nil:
			return errors.Join(workErr, settleErr)
		case settleErr != nil:
			return settleErr
		default:
			return ErrRollbackOnly
		}
	}

	manager.fireBeforeCommit(bound, *status)
	if commitErr := transaction.Commit(); commitErr != nil {
		unit.markCompleted()
		unit.runAfterCompletion(completed, StatusUnknown)
		manager.fireAfterCommit(completed, *status, commitErr)
		return errors.Join(workErr, wrapSystem(commitErr))
	}
	unit.markCompleted()
	// The transaction is durably committed; the after phases recover their own
	// panics so a faulty callback cannot turn a committed transaction into a
	// caller-visible failure.
	unit.runAfterCommit(completed)
	unit.runAfterCompletion(completed, StatusCommitted)
	manager.fireAfterCommit(completed, *status, nil)
	return workErr
}

// runGuardedVoid runs fn and turns a panic into a recovered value, so the
// caller can settle the transaction before re-raising it.
func runGuardedVoid(ctx context.Context, fn func(ctx context.Context)) (panicValue any) {
	defer func() {
		if recovered := recover(); recovered != nil {
			panicValue = recovered
		}
	}()
	fn(ctx)
	return nil
}

// reraise re-panics with the first non-nil panic value, used to propagate a
// pre-settlement panic once the transaction has been safely rolled back.
func reraise(panicValues ...any) {
	for _, panicValue := range panicValues {
		if panicValue != nil {
			panic(panicValue)
		}
	}
}

// runNested runs work inside a savepoint of the active transaction. A failure
// the rollback rules treat as fatal rolls back to the savepoint, leaving the
// outer transaction intact; success or a non-rolling-back error releases it.
// Synchronizations belong to the outermost transaction, so a nested rollback
// does not unregister callbacks the nested work added.
func (manager *Manager) runNested(
	ctx context.Context, unit *unitOfWork, work func(ctx context.Context) error, def definition,
) error {
	if err := validateJoin(def, unit); err != nil {
		return err
	}
	name := unit.nextSavepointName()
	if _, err := unit.transaction.ExecContext(ctx, "SAVEPOINT "+name); err != nil {
		return wrapSystem(fmt.Errorf("create savepoint: %w", err))
	}
	unit.runSavepoint(ctx, name)

	status := &TransactionStatus{unit: unit, name: def.name, savepoint: true}
	// A panic propagates to the guard of the outer transaction, which rolls the
	// whole transaction back and discards the savepoint with it, so the nested
	// run needs no panic handling of its own.
	workErr := work(withStatus(ctx, status))

	if workErr != nil && def.shouldRollback(workErr) {
		unit.runSavepointRollback(ctx, name)
		if _, err := unit.transaction.ExecContext(ctx, "ROLLBACK TO SAVEPOINT "+name); err != nil {
			// The nested work could not be undone; escalate so the outer
			// transaction does not commit it.
			unit.markRollbackOnly()
			return errors.Join(workErr, wrapSystem(err))
		}
	}

	// Releasing the savepoint is cleanup: it is discarded with the transaction
	// regardless, so a failure here must not abort the surrounding work. A
	// failure is logged rather than discarded so a genuine driver or connection
	// problem during the release stays observable. This mirrors Spring's
	// rollbackToHeldSavepoint, which rolls back to the savepoint and releases it.
	if _, err := unit.transaction.ExecContext(ctx, "RELEASE SAVEPOINT "+name); err != nil {
		slog.Warn("releasing savepoint failed", "savepoint", name, "error", err)
	}
	return workErr
}

// runGuarded runs work and turns a panic into a recovered value, so the caller
// can roll back the transaction before re-raising it.
func runGuarded(
	ctx context.Context, work func(ctx context.Context) error,
) (err error, panicValue any) {
	defer func() {
		if recovered := recover(); recovered != nil {
			panicValue = recovered
		}
	}()
	return work(ctx), nil
}

// timedOut reports whether the transaction's own timeout, configured through
// WithTimeout, expired, as opposed to a cancellation of the caller's context.
// It relies on the cause WithTimeoutCause attaches when only that timer fires.
func timedOut(ctx context.Context) bool {
	return errors.Is(context.Cause(ctx), ErrTransactionTimedOut)
}

// ignoreTxDone discards the error a rollback reports when the transaction was
// already settled, for example because its context was cancelled by a timeout.
func ignoreTxDone(err error) error {
	if errors.Is(err, sql.ErrTxDone) {
		return nil
	}
	return err
}

// wrapSystem tags an infrastructure failure of a commit, rollback, or savepoint
// step with ErrTransactionSystem, leaving the driver error reachable through
// errors.Is and errors.As. It returns a nil error unchanged.
func wrapSystem(err error) error {
	if err == nil {
		return nil
	}
	return fmt.Errorf("%w: %w", ErrTransactionSystem, err)
}

// RunResult runs work as a unit of work like Transactor.Run, but lets work
// return a value alongside its error. The value work produced is returned with
// the error Run reports; on a failed or rolled-back transaction the caller
// should consult the error rather than the value. It is the generic counterpart
// of Spring's TransactionTemplate.execute, expressed as a free function because
// Go methods cannot be generic.
func RunResult[T any](
	ctx context.Context, transactor Transactor,
	work func(ctx context.Context) (T, error), opts ...Option,
) (T, error) {
	var result T
	err := transactor.Run(ctx, func(ctx context.Context) error {
		var workErr error
		result, workErr = work(ctx)
		return workErr
	}, opts...)
	return result, err
}

// Querier resolves the executor for the current context: the active
// transaction when a unit of work is in progress, otherwise the database for
// an auto-commit statement. Stores pass the result as the final querier
// argument of their terminal calls, mirroring DataSourceUtils.getConnection.
func (manager *Manager) Querier(ctx context.Context) Querier {
	if unit := fromContext(ctx); unit != nil {
		return unit.transaction
	}
	return manager.database
}

// Transactor is the slice of the Manager that stores depend on: it runs a unit
// of work and resolves the querier the store executes against. It is satisfied
// by *Manager.
type Transactor interface {
	Run(ctx context.Context, work func(ctx context.Context) error, opts ...Option) error
	Querier(ctx context.Context) Querier
}
