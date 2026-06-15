package integration

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/cgardev/gokeel/transaction"
)

func registerAllPhases(ctx context.Context, phases *[]string) {
	transaction.RegisterBeforeCommit(ctx, func(context.Context) error {
		*phases = append(*phases, "beforeCommit")
		return nil
	})
	transaction.RegisterBeforeCompletion(ctx, func(context.Context) {
		*phases = append(*phases, "beforeCompletion")
	})
	transaction.RegisterAfterCommit(ctx, func(context.Context) {
		*phases = append(*phases, "afterCommit")
	})
	transaction.RegisterAfterCompletion(ctx,
		func(_ context.Context, status transaction.Status) {
			*phases = append(*phases, "afterCompletion:"+status.String())
		})
}

func TestSynchronizationPhaseOrderOnCommit(t *testing.T) {
	manager := transaction.NewManager(openDatabase(t))
	var phases []string
	err := manager.Run(t.Context(), func(ctx context.Context) error {
		registerAllPhases(ctx, &phases)
		return nil
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	want := []string{"beforeCommit", "beforeCompletion", "afterCommit", "afterCompletion:Committed"}
	if !slices.Equal(phases, want) {
		t.Errorf("phase order = %v, want %v", phases, want)
	}
}

func TestSynchronizationPhasesOnRollback(t *testing.T) {
	manager := transaction.NewManager(openDatabase(t))
	var phases []string
	_ = manager.Run(t.Context(), func(ctx context.Context) error {
		registerAllPhases(ctx, &phases)
		return errors.New("boom")
	})
	// before-commit and after-commit never run on the rollback path.
	want := []string{"beforeCompletion", "afterCompletion:RolledBack"}
	if !slices.Equal(phases, want) {
		t.Errorf("phases on rollback = %v, want %v", phases, want)
	}
}

func TestBeforeCommitRunsInsideTheTransaction(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)
	err := manager.Run(t.Context(), func(ctx context.Context) error {
		transaction.RegisterBeforeCommit(ctx, func(callbackCtx context.Context) error {
			return insertWidget(callbackCtx, manager.Querier(callbackCtx), "before-commit-write")
		})
		return nil
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !widgetExists(t, database, "before-commit-write") {
		t.Error("before-commit write was not committed with the transaction")
	}
}

func TestBeforeCommitVetoForcesRollback(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)
	veto := errors.New("veto")
	completion := transaction.StatusCommitted

	err := manager.Run(t.Context(), func(ctx context.Context) error {
		if err := insertWidget(ctx, manager.Querier(ctx), "vetoed"); err != nil {
			return err
		}
		transaction.RegisterBeforeCommit(ctx, func(context.Context) error {
			return veto
		})
		transaction.RegisterAfterCompletion(ctx,
			func(_ context.Context, status transaction.Status) {
				completion = status
			})
		return nil
	})
	if !errors.Is(err, veto) {
		t.Fatalf("error = %v, want veto", err)
	}
	if widgetExists(t, database, "vetoed") {
		t.Error("before-commit veto did not roll back the transaction")
	}
	if completion != transaction.StatusRolledBack {
		t.Errorf("completion status = %s, want RolledBack", completion)
	}
}

func TestBeforeCommitVetoSkipsLaterCallbacksAndCommitPhases(t *testing.T) {
	manager := transaction.NewManager(openDatabase(t))
	veto := errors.New("veto")
	var phases []string

	err := manager.Run(t.Context(), func(ctx context.Context) error {
		transaction.RegisterBeforeCommit(ctx, func(context.Context) error {
			phases = append(phases, "beforeCommit1")
			return veto
		})
		transaction.RegisterBeforeCommit(ctx, func(context.Context) error {
			phases = append(phases, "beforeCommit2")
			return nil
		})
		transaction.RegisterBeforeCompletion(ctx, func(context.Context) {
			phases = append(phases, "beforeCompletion")
		})
		transaction.RegisterAfterCommit(ctx, func(context.Context) {
			phases = append(phases, "afterCommit")
		})
		transaction.RegisterAfterCompletion(ctx,
			func(_ context.Context, status transaction.Status) {
				phases = append(phases, "afterCompletion:"+status.String())
			})
		return nil
	})
	if !errors.Is(err, veto) {
		t.Fatalf("error = %v, want veto", err)
	}
	// The vetoing callback aborts the commit: the second before-commit and the
	// after-commit phase never run, but before-completion still does.
	want := []string{"beforeCommit1", "beforeCompletion", "afterCompletion:RolledBack"}
	if !slices.Equal(phases, want) {
		t.Errorf("phases = %v, want %v", phases, want)
	}
}

func TestNoRollbackRuleRunsCommitSynchronizations(t *testing.T) {
	manager := transaction.NewManager(openDatabase(t))
	skip := errors.New("skip")

	afterCommitRan := false
	completion := transaction.StatusRolledBack
	err := manager.Run(t.Context(), func(ctx context.Context) error {
		transaction.RegisterAfterCommit(ctx, func(context.Context) {
			afterCommitRan = true
		})
		transaction.RegisterAfterCompletion(ctx,
			func(_ context.Context, status transaction.Status) {
				completion = status
			})
		return skip
	}, transaction.NoRollbackForError(skip))
	if !errors.Is(err, skip) {
		t.Fatalf("error = %v, want skip", err)
	}
	if !afterCommitRan {
		t.Error("after-commit did not run for a committed no-rollback transaction")
	}
	if completion != transaction.StatusCommitted {
		t.Errorf("completion status = %s, want Committed", completion)
	}
}

func TestCommitFailureReportsUnknownAndSkipsAfterCommit(t *testing.T) {
	database := openDeferredConstraintDatabase(t)
	manager := transaction.NewManager(database)

	afterCommitRan := false
	completion := transaction.StatusCommitted
	completionRan := false

	err := manager.Run(t.Context(), func(ctx context.Context) error {
		transaction.RegisterAfterCommit(ctx, func(context.Context) {
			afterCommitRan = true
		})
		transaction.RegisterAfterCompletion(ctx,
			func(_ context.Context, status transaction.Status) {
				completion = status
				completionRan = true
			})
		// The reference to a missing parent passes now and fails at COMMIT.
		_, execErr := manager.Querier(ctx).ExecContext(ctx,
			"INSERT INTO child (id, parent_id) VALUES (1, 999)")
		return execErr
	})
	if err == nil {
		t.Fatal("run reported no error despite a failing commit")
	}
	if afterCommitRan {
		t.Error("after-commit ran even though the commit failed")
	}
	if !completionRan || completion != transaction.StatusUnknown {
		t.Errorf("after-completion status = %s (ran=%v), want Unknown", completion, completionRan)
	}
}

func TestSynchronizationsRunInOrder(t *testing.T) {
	manager := transaction.NewManager(openDatabase(t))
	var order []int
	err := manager.Run(t.Context(), func(ctx context.Context) error {
		transaction.RegisterAfterCommit(ctx, func(context.Context) {
			order = append(order, 2)
		}, transaction.WithOrder(2))
		transaction.RegisterAfterCommit(ctx, func(context.Context) {
			order = append(order, -1)
		}, transaction.WithOrder(-1))
		transaction.RegisterAfterCommit(ctx, func(context.Context) {
			order = append(order, 0) // default order
		})
		transaction.RegisterAfterCommit(ctx, func(context.Context) {
			order = append(order, 1)
		}, transaction.WithOrder(1))
		return nil
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	want := []int{-1, 0, 1, 2}
	if !slices.Equal(order, want) {
		t.Errorf("after-commit order = %v, want %v", order, want)
	}
}

func TestSynchronizationsWithEqualOrderKeepRegistrationOrder(t *testing.T) {
	manager := transaction.NewManager(openDatabase(t))
	var order []string
	err := manager.Run(t.Context(), func(ctx context.Context) error {
		transaction.RegisterAfterCommit(ctx, func(context.Context) {
			order = append(order, "first")
		}, transaction.WithOrder(5))
		transaction.RegisterAfterCommit(ctx, func(context.Context) {
			order = append(order, "second")
		}, transaction.WithOrder(5))
		return nil
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	want := []string{"first", "second"}
	if !slices.Equal(order, want) {
		t.Errorf("order = %v, want %v", order, want)
	}
}

func TestRegisterReportsNoActiveUnitOutsideRun(t *testing.T) {
	ctx := t.Context()
	if transaction.RegisterBeforeCommit(ctx, func(context.Context) error { return nil }) {
		t.Error("RegisterBeforeCommit reported an active unit outside Run")
	}
	if transaction.RegisterBeforeCompletion(ctx, func(context.Context) {}) {
		t.Error("RegisterBeforeCompletion reported an active unit outside Run")
	}
	registeredCompletion := transaction.RegisterAfterCompletion(
		ctx, func(context.Context, transaction.Status) {})
	if registeredCompletion {
		t.Error("RegisterAfterCompletion reported an active unit outside Run")
	}
}
