package integration

import (
	"context"
	"errors"
	"testing"

	"github.com/cgardev/gokeel/transaction"
)

func TestSupportsJoinsAnActiveTransaction(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)

	err := manager.Run(t.Context(), func(outer context.Context) error {
		outerQuerier := manager.Querier(outer)
		return manager.Run(outer, func(inner context.Context) error {
			if manager.Querier(inner) != outerQuerier {
				t.Error("Supports did not join the active transaction")
			}
			return insertWidget(inner, manager.Querier(inner), "joined")
		}, transaction.WithPropagation(transaction.Supports))
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !widgetExists(t, database, "joined") {
		t.Error("joined write was not committed")
	}
}

func TestSupportsRunsWithoutATransactionWhenNoneIsActive(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)

	// The write auto-commits because there is no transaction to roll back,
	// even though work then returns an error.
	err := manager.Run(t.Context(), func(ctx context.Context) error {
		if manager.Querier(ctx) != database {
			t.Error("Supports without a transaction did not resolve to the database")
		}
		if err := insertWidget(ctx, manager.Querier(ctx), "auto"); err != nil {
			return err
		}
		return errors.New("work failed after the auto-commit")
	}, transaction.WithPropagation(transaction.Supports))
	if err == nil {
		t.Fatal("run reported no error")
	}
	if !widgetExists(t, database, "auto") {
		t.Error("non-transactional write was rolled back, but nothing wraps it")
	}
}

func TestMandatoryFailsWithoutAnActiveTransaction(t *testing.T) {
	manager := transaction.NewManager(openDatabase(t))
	err := manager.Run(t.Context(), func(context.Context) error {
		t.Error("work ran without an active transaction")
		return nil
	}, transaction.WithPropagation(transaction.Mandatory))
	if !errors.Is(err, transaction.ErrTransactionRequired) {
		t.Fatalf("error = %v, want ErrTransactionRequired", err)
	}
}

func TestMandatoryJoinsAnActiveTransaction(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)
	err := manager.Run(t.Context(), func(outer context.Context) error {
		return manager.Run(outer, func(inner context.Context) error {
			return insertWidget(inner, manager.Querier(inner), "mandatory")
		}, transaction.WithPropagation(transaction.Mandatory))
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !widgetExists(t, database, "mandatory") {
		t.Error("mandatory join was not committed")
	}
}

func TestNeverFailsWithinAnActiveTransaction(t *testing.T) {
	manager := transaction.NewManager(openDatabase(t))
	err := manager.Run(t.Context(), func(outer context.Context) error {
		return manager.Run(outer, func(context.Context) error {
			t.Error("work ran inside an active transaction")
			return nil
		}, transaction.WithPropagation(transaction.Never))
	})
	if !errors.Is(err, transaction.ErrTransactionNotAllowed) {
		t.Fatalf("error = %v, want ErrTransactionNotAllowed", err)
	}
}

func TestNeverRunsWithoutATransaction(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)
	err := manager.Run(t.Context(), func(ctx context.Context) error {
		return insertWidget(ctx, manager.Querier(ctx), "never")
	}, transaction.WithPropagation(transaction.Never))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !widgetExists(t, database, "never") {
		t.Error("non-transactional write did not persist")
	}
}

