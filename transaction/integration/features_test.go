package integration

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cgardev/gokeel/transaction"
)

// --- Rollback rules: a rollback rule overrides a no-rollback rule ---

func TestRollbackForErrorOverridesNoRollbackForError(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)
	sentinel := errors.New("boom")

	err := manager.Run(t.Context(), func(ctx context.Context) error {
		if err := insertWidget(ctx, manager.Querier(ctx), "one"); err != nil {
			return err
		}
		return sentinel
	},
		transaction.NoRollbackForError(sentinel),
		transaction.RollbackForError(sentinel),
	)
	if !errors.Is(err, sentinel) {
		t.Fatalf("run error = %v, want sentinel", err)
	}
	if got := countWidgets(t, database); got != 0 {
		t.Errorf("widgets = %d, want 0 (rollbackFor must override noRollbackFor)", got)
	}
}

func TestRollbackForFuncForcesRollbackOverABroadNoRollbackRule(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)
	sentinel := errors.New("boom")

	err := manager.Run(t.Context(), func(ctx context.Context) error {
		if err := insertWidget(ctx, manager.Querier(ctx), "one"); err != nil {
			return err
		}
		return sentinel
	},
		transaction.NoRollbackForFunc(func(error) bool { return true }),
		transaction.RollbackForFunc(func(err error) bool { return errors.Is(err, sentinel) }),
	)
	if !errors.Is(err, sentinel) {
		t.Fatalf("run error = %v, want sentinel", err)
	}
	if got := countWidgets(t, database); got != 0 {
		t.Errorf("widgets = %d, want 0", got)
	}
}

// --- Savepoint synchronization callbacks ---

func TestSavepointCallbackFiresOnNestedSuccess(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)

	var created, rolledBack []string
	err := manager.Run(t.Context(), func(outer context.Context) error {
		transaction.RegisterSavepoint(outer, func(_ context.Context, name string) {
			created = append(created, name)
		})
		transaction.RegisterSavepointRollback(outer, func(_ context.Context, name string) {
			rolledBack = append(rolledBack, name)
		})
		return manager.Run(outer, func(inner context.Context) error {
			return insertWidget(inner, manager.Querier(inner), "one")
		}, transaction.WithPropagation(transaction.Nested))
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(created) != 1 || created[0] == "" {
		t.Errorf("savepoint-created callbacks = %v, want one non-empty name", created)
	}
	if len(rolledBack) != 0 {
		t.Errorf("savepoint-rollback callbacks = %v, want none on success", rolledBack)
	}
}

func TestSavepointRollbackCallbackFiresOnNestedRollback(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)
	sentinel := errors.New("nested boom")

	var created, rolledBack []string
	err := manager.Run(t.Context(), func(outer context.Context) error {
		transaction.RegisterSavepoint(outer, func(_ context.Context, name string) {
			created = append(created, name)
		})
		transaction.RegisterSavepointRollback(outer, func(_ context.Context, name string) {
			rolledBack = append(rolledBack, name)
		})
		if err := insertWidget(outer, manager.Querier(outer), "outer"); err != nil {
			return err
		}
		// The nested failure rolls back to the savepoint, leaving the outer intact.
		_ = manager.Run(outer, func(inner context.Context) error {
			if err := insertWidget(inner, manager.Querier(inner), "inner"); err != nil {
				return err
			}
			return sentinel
		}, transaction.WithPropagation(transaction.Nested))
		return nil
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if len(created) != 1 || len(rolledBack) != 1 {
		t.Fatalf("savepoint callbacks created=%v rolledBack=%v, want one each", created, rolledBack)
	}
	if created[0] != rolledBack[0] {
		t.Errorf("savepoint identifiers differ: created %q, rolled back %q", created[0], rolledBack[0])
	}
	if !widgetExists(t, database, "outer") {
		t.Error("outer widget missing; the outer transaction should have committed")
	}
	if widgetExists(t, database, "inner") {
		t.Error("inner widget present; the nested savepoint should have rolled back")
	}
}

// --- Completion state and the post-settlement guard ---

func TestStatusIsCompletedAfterSettlement(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)

	var captured transaction.TransactionStatus
	var completedDuringWork bool
	err := manager.Run(t.Context(), func(ctx context.Context) error {
		captured, _ = transaction.StatusFromContext(ctx)
		completedDuringWork = captured.IsCompleted()
		return insertWidget(ctx, manager.Querier(ctx), "one")
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if completedDuringWork {
		t.Error("status.IsCompleted() = true during work, want false")
	}
	if !captured.IsCompleted() {
		t.Error("status.IsCompleted() = false after Run, want true")
	}
	// SetRollbackOnly after completion must be a harmless no-op.
	captured.SetRollbackOnly()
	if captured.IsRollbackOnly() {
		t.Error("SetRollbackOnly took effect after completion, want no-op")
	}
}

// --- Read-only exposure ---

func TestStatusReportsReadOnly(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)

	var readOnly, readWrite bool
	_ = manager.Run(t.Context(), func(ctx context.Context) error {
		status, _ := transaction.StatusFromContext(ctx)
		readOnly = status.IsReadOnly()
		return nil
	}, transaction.ReadOnly())
	_ = manager.Run(t.Context(), func(ctx context.Context) error {
		status, _ := transaction.StatusFromContext(ctx)
		readWrite = status.IsReadOnly()
		return nil
	})
	if !readOnly {
		t.Error("IsReadOnly() = false for a read-only transaction, want true")
	}
	if readWrite {
		t.Error("IsReadOnly() = true for a read-write transaction, want false")
	}
}

// --- Typed transaction-system errors ---

func TestBeginFailureIsTyped(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)
	database.Close()

	err := manager.Run(t.Context(), func(context.Context) error { return nil })
	if !errors.Is(err, transaction.ErrBeginFailed) {
		t.Fatalf("begin error = %v, want ErrBeginFailed", err)
	}
}

func TestCommitFailureIsTyped(t *testing.T) {
	database := openDeferredConstraintDatabase(t)
	manager := transaction.NewManager(database)

	err := manager.Run(t.Context(), func(ctx context.Context) error {
		// The reference to a missing parent passes now and fails at COMMIT.
		_, execErr := manager.Querier(ctx).ExecContext(ctx,
			"INSERT INTO child (id, parent_id) VALUES (1, 999)")
		return execErr
	})
	if !errors.Is(err, transaction.ErrTransactionSystem) {
		t.Fatalf("commit error = %v, want ErrTransactionSystem", err)
	}
}

// --- Timeout validation ---

func TestNegativeTimeoutIsRejected(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)

	err := manager.Run(t.Context(), func(ctx context.Context) error {
		return insertWidget(ctx, manager.Querier(ctx), "one")
	}, transaction.WithTimeout(-time.Second))
	if !errors.Is(err, transaction.ErrInvalidTimeout) {
		t.Fatalf("run error = %v, want ErrInvalidTimeout", err)
	}
	if got := countWidgets(t, database); got != 0 {
		t.Errorf("widgets = %d, want 0 (work must not run with an invalid timeout)", got)
	}
}

