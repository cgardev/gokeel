package integration

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/cgardev/gokeel/transaction"
)

// openDeferredConstraintDatabase opens a database whose schema carries a
// deferred foreign key, so a reference to a missing row is accepted by the
// statement and rejected only at COMMIT. This is the deterministic way to drive
// a transaction whose work succeeds but whose commit fails.
func openDeferredConstraintDatabase(t *testing.T) *sql.DB {
	t.Helper()
	path := filepath.Join(t.TempDir(), "constraint.db")
	dataSourceName := "file:" + path +
		"?_txlock=immediate&_pragma=busy_timeout(5000)&_pragma=foreign_keys(1)"
	database, err := sql.Open("sqlite", dataSourceName)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	schema := []string{
		"CREATE TABLE parent (id INTEGER PRIMARY KEY)",
		"CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER " +
			"REFERENCES parent(id) DEFERRABLE INITIALLY DEFERRED)",
	}
	for _, statement := range schema {
		if _, err := database.ExecContext(t.Context(), statement); err != nil {
			t.Fatalf("create schema: %v", err)
		}
	}
	return database
}

func TestIsolationLevelsAreAppliedAndCommit(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)

	levels := []struct {
		name  string
		level sql.IsolationLevel
	}{
		{"default", sql.LevelDefault},
		{"read-committed", sql.LevelReadCommitted},
		{"repeatable-read", sql.LevelRepeatableRead},
		{"serializable", sql.LevelSerializable},
	}
	for index, isolation := range levels {
		id := fmt.Sprintf("isolation-%d", index)
		err := manager.Run(t.Context(), func(ctx context.Context) error {
			return insertWidget(ctx, manager.Querier(ctx), id)
		}, transaction.WithIsolation(isolation.level))
		if err != nil {
			t.Fatalf("isolation %s: %v", isolation.name, err)
		}
		if !widgetExists(t, database, id) {
			t.Errorf("isolation %s did not commit", isolation.name)
		}
	}
}

// TestReadOnlyTransactionReads exercises the read-only path. The modernc SQLite
// driver treats read-only as an advisory hint and does not refuse writes, so
// the test asserts only that a read-only transaction reads and commits.
func TestReadOnlyTransactionReads(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)
	if err := manager.Run(t.Context(), func(ctx context.Context) error {
		return insertWidget(ctx, manager.Querier(ctx), "seed")
	}); err != nil {
		t.Fatalf("seed: %v", err)
	}

	var count int
	err := manager.Run(t.Context(), func(ctx context.Context) error {
		rows, err := manager.Querier(ctx).QueryContext(ctx, "SELECT COUNT(*) FROM widget")
		if err != nil {
			return err
		}
		defer rows.Close()
		if rows.Next() {
			if err := rows.Scan(&count); err != nil {
				return err
			}
		}
		return rows.Err()
	}, transaction.ReadOnly())
	if err != nil {
		t.Fatalf("read-only run: %v", err)
	}
	if count != 1 {
		t.Errorf("count in read-only transaction = %d, want 1", count)
	}
}

func TestTimeoutBoundsTheTransactionContext(t *testing.T) {
	manager := transaction.NewManager(openDatabase(t))
	err := manager.Run(t.Context(), func(ctx context.Context) error {
		if _, hasDeadline := ctx.Deadline(); !hasDeadline {
			t.Error("work context carries no deadline under WithTimeout")
		}
		return nil
	}, transaction.WithTimeout(time.Second))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
}

func TestTimeoutRollsBackWhenTheDeadlineElapses(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)

	err := manager.Run(t.Context(), func(ctx context.Context) error {
		if err := insertWidget(ctx, manager.Querier(ctx), "slow"); err != nil {
			return err
		}
		// Wait past the deadline; the bound context is cancelled, so the
		// transaction must roll back.
		<-ctx.Done()
		return ctx.Err()
	}, transaction.WithTimeout(50*time.Millisecond))
	if !errors.Is(err, context.DeadlineExceeded) {
		t.Fatalf("error = %v, want context.DeadlineExceeded", err)
	}
	if widgetExists(t, database, "slow") {
		t.Error("write survived a timed-out transaction")
	}
}

func TestNoRollbackForErrorCommitsAndReturnsTheError(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)
	skip := errors.New("skip")

	err := manager.Run(t.Context(), func(ctx context.Context) error {
		if err := insertWidget(ctx, manager.Querier(ctx), "kept"); err != nil {
			return err
		}
		return skip
	}, transaction.NoRollbackForError(skip))
	if !errors.Is(err, skip) {
		t.Fatalf("error = %v, want skip", err)
	}
	if !widgetExists(t, database, "kept") {
		t.Error("no-rollback work was discarded")
	}
}

func TestRollbackForAnUnmatchedError(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)
	skip := errors.New("skip")
	other := errors.New("other")

	err := manager.Run(t.Context(), func(ctx context.Context) error {
		if err := insertWidget(ctx, manager.Querier(ctx), "discarded"); err != nil {
			return err
		}
		return other
	}, transaction.NoRollbackForError(skip))
	if !errors.Is(err, other) {
		t.Fatalf("error = %v, want other", err)
	}
	if widgetExists(t, database, "discarded") {
		t.Error("unmatched error did not roll back")
	}
}

func TestNoRollbackForErrorInAJoinDoesNotPoisonTheOuter(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)
	skip := errors.New("skip")

	err := manager.Run(t.Context(), func(outer context.Context) error {
		if err := insertWidget(outer, manager.Querier(outer), "outer"); err != nil {
			return err
		}
		joinErr := manager.Run(outer, func(context.Context) error {
			return skip
		}, transaction.NoRollbackForError(skip))
		if !errors.Is(joinErr, skip) {
			t.Errorf("join error = %v, want skip", joinErr)
		}
		return nil
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !widgetExists(t, database, "outer") {
		t.Error("a no-rollback join error wrongly aborted the outer transaction")
	}
}

func TestNoRollbackForFunc(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)
	transient := errors.New("transient outage")

	err := manager.Run(t.Context(), func(ctx context.Context) error {
		if err := insertWidget(ctx, manager.Querier(ctx), "func-kept"); err != nil {
			return err
		}
		return transient
	}, transaction.NoRollbackForFunc(func(err error) bool {
		return err != nil && strings.Contains(err.Error(), "transient")
	}))
	if !errors.Is(err, transient) {
		t.Fatalf("error = %v, want transient", err)
	}
	if !widgetExists(t, database, "func-kept") {
		t.Error("no-rollback (func) work was discarded")
	}
}