func TestNestedFailureRollsBackToTheSavepointAndKeepsTheOuter(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)

	err := manager.Run(t.Context(), func(outer context.Context) error {
		if err := insertWidget(outer, manager.Querier(outer), "outer-before"); err != nil {
			return err
		}

		// The nested failure is contained: it rolls back only its own write.
		nestedErr := manager.Run(outer, func(inner context.Context) error {
			if err := insertWidget(inner, manager.Querier(inner), "nested"); err != nil {
				return err
			}
			return errors.New("nested failure")
		}, transaction.WithPropagation(transaction.Nested))
		if nestedErr == nil {
			t.Error("nested run reported no error")
		}

		return insertWidget(outer, manager.Querier(outer), "outer-after")
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	if !widgetExists(t, database, "outer-before") || !widgetExists(t, database, "outer-after") {
		t.Error("outer writes were lost; the nested rollback aborted the whole transaction")
	}
	if widgetExists(t, database, "nested") {
		t.Error("nested write survived; it should have rolled back to the savepoint")
	}
}

func TestNestedSuccessPersistsWithinTheOuterTransaction(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)
	err := manager.Run(t.Context(), func(outer context.Context) error {
		return manager.Run(outer, func(inner context.Context) error {
			return insertWidget(inner, manager.Querier(inner), "nested-ok")
		}, transaction.WithPropagation(transaction.Nested))
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !widgetExists(t, database, "nested-ok") {
		t.Error("nested success was not committed with the outer transaction")
	}
}

func TestNestedWorkRollsBackWithTheOuterTransaction(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)
	err := manager.Run(t.Context(), func(outer context.Context) error {
		if err := manager.Run(outer, func(inner context.Context) error {
			return insertWidget(inner, manager.Querier(inner), "nested-released")
		}, transaction.WithPropagation(transaction.Nested)); err != nil {
			return err
		}
		return errors.New("outer failure")
	})
	if err == nil {
		t.Fatal("run reported no error")
	}
	if widgetExists(t, database, "nested-released") {
		t.Error("released nested work survived an outer rollback")
	}
}

func TestNestedBeginsANewTransactionWhenNoneIsActive(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)

	if err := manager.Run(t.Context(), func(ctx context.Context) error {
		return insertWidget(ctx, manager.Querier(ctx), "nested-new")
	}, transaction.WithPropagation(transaction.Nested)); err != nil {
		t.Fatalf("run: %v", err)
	}
	if !widgetExists(t, database, "nested-new") {
		t.Error("nested-as-new did not commit")
	}

	sentinel := errors.New("boom")
	err := manager.Run(t.Context(), func(ctx context.Context) error {
		if err := insertWidget(ctx, manager.Querier(ctx), "nested-new-rollback"); err != nil {
			return err
		}
		return sentinel
	}, transaction.WithPropagation(transaction.Nested))
	if !errors.Is(err, sentinel) {
		t.Fatalf("error = %v, want sentinel", err)
	}
	if widgetExists(t, database, "nested-new-rollback") {
		t.Error("nested-as-new did not roll back")
	}
}

func TestNestedPanicRollsBackTheWholeTransaction(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)

	func() {
		defer func() {
			if recover() == nil {
				t.Error("panic was not re-raised")
			}
		}()
		_ = manager.Run(t.Context(), func(outer context.Context) error {
			if err := insertWidget(outer, manager.Querier(outer), "outer"); err != nil {
				return err
			}
			return manager.Run(outer, func(inner context.Context) error {
				_ = insertWidget(inner, manager.Querier(inner), "nested")
				panic("nested boom")
			}, transaction.WithPropagation(transaction.Nested))
		})
	}()

	if widgetExists(t, database, "outer") || widgetExists(t, database, "nested") {
		t.Error("a panic in nested work did not roll back the whole transaction")
	}
}

func TestNestedKeepsWorkForANoRollbackError(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)
	skip := errors.New("skip")

	err := manager.Run(t.Context(), func(outer context.Context) error {
		nestedErr := manager.Run(outer, func(inner context.Context) error {
			if err := insertWidget(inner, manager.Querier(inner), "nested-kept"); err != nil {
				return err
			}
			return skip
		},
			transaction.WithPropagation(transaction.Nested),
			transaction.NoRollbackForError(skip),
		)
		if !errors.Is(nestedErr, skip) {
			t.Errorf("nested error = %v, want skip", nestedErr)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !widgetExists(t, database, "nested-kept") {
		t.Error("no-rollback nested work was discarded")
	}
}
