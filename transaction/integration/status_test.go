package integration

import (
	"context"
	"database/sql"
	"errors"
	"testing"

	"github.com/cgardev/gokeel/transaction"
)

func TestMarkRollbackOnlyRollsBackWithoutAnError(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)

	err := manager.Run(t.Context(), func(ctx context.Context) error {
		if err := insertWidget(ctx, manager.Querier(ctx), "doomed"); err != nil {
			return err
		}
		if !transaction.MarkRollbackOnly(ctx) {
			t.Error("MarkRollbackOnly reported no active transaction")
		}
		return nil
	})
	if !errors.Is(err, transaction.ErrRollbackOnly) {
		t.Fatalf("error = %v, want ErrRollbackOnly", err)
	}
	if widgetExists(t, database, "doomed") {
		t.Error("write survived a rollback-only transaction")
	}
}

func TestMarkRollbackOnlyReportsNoActiveTransaction(t *testing.T) {
	if transaction.MarkRollbackOnly(t.Context()) {
		t.Error("MarkRollbackOnly reported an active transaction outside Run")
	}
}

func TestStatusSetRollbackOnlyRollsBack(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)

	err := manager.Run(t.Context(), func(ctx context.Context) error {
		_ = insertWidget(ctx, manager.Querier(ctx), "doomed")
		status, ok := transaction.StatusFromContext(ctx)
		if !ok {
			t.Fatal("no status in an active transaction")
		}
		status.SetRollbackOnly()
		if !status.IsRollbackOnly() {
			t.Error("status does not report rollback-only after SetRollbackOnly")
		}
		return nil
	})
	if !errors.Is(err, transaction.ErrRollbackOnly) {
		t.Fatalf("error = %v, want ErrRollbackOnly", err)
	}
	if widgetExists(t, database, "doomed") {
		t.Error("write survived SetRollbackOnly")
	}
}

func TestStatusFromContextReportsNoActiveTransaction(t *testing.T) {
	if _, ok := transaction.StatusFromContext(t.Context()); ok {
		t.Error("StatusFromContext reported an active transaction outside Run")
	}
}

func TestStatusReflectsNewTransactionAndName(t *testing.T) {
	manager := transaction.NewManager(openDatabase(t))
	err := manager.Run(t.Context(), func(ctx context.Context) error {
		status, ok := transaction.StatusFromContext(ctx)
		if !ok {
			t.Fatal("no status in an active transaction")
		}
		if !status.IsNewTransaction() {
			t.Error("outermost Run does not report a new transaction")
		}
		if status.HasSavepoint() {
			t.Error("outermost Run wrongly reports a savepoint")
		}
		if status.Name() != "registration" {
			t.Errorf("name = %q, want %q", status.Name(), "registration")
		}
		return nil
	}, transaction.WithName("registration"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
}

func TestStatusReflectsJoinAndNested(t *testing.T) {
	manager := transaction.NewManager(openDatabase(t))
	err := manager.Run(t.Context(), func(outer context.Context) error {
		joinErr := manager.Run(outer, func(inner context.Context) error {
			status, _ := transaction.StatusFromContext(inner)
			if status.IsNewTransaction() {
				t.Error("joined Run reports a new transaction")
			}
			if status.HasSavepoint() {
				t.Error("joined Run reports a savepoint")
			}
			return nil
		})
		if joinErr != nil {
			return joinErr
		}
		return manager.Run(outer, func(nested context.Context) error {
			status, _ := transaction.StatusFromContext(nested)
			if status.IsNewTransaction() {
				t.Error("nested Run reports a new transaction")
			}
			if !status.HasSavepoint() {
				t.Error("nested Run does not report a savepoint")
			}
			return nil
		}, transaction.WithPropagation(transaction.Nested))
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
}

func TestJoinWithConflictingIsolationFails(t *testing.T) {
	manager := transaction.NewManager(openDatabase(t))
	err := manager.Run(t.Context(), func(outer context.Context) error {
		return manager.Run(outer, func(context.Context) error {
			t.Error("work ran despite an incompatible isolation join")
			return nil
		}, transaction.WithIsolation(sql.LevelSerializable))
	}, transaction.WithIsolation(sql.LevelReadCommitted))
	if !errors.Is(err, transaction.ErrIncompatibleJoin) {
		t.Fatalf("error = %v, want ErrIncompatibleJoin", err)
	}
}

func TestJoinWithReadOnlyOverReadWriteFails(t *testing.T) {
	manager := transaction.NewManager(openDatabase(t))
	err := manager.Run(t.Context(), func(outer context.Context) error {
		return manager.Run(outer, func(context.Context) error {
			t.Error("work ran despite an incompatible read-only join")
			return nil
		}, transaction.ReadOnly())
	})
	if !errors.Is(err, transaction.ErrIncompatibleJoin) {
		t.Fatalf("error = %v, want ErrIncompatibleJoin", err)
	}
}

func TestJoinWithMatchingIsolationSucceeds(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)
	err := manager.Run(t.Context(), func(outer context.Context) error {
		return manager.Run(outer, func(inner context.Context) error {
			return insertWidget(inner, manager.Querier(inner), "compatible")
		}, transaction.WithIsolation(sql.LevelSerializable))
	}, transaction.WithIsolation(sql.LevelSerializable))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !widgetExists(t, database, "compatible") {
		t.Error("compatible join did not commit")
	}
}
