package integration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strconv"
	"testing"
	"time"

	"github.com/cgardev/gokeel/transaction"

	"go.uber.org/goleak"
)

// assertRunSucceedsPromptly runs a trivial write and fails if it does not
// return quickly. With the database capped to a single connection, a leaked
// transaction would hold that connection and block this run until the deadline,
// turning a leak into a deterministic failure.
func assertRunSucceedsPromptly(t *testing.T, manager *transaction.Manager, id string) {
	t.Helper()
	done := make(chan error, 1)
	go func() {
		done <- manager.Run(context.Background(), func(ctx context.Context) error {
			return insertWidget(ctx, manager.Querier(ctx), id)
		})
	}()
	select {
	case err := <-done:
		if err != nil {
			t.Fatalf("subsequent run failed: %v", err)
		}
	case <-time.After(10 * time.Second):
		t.Fatal("subsequent run blocked; an open transaction leaked the connection")
	}
}

func TestWorkPanicDoesNotLeakTheConnection(t *testing.T) {
	database := openDatabase(t)
	database.SetMaxOpenConns(1)
	manager := transaction.NewManager(database)

	func() {
		defer func() {
			if recover() == nil {
				t.Error("work panic was not re-raised")
			}
		}()
		_ = manager.Run(t.Context(), func(ctx context.Context) error {
			_ = insertWidget(ctx, manager.Querier(ctx), "work-panic")
			panic("boom")
		})
	}()

	if widgetExists(t, database, "work-panic") {
		t.Error("panicked work was not rolled back")
	}
	assertRunSucceedsPromptly(t, manager, "after-work-panic")
}

func TestBeforeCommitPanicRollsBackAndReraisesWithoutLeak(t *testing.T) {
	database := openDatabase(t)
	database.SetMaxOpenConns(1)
	manager := transaction.NewManager(database)

	reraised := false
	func() {
		defer func() {
			if recover() != nil {
				reraised = true
			}
		}()
		_ = manager.Run(t.Context(), func(ctx context.Context) error {
			if err := insertWidget(ctx, manager.Querier(ctx), "before-commit-panic"); err != nil {
				return err
			}
			transaction.RegisterBeforeCommit(ctx, func(context.Context) error {
				panic("before-commit boom")
			})
			return nil
		})
	}()

	if !reraised {
		t.Error("before-commit panic was not re-raised")
	}
	if widgetExists(t, database, "before-commit-panic") {
		t.Error("before-commit panic did not roll back the write")
	}
	assertRunSucceedsPromptly(t, manager, "after-before-commit-panic")
}

func TestBeforeCompletionPanicRollsBackWithoutLeak(t *testing.T) {
	database := openDatabase(t)
	database.SetMaxOpenConns(1)
	manager := transaction.NewManager(database)

	func() {
		defer func() { _ = recover() }()
		_ = manager.Run(t.Context(), func(ctx context.Context) error {
			if err := insertWidget(ctx, manager.Querier(ctx), "before-completion-panic"); err != nil {
				return err
			}
			transaction.RegisterBeforeCompletion(ctx, func(context.Context) {
				panic("before-completion boom")
			})
			return nil
		})
	}()

	if widgetExists(t, database, "before-completion-panic") {
		t.Error("before-completion panic did not roll back the write")
	}
	assertRunSucceedsPromptly(t, manager, "after-before-completion-panic")
}

func TestAfterCommitPanicIsIsolatedAndDoesNotFailTheTransaction(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)

	secondRan := false
	completion := transaction.StatusRolledBack
	err := manager.Run(t.Context(), func(ctx context.Context) error {
		if err := insertWidget(ctx, manager.Querier(ctx), "committed"); err != nil {
			return err
		}
		transaction.RegisterAfterCommit(ctx, func(context.Context) {
			panic("after-commit boom")
		})
		transaction.RegisterAfterCommit(ctx, func(context.Context) {
			secondRan = true
		})
		transaction.RegisterAfterCompletion(ctx,
			func(_ context.Context, status transaction.Status) {
				completion = status
			})
		return nil
	})
	if err != nil {
		t.Fatalf("a post-commit panic surfaced as a transaction failure: %v", err)
	}
	if !widgetExists(t, database, "committed") {
		t.Error("the transaction did not commit")
	}
	if !secondRan {
		t.Error("a later after-commit callback did not run after an earlier one panicked")
	}
	if completion != transaction.StatusCommitted {
		t.Errorf("completion = %s, want Committed", completion)
	}
}

func TestRollbackOnlyBeatsNoRollbackError(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)
	skip := errors.New("skip")

	err := manager.Run(t.Context(), func(ctx context.Context) error {
		if err := insertWidget(ctx, manager.Querier(ctx), "doomed"); err != nil {
			return err
		}
		transaction.MarkRollbackOnly(ctx)
		return skip
	}, transaction.NoRollbackForError(skip))
	if !errors.Is(err, skip) {
		t.Fatalf("error = %v, want skip", err)
	}
	if widgetExists(t, database, "doomed") {
		t.Error("rollback-only did not override the no-rollback rule")
	}
}