func TestTimeoutSurfacesTypedError(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)

	err := manager.Run(t.Context(), func(ctx context.Context) error {
		// Wait past the transaction deadline, then run a statement the cancelled
		// context must reject.
		<-ctx.Done()
		return insertWidget(ctx, manager.Querier(ctx), "slow")
	}, transaction.WithTimeout(50*time.Millisecond))
	if !errors.Is(err, transaction.ErrTransactionTimedOut) {
		t.Fatalf("run error = %v, want ErrTransactionTimedOut", err)
	}
	// The underlying cause stays reachable for callers that inspect it.
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Errorf("run error = %v, want the deadline cause to remain reachable", err)
	}
	if got := countWidgets(t, database); got != 0 {
		t.Errorf("widgets = %d, want 0", got)
	}
}

// TestCallerCancellationIsNotReportedAsTimeout asserts that cancelling the
// caller's own context is distinguished from the transaction's own timeout.
func TestCallerCancellationIsNotReportedAsTimeout(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)

	ctx, cancel := context.WithCancel(t.Context())
	err := manager.Run(ctx, func(workCtx context.Context) error {
		cancel()
		<-workCtx.Done()
		return workCtx.Err()
	}, transaction.WithTimeout(time.Hour))
	if errors.Is(err, transaction.ErrTransactionTimedOut) {
		t.Fatalf("caller cancellation reported as a transaction timeout: %v", err)
	}
	if !errors.Is(err, context.Canceled) {
		t.Fatalf("run error = %v, want context.Canceled", err)
	}
}

// --- Current transaction name for logging ---

func TestCurrentTransactionName(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)

	var name string
	var ok bool
	_ = manager.Run(t.Context(), func(ctx context.Context) error {
		name, ok = transaction.CurrentTransactionName(ctx)
		return nil
	}, transaction.WithName("create-widget"))
	if !ok || name != "create-widget" {
		t.Errorf("CurrentTransactionName = (%q, %v), want (\"create-widget\", true)", name, ok)
	}

	if _, active := transaction.CurrentTransactionName(t.Context()); active {
		t.Error("CurrentTransactionName reported a name outside a transaction")
	}
}

// --- Result-returning unit of work ---

func TestRunResultReturnsValueOnCommit(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)

	count, err := transaction.RunResult(t.Context(), manager,
		func(ctx context.Context) (int, error) {
			if err := insertWidget(ctx, manager.Querier(ctx), "one"); err != nil {
				return 0, err
			}
			return 42, nil
		})
	if err != nil {
		t.Fatalf("run result: %v", err)
	}
	if count != 42 {
		t.Errorf("result = %d, want 42", count)
	}
	if got := countWidgets(t, database); got != 1 {
		t.Errorf("widgets = %d, want 1", got)
	}
}

func TestRunResultReturnsErrorOnRollback(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)
	sentinel := errors.New("boom")

	_, err := transaction.RunResult(t.Context(), manager,
		func(ctx context.Context) (string, error) {
			if err := insertWidget(ctx, manager.Querier(ctx), "one"); err != nil {
				return "", err
			}
			return "done", sentinel
		})
	if !errors.Is(err, sentinel) {
		t.Fatalf("run result error = %v, want sentinel", err)
	}
	if got := countWidgets(t, database); got != 0 {
		t.Errorf("widgets after rollback = %d, want 0", got)
	}
}
