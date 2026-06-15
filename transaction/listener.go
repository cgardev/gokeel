package transaction

import (
	"context"
	"log/slog"
)

// ExecutionListener observes the lifecycle of the physical database transaction
// a Manager drives: the begin, commit, and rollback of a newly begun
// transaction. It is the analog of Spring's TransactionExecutionListener and is
// meant for stateless observation — logging, metrics, tracing — not for taking
// part in the transaction; use the synchronization phases (RegisterBeforeCommit
// and its companions) for that.
//
// Every hook is optional: a nil field is skipped. The before hooks run just
// before their step, the after hooks just after it, receiving the error the
// step produced or nil on success. Hooks fire only around the physical begin,
// commit, and rollback of a new transaction; they do not fire for a Run that
// joins an active transaction (which performs no physical begin or commit) nor
// for the savepoint operations of Nested propagation. The commit and rollback
// hooks run after the corresponding synchronization phases, mirroring Spring's
// ordering.
//
// A panic raised by a hook is recovered and logged, never propagated, so an
// observation callback can never disturb the transaction lifecycle. This
// diverges from Spring, where a listener exception propagates to the caller.
type ExecutionListener struct {
	// BeforeBegin runs before the transaction is begun.
	BeforeBegin func(ctx context.Context, status TransactionStatus)

	// AfterBegin runs after the begin step, with the error it produced or nil on
	// success. On failure no transaction is active and the Run returns that same
	// error.
	AfterBegin func(ctx context.Context, status TransactionStatus, beginErr error)

	// BeforeCommit runs inside the transaction, just before it is committed.
	BeforeCommit func(ctx context.Context, status TransactionStatus)

	// AfterCommit runs after the commit step, with the error it produced or nil
	// on success. It runs after the after-commit and after-completion
	// synchronizations.
	AfterCommit func(ctx context.Context, status TransactionStatus, commitErr error)

	// BeforeRollback runs just before the transaction is rolled back.
	BeforeRollback func(ctx context.Context, status TransactionStatus)

	// AfterRollback runs after the rollback step, with the error it produced or
	// nil on success. It runs after the after-completion synchronizations.
	AfterRollback func(ctx context.Context, status TransactionStatus, rollbackErr error)
}

func (manager *Manager) fireBeforeBegin(ctx context.Context, status TransactionStatus) {
	for _, listener := range manager.listeners {
		if listener.BeforeBegin != nil {
			recoverListener("before-begin", func() { listener.BeforeBegin(ctx, status) })
		}
	}
}

func (manager *Manager) fireAfterBegin(
	ctx context.Context, status TransactionStatus, beginErr error,
) {
	for _, listener := range manager.listeners {
		if listener.AfterBegin != nil {
			recoverListener("after-begin", func() { listener.AfterBegin(ctx, status, beginErr) })
		}
	}
}

func (manager *Manager) fireBeforeCommit(ctx context.Context, status TransactionStatus) {
	for _, listener := range manager.listeners {
		if listener.BeforeCommit != nil {
			recoverListener("before-commit", func() { listener.BeforeCommit(ctx, status) })
		}
	}
}

func (manager *Manager) fireAfterCommit(
	ctx context.Context, status TransactionStatus, commitErr error,
) {
	for _, listener := range manager.listeners {
		if listener.AfterCommit != nil {
			recoverListener("after-commit", func() { listener.AfterCommit(ctx, status, commitErr) })
		}
	}
}

func (manager *Manager) fireBeforeRollback(ctx context.Context, status TransactionStatus) {
	for _, listener := range manager.listeners {
		if listener.BeforeRollback != nil {
			recoverListener("before-rollback", func() { listener.BeforeRollback(ctx, status) })
		}
	}
}

func (manager *Manager) fireAfterRollback(
	ctx context.Context, status TransactionStatus, rollbackErr error,
) {
	for _, listener := range manager.listeners {
		if listener.AfterRollback != nil {
			recoverListener("after-rollback", func() { listener.AfterRollback(ctx, status, rollbackErr) })
		}
	}
}

// recoverListener runs an execution-listener hook, recovering and logging a
// panic so an observation callback can never disturb the transaction lifecycle.
func recoverListener(phase string, invoke func()) {
	defer func() {
		if recovered := recover(); recovered != nil {
			slog.Error("execution listener panicked",
				"phase", phase, "panic", recovered)
		}
	}()
	invoke()
}