func TestNoRollbackForErrorMatchesWrappedError(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)
	base := errors.New("base")

	err := manager.Run(t.Context(), func(ctx context.Context) error {
		if err := insertWidget(ctx, manager.Querier(ctx), "kept"); err != nil {
			return err
		}
		return fmt.Errorf("wrapping: %w", base)
	}, transaction.NoRollbackForError(base))
	if !errors.Is(err, base) {
		t.Fatalf("error = %v, want base", err)
	}
	if !widgetExists(t, database, "kept") {
		t.Error("a wrapped no-rollback error was not honored")
	}
}

func TestDeeplyNestedJoinsShareOneTransaction(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)
	const depth = 256

	var recurse func(ctx context.Context, level int) error
	recurse = func(ctx context.Context, level int) error {
		if err := insertWidget(ctx, manager.Querier(ctx), "join-"+strconv.Itoa(level)); err != nil {
			return err
		}
		if level == depth {
			return nil
		}
		return manager.Run(ctx, func(inner context.Context) error {
			return recurse(inner, level+1)
		})
	}

	if err := manager.Run(t.Context(), func(ctx context.Context) error {
		return recurse(ctx, 0)
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := countWidgets(t, database); got != depth+1 {
		t.Errorf("rows = %d, want %d", got, depth+1)
	}
}

func TestDeeplyNestedSavepointsRollBackSelectively(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)
	const depth = 64

	var recurse func(ctx context.Context, level int) error
	recurse = func(ctx context.Context, level int) error {
		if err := insertWidget(ctx, manager.Querier(ctx), "sp-"+strconv.Itoa(level)); err != nil {
			return err
		}
		if level == depth {
			return errors.New("deepest failure")
		}
		// The inner failure is contained by its savepoint; the outer levels
		// still commit.
		_ = manager.Run(ctx, func(inner context.Context) error {
			return recurse(inner, level+1)
		}, transaction.WithPropagation(transaction.Nested))
		return nil
	}

	if err := manager.Run(t.Context(), func(ctx context.Context) error {
		return recurse(ctx, 0)
	}); err != nil {
		t.Fatalf("run: %v", err)
	}
	if widgetExists(t, database, "sp-"+strconv.Itoa(depth)) {
		t.Error("the deepest savepoint did not roll back")
	}
	if got := countWidgets(t, database); got != depth {
		t.Errorf("rows = %d, want %d", got, depth)
	}
}

func TestManyAfterCommitCallbacksAllRun(t *testing.T) {
	manager := transaction.NewManager(openDatabase(t))
	const count = 10_000
	ran := 0

	err := manager.Run(t.Context(), func(ctx context.Context) error {
		for range count {
			transaction.RegisterAfterCommit(ctx, func(context.Context) { ran++ })
		}
		return nil
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if ran != count {
		t.Errorf("after-commit callbacks run = %d, want %d", ran, count)
	}
}

func TestAfterCommitCallbackStartsAFreshTransaction(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)

	err := manager.Run(t.Context(), func(ctx context.Context) error {
		if err := insertWidget(ctx, manager.Querier(ctx), "outer"); err != nil {
			return err
		}
		transaction.RegisterAfterCommit(ctx, func(callbackCtx context.Context) {
			// The detached context carries no unit, so this begins its own
			// transaction, mirroring an event listener that persists a record.
			_ = manager.Run(callbackCtx, func(innerCtx context.Context) error {
				return insertWidget(innerCtx, manager.Querier(innerCtx), "from-after-commit")
			})
		})
		return nil
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !widgetExists(t, database, "outer") || !widgetExists(t, database, "from-after-commit") {
		t.Error("the re-entrant after-commit transaction did not commit")
	}
}

func TestRunLeavesNoGoroutines(t *testing.T) {
	defer goleak.VerifyNone(t)

	// A dedicated database, closed before the leak check, so the driver's pool
	// goroutines are not mistaken for a leak of the unit of work.
	path := filepath.Join(t.TempDir(), "leak.db")
	database, err := sql.Open("sqlite", "file:"+path+"?_txlock=immediate&_pragma=busy_timeout(5000)")
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	if _, err := database.ExecContext(t.Context(),
		"CREATE TABLE widget (id TEXT PRIMARY KEY)"); err != nil {
		t.Fatalf("create schema: %v", err)
	}
	manager := transaction.NewManager(database)

	for i := range 200 {
		_ = manager.Run(context.Background(), func(ctx context.Context) error {
			return insertWidget(ctx, manager.Querier(ctx), "leak-"+strconv.Itoa(i))
		})
		_ = manager.Run(context.Background(), func(context.Context) error {
			return errors.New("rolled back")
		})
		_ = manager.Run(context.Background(), func(context.Context) error {
			return nil
		}, transaction.WithTimeout(time.Second))
	}

	if err := database.Close(); err != nil {
		t.Fatalf("close database: %v", err)
	}
}
