package integration

import (
	"context"
	"errors"
	"slices"
	"testing"

	"github.com/cgardev/gokeel/transaction"
)

// recordingListener captures the lifecycle events a Manager reports, so a test
// can assert which steps fired and in what order. The after-step events carry
// the step outcome (ok or error).
func recordingListener(events *[]string) transaction.ExecutionListener {
	return transaction.ExecutionListener{
		BeforeBegin: func(context.Context, transaction.TransactionStatus) {
			*events = append(*events, "before-begin")
		},
		AfterBegin: func(_ context.Context, _ transaction.TransactionStatus, err error) {
			*events = append(*events, "after-begin:"+stepOutcome(err))
		},
		BeforeCommit: func(context.Context, transaction.TransactionStatus) {
			*events = append(*events, "before-commit")
		},
		AfterCommit: func(_ context.Context, _ transaction.TransactionStatus, err error) {
			*events = append(*events, "after-commit:"+stepOutcome(err))
		},
		BeforeRollback: func(context.Context, transaction.TransactionStatus) {
			*events = append(*events, "before-rollback")
		},
		AfterRollback: func(_ context.Context, _ transaction.TransactionStatus, err error) {
			*events = append(*events, "after-rollback:"+stepOutcome(err))
		},
	}
}

func stepOutcome(err error) string {
	if err != nil {
		return "error"
	}
	return "ok"
}

func TestExecutionListenerObservesCommit(t *testing.T) {
	var events []string
	database := openDatabase(t)
	manager := transaction.NewManager(database, recordingListener(&events))

	err := manager.Run(t.Context(), func(ctx context.Context) error {
		return insertWidget(ctx, manager.Querier(ctx), "one")
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	want := []string{"before-begin", "after-begin:ok", "before-commit", "after-commit:ok"}
	if !slices.Equal(events, want) {
		t.Errorf("commit events = %v, want %v", events, want)
	}
}

func TestExecutionListenerObservesRollback(t *testing.T) {
	var events []string
	database := openDatabase(t)
	manager := transaction.NewManager(database, recordingListener(&events))

	_ = manager.Run(t.Context(), func(context.Context) error {
		return errors.New("boom")
	})

	want := []string{"before-begin", "after-begin:ok", "before-rollback", "after-rollback:ok"}
	if !slices.Equal(events, want) {
		t.Errorf("rollback events = %v, want %v", events, want)
	}
}

// TestExecutionListenerDoesNotFireForJoinedRun asserts the lifecycle hooks fire
// once for the outermost transaction and not for a joining Run, which performs
// no physical begin or commit of its own.
func TestExecutionListenerDoesNotFireForJoinedRun(t *testing.T) {
	var events []string
	database := openDatabase(t)
	manager := transaction.NewManager(database, recordingListener(&events))

	err := manager.Run(t.Context(), func(outer context.Context) error {
		return manager.Run(outer, func(inner context.Context) error {
			return insertWidget(inner, manager.Querier(inner), "one")
		})
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}

	want := []string{"before-begin", "after-begin:ok", "before-commit", "after-commit:ok"}
	if !slices.Equal(events, want) {
		t.Errorf("joined-run events = %v, want %v (the join must not begin or commit)", events, want)
	}
}

// TestExecutionListenerPanicDoesNotDisturbTheTransaction asserts a panicking
// observation hook is recovered and does not turn a committed transaction into a
// caller-visible failure.
func TestExecutionListenerPanicDoesNotDisturbTheTransaction(t *testing.T) {
	database := openDatabase(t)
	listener := transaction.ExecutionListener{
		AfterCommit: func(context.Context, transaction.TransactionStatus, error) {
			panic("listener boom")
		},
	}
	manager := transaction.NewManager(database, listener)

	err := manager.Run(t.Context(), func(ctx context.Context) error {
		return insertWidget(ctx, manager.Querier(ctx), "one")
	})
	if err != nil {
		t.Fatalf("run surfaced the listener panic: %v", err)
	}
	if got := countWidgets(t, database); got != 1 {
		t.Errorf("widgets after commit = %d, want 1 (a listener panic must not roll back)", got)
	}
}

func TestExecutionListenerSeesTransactionStatus(t *testing.T) {
	database := openDatabase(t)
	var name string
	var newTransaction bool
	listener := transaction.ExecutionListener{
		BeforeCommit: func(_ context.Context, status transaction.TransactionStatus) {
			name = status.Name()
			newTransaction = status.IsNewTransaction()
		},
	}
	manager := transaction.NewManager(database, listener)

	err := manager.Run(t.Context(), func(ctx context.Context) error {
		return insertWidget(ctx, manager.Querier(ctx), "one")
	}, transaction.WithName("create-widget"))
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if name != "create-widget" {
		t.Errorf("status name = %q, want %q", name, "create-widget")
	}
	if !newTransaction {
		t.Error("status.IsNewTransaction() = false, want true for a new transaction")
	}
}

// TestExecutionListenerContextResolvesQuerier asserts the context handed to the
// commit hooks resolves as documented: the before-commit context still resolves
// to the open transaction, while the after-commit context is detached and
// resolves to the database.
func TestExecutionListenerContextResolvesQuerier(t *testing.T) {
	database := openDatabase(t)
	var beforeResolvesToTransaction, afterResolvesToDatabase bool
	var manager *transaction.Manager
	listener := transaction.ExecutionListener{
		BeforeCommit: func(ctx context.Context, _ transaction.TransactionStatus) {
			beforeResolvesToTransaction = manager.Querier(ctx) != database
		},
		AfterCommit: func(ctx context.Context, _ transaction.TransactionStatus, _ error) {
			afterResolvesToDatabase = manager.Querier(ctx) == database
		},
	}
	manager = transaction.NewManager(database, listener)

	err := manager.Run(t.Context(), func(ctx context.Context) error {
		return insertWidget(ctx, manager.Querier(ctx), "one")
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if !beforeResolvesToTransaction {
		t.Error("before-commit context did not resolve to the transaction")
	}
	if !afterResolvesToDatabase {
		t.Error("after-commit context did not resolve to the database")
	}
}
