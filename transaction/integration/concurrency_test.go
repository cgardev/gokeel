package integration

import (
	"context"
	"errors"
	"strconv"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cgardev/gokeel/transaction"
)

// These tests are written to be run under the race detector in CI
// (go test -race), where they validate that the manager has no data races
// across concurrent transactions and concurrent callback registration. Without
// the detector they still assert data integrity under concurrency.

func TestConcurrentIndependentTransactions(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)
	const workers = 24

	var group sync.WaitGroup
	failures := make(chan error, workers)
	for worker := range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			failures <- manager.Run(context.Background(), func(ctx context.Context) error {
				return insertWidget(ctx, manager.Querier(ctx), "worker-"+strconv.Itoa(worker))
			})
		}()
	}
	group.Wait()
	close(failures)
	for err := range failures {
		if err != nil {
			t.Fatalf("concurrent run: %v", err)
		}
	}
	if got := countWidgets(t, database); got != workers {
		t.Errorf("committed rows = %d, want %d", got, workers)
	}
}

func TestConcurrentTransactionsAreIsolated(t *testing.T) {
	database := openDatabase(t)
	manager := transaction.NewManager(database)
	const pairs = 16

	var group sync.WaitGroup
	for pair := range pairs {
		group.Add(2)
		go func() {
			defer group.Done()
			_ = manager.Run(context.Background(), func(ctx context.Context) error {
				return insertWidget(ctx, manager.Querier(ctx), "commit-"+strconv.Itoa(pair))
			})
		}()
		go func() {
			defer group.Done()
			_ = manager.Run(context.Background(), func(ctx context.Context) error {
				if err := insertWidget(ctx, manager.Querier(ctx), "rollback-"+strconv.Itoa(pair)); err != nil {
					return err
				}
				return errors.New("rollback")
			})
		}()
	}
	group.Wait()

	for pair := range pairs {
		if !widgetExists(t, database, "commit-"+strconv.Itoa(pair)) {
			t.Errorf("committed row commit-%d is missing", pair)
		}
		if widgetExists(t, database, "rollback-"+strconv.Itoa(pair)) {
			t.Errorf("rolled-back row rollback-%d is present", pair)
		}
	}
}

func TestConcurrentSynchronizationRegistrationIsSafe(t *testing.T) {
	manager := transaction.NewManager(openDatabase(t))
	const registrars = 32
	var counter atomic.Int64

	err := manager.Run(t.Context(), func(ctx context.Context) error {
		var group sync.WaitGroup
		for range registrars {
			group.Add(1)
			go func() {
				defer group.Done()
				transaction.RegisterAfterCommit(ctx, func(context.Context) {
					counter.Add(1)
				})
			}()
		}
		group.Wait()
		return nil
	})
	if err != nil {
		t.Fatalf("run: %v", err)
	}
	if got := counter.Load(); got != registrars {
		t.Errorf("after-commit callbacks run = %d, want %d", got, registrars)
	}
}
