package integration

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"

	"github.com/cgardev/gokeel/transaction"

	_ "modernc.org/sqlite"
)

// openDatabase opens a temporary SQLite database with the same connection
// settings the application uses, so the tests exercise the immediate write
// lock that makes REQUIRED the only safe propagation.
func openDatabase(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "transaction.db")
	dataSourceName := "file:" + path +
		"?_txlock=immediate" +
		"&_pragma=journal_mode(WAL)" +
		"&_pragma=busy_timeout(5000)"
	database, err := sql.Open("sqlite", dataSourceName)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	_, err = database.ExecContext(t.Context(), "CREATE TABLE widget (id TEXT PRIMARY KEY)")
	if err != nil {
		t.Fatalf("create table: %v", err)
	}
	return database
}

func insertWidget(ctx context.Context, querier interface {
	ExecContext(context.Context, string, ...any) (sql.Result, error)
}, id string) error {
	_, err := querier.ExecContext(ctx, "INSERT INTO widget (id) VALUES (?)", id)
	return err
}

func countWidgets(t *testing.T, database *sql.DB) int {
	t.Helper()
	var count int
	row := database.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM widget")
	if err := row.Scan(&count); err != nil {
		t.Fatalf("count widgets: %v", err)
	}
	return count
}

func widgetExists(t *testing.T, database *sql.DB, id string) bool {
	t.Helper()
	var count int
	row := database.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM widget WHERE id = ?", id)
	if err := row.Scan(&count); err != nil {
		t.Fatalf("query widget %s: %v", id, err)
	}
	return count > 0
}

func TestRunCommitsOnSuccess(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)

	err := manager.Run(t.Context(), func(ctx context.Context) error {
		return insertWidget(ctx, manager.Querier(ctx), "one")
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := countWidgets(t, database); got != 1 {
		t.Errorf("widgets after commit = %d, want 1", got)
	}
}

func TestRunRollsBackOnError(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)
	sentinel := errors.New("boom")

	err := manager.Run(t.Context(), func(ctx context.Context) error {
		if err := insertWidget(ctx, manager.Querier(ctx), "one"); err != nil {
			return err
		}
		return sentinel
	})
	if !errors.Is(err, sentinel) {
		t.Fatalf("run error = %v, want sentinel", err)
	}
	if got := countWidgets(t, database); got != 0 {
		t.Errorf("widgets after rollback = %d, want 0", got)
	}
}

func TestRunRollsBackOnPanic(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)

	func() {
		defer func() {
			if recover() == nil {
				t.Error("panic was not re-raised")
			}
		}()
		_ = manager.Run(t.Context(), func(ctx context.Context) error {
			if err := insertWidget(ctx, manager.Querier(ctx), "one"); err != nil {
				return err
			}
			panic("boom")
		})
	}()

	if got := countWidgets(t, database); got != 0 {
		t.Errorf("widgets after panic = %d, want 0", got)
	}
}

func TestQuerierResolvesToDatabaseOutsideRun(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)

	if manager.Querier(t.Context()) != database {
		t.Error("querier outside a unit of work is not the database")
	}
}

func TestJoinedRunSharesTheSameTransaction(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)

	err := manager.Run(t.Context(), func(outer context.Context) error {
		outerQuerier := manager.Querier(outer)
		if outerQuerier == database {
			t.Error("querier inside a unit of work resolved to the database")
		}
		if err := insertWidget(outer, outerQuerier, "one"); err != nil {
			return err
		}
		return manager.Run(outer, func(inner context.Context) error {
			if manager.Querier(inner) != outerQuerier {
				t.Error("inner run did not join the outer transaction")
			}
			return insertWidget(inner, manager.Querier(inner), "two")
		})
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := countWidgets(t, database); got != 2 {
		t.Errorf("widgets after nested commit = %d, want 2", got)
	}
}

func TestSwallowedInnerErrorRollsBackTheWholeTransaction(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)

	err := manager.Run(t.Context(), func(outer context.Context) error {
		if err := insertWidget(outer, manager.Querier(outer), "one"); err != nil {
			return err
		}
		// The inner failure is deliberately swallowed; the unit must still
		// roll back because it was marked rollback only.
		_ = manager.Run(outer, func(inner context.Context) error {
			return errors.New("inner failure")
		})
		return nil
	})
	if !errors.Is(err, transaction.ErrRollbackOnly) {
		t.Fatalf("run error = %v, want ErrRollbackOnly", err)
	}
	if got := countWidgets(t, database); got != 0 {
		t.Errorf("widgets after rollback only = %d, want 0", got)
	}
}

func TestAfterCommitRunsOnlyOnCommit(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)

	committed := 0
	rolledBack := 0

	err := manager.Run(t.Context(), func(ctx context.Context) error {
		transaction.RegisterAfterCommit(ctx, func(callbackCtx context.Context) {
			committed++
			if manager.Querier(callbackCtx) != database {
				t.Error("after-commit querier is not the database")
			}
		})
		return insertWidget(ctx, manager.Querier(ctx), "one")
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	_ = manager.Run(t.Context(), func(ctx context.Context) error {
		transaction.RegisterAfterCommit(ctx, func(context.Context) { rolledBack++ })
		return errors.New("boom")
	})

	if committed != 1 {
		t.Errorf("after-commit runs on commit = %d, want 1", committed)
	}
	if rolledBack != 0 {
		t.Errorf("after-commit runs on rollback = %d, want 0", rolledBack)
	}
}

func TestAfterCompletionReportsTheStatus(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)

	var statuses []transaction.Status
	record := func(_ context.Context, status transaction.Status) {
		statuses = append(statuses, status)
	}

	_ = manager.Run(t.Context(), func(ctx context.Context) error {
		transaction.RegisterAfterCompletion(ctx, record)
		return nil
	})
	_ = manager.Run(t.Context(), func(ctx context.Context) error {
		transaction.RegisterAfterCompletion(ctx, record)
		return errors.New("boom")
	})

	want := []transaction.Status{
		transaction.StatusCommitted,
		transaction.StatusRolledBack,
	}
	if len(statuses) != len(want) || statuses[0] != want[0] || statuses[1] != want[1] {
		t.Errorf("completion statuses = %v, want %v", statuses, want)
	}
}

func TestRegisterAfterCommitReportsNoActiveUnit(t *testing.T) {
	if transaction.RegisterAfterCommit(t.Context(), func(context.Context) {}) {
		t.Error("register after-commit reported an active unit of work outside Run")
	}
}
