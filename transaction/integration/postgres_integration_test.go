//go:build integration

package integration

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/cgardev/gokeel/transaction"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// postgresImages lists the two most recent PostgreSQL releases the unit of work
// is verified against. Adjust the tags here if a registry lacks one of them.
var postgresImages = []string{"postgres:18-alpine", "postgres:17-alpine"}

// TestPostgresIntegration runs the unit of work against a real PostgreSQL
// server, where the database honors options that the SQLite driver only treats
// as hints: isolation levels reach the session, read-only transactions refuse
// writes, and a deferred constraint fails the commit rather than the statement.
//
// It is gated behind the integration build tag because it needs a Docker
// daemon. Run it with: go test -tags=integration ./...
func TestPostgresIntegration(t *testing.T) {
	for _, image := range postgresImages {
		t.Run(image, func(t *testing.T) {
			database := startPostgres(t, image)
			createPostgresSchema(t, database)
			manager := transaction.NewManager(database)
			runManagerBattery(t, manager, database)
		})
	}
}

func startPostgres(t *testing.T, image string) *sql.DB {
	t.Helper()
	ctx := context.Background()
	container, err := postgres.Run(ctx, image,
		postgres.WithDatabase("transaction"),
		postgres.WithUsername("test"),
		postgres.WithPassword("test"),
		testcontainers.WithWaitStrategy(
			wait.ForLog("database system is ready to accept connections").
				WithOccurrence(2).
				WithStartupTimeout(120*time.Second),
		),
	)
	if err != nil {
		t.Fatalf("start postgres %s: %v", image, err)
	}
	t.Cleanup(func() {
		if err := container.Terminate(context.Background()); err != nil {
			t.Logf("terminate postgres %s: %v", image, err)
		}
	})

	connString, err := container.ConnectionString(ctx, "sslmode=disable")
	if err != nil {
		t.Fatalf("connection string: %v", err)
	}
	database, err := sql.Open("pgx", connString)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { database.Close() })
	if err := database.PingContext(ctx); err != nil {
		t.Fatalf("ping postgres: %v", err)
	}
	return database
}

func createPostgresSchema(t *testing.T, database *sql.DB) {
	t.Helper()
	statements := []string{
		"CREATE TABLE widget (id TEXT PRIMARY KEY)",
		"CREATE TABLE parent (id INTEGER PRIMARY KEY)",
		"CREATE TABLE child (id INTEGER PRIMARY KEY, parent_id INTEGER " +
			"REFERENCES parent(id) DEFERRABLE INITIALLY DEFERRED)",
	}
	for _, statement := range statements {
		if _, err := database.ExecContext(context.Background(), statement); err != nil {
			t.Fatalf("create schema: %v", err)
		}
	}
}

func pgInsertWidget(ctx context.Context, querier transaction.Querier, id string) error {
	_, err := querier.ExecContext(ctx, "INSERT INTO widget (id) VALUES ($1)", id)
	return err
}

func pgWidgetExists(t *testing.T, database *sql.DB, id string) bool {
	t.Helper()
	var count int
	row := database.QueryRowContext(context.Background(), "SELECT count(*) FROM widget WHERE id = $1", id)
	if err := row.Scan(&count); err != nil {
		t.Fatalf("query widget %s: %v", id, err)
	}
	return count > 0
}

func pgQueryString(
	t *testing.T, ctx context.Context, querier transaction.Querier, query string,
) string {
	t.Helper()
	rows, err := querier.QueryContext(ctx, query)
	if err != nil {
		t.Fatalf("query %q: %v", query, err)
	}
	defer rows.Close()
	var value string
	if rows.Next() {
		if err := rows.Scan(&value); err != nil {
			t.Fatalf("scan %q: %v", query, err)
		}
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("rows %q: %v", query, err)
	}
	return value
}

// runManagerBattery exercises the cross-cutting behaviors of the unit of work
// against the live database the subtest provides.
func runManagerBattery(t *testing.T, manager *transaction.Manager, database *sql.DB) {
	ctx := context.Background()

	t.Run("commit persists and rollback discards", func(t *testing.T) {
		if err := manager.Run(ctx, func(ctx context.Context) error {
			return pgInsertWidget(ctx, manager.Querier(ctx), "commit")
		}); err != nil {
			t.Fatalf("run: %v", err)
		}
		if !pgWidgetExists(t, database, "commit") {
			t.Error("committed write did not persist")
		}

		sentinel := errors.New("boom")
		err := manager.Run(ctx, func(ctx context.Context) error {
			if err := pgInsertWidget(ctx, manager.Querier(ctx), "rollback"); err != nil {
				return err
			}
			return sentinel
		})
		if !errors.Is(err, sentinel) {
			t.Fatalf("error = %v, want sentinel", err)
		}
		if pgWidgetExists(t, database, "rollback") {
			t.Error("rolled-back write persisted")
		}
	})

	t.Run("required join shares the transaction", func(t *testing.T) {
		err := manager.Run(ctx, func(outer context.Context) error {
			outerQuerier := manager.Querier(outer)
			return manager.Run(outer, func(inner context.Context) error {
				if manager.Querier(inner) != outerQuerier {
					t.Error("inner run did not join the outer transaction")
				}
				return pgInsertWidget(inner, manager.Querier(inner), "joined")
			})
		})
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if !pgWidgetExists(t, database, "joined") {
			t.Error("joined write was not committed")
		}
	})

	t.Run("nested savepoint contains its failure", func(t *testing.T) {
		err := manager.Run(ctx, func(outer context.Context) error {
			if err := pgInsertWidget(outer, manager.Querier(outer), "outer-before"); err != nil {
				return err
			}
			_ = manager.Run(outer, func(inner context.Context) error {
				if err := pgInsertWidget(inner, manager.Querier(inner), "nested"); err != nil {
					return err
				}
				return errors.New("nested failure")
			}, transaction.WithPropagation(transaction.Nested))
			return pgInsertWidget(outer, manager.Querier(outer), "outer-after")
		})
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if !pgWidgetExists(t, database, "outer-before") || !pgWidgetExists(t, database, "outer-after") {
			t.Error("outer writes were lost to the nested rollback")
		}
		if pgWidgetExists(t, database, "nested") {
			t.Error("nested write survived the savepoint rollback")
		}
	})

	t.Run("isolation level reaches the session", func(t *testing.T) {
		var level string
		err := manager.Run(ctx, func(ctx context.Context) error {
			level = pgQueryString(t, ctx, manager.Querier(ctx), "SHOW transaction_isolation")
			return nil
		}, transaction.WithIsolation(sql.LevelSerializable))
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		if level != "serializable" {
			t.Errorf("transaction_isolation = %q, want %q", level, "serializable")
		}
	})

	t.Run("read-only transaction refuses writes", func(t *testing.T) {
		err := manager.Run(ctx, func(ctx context.Context) error {
			return pgInsertWidget(ctx, manager.Querier(ctx), "read-only")
		}, transaction.ReadOnly())
		if err == nil {
			t.Fatal("postgres accepted a write inside a read-only transaction")
		}
		if pgWidgetExists(t, database, "read-only") {
			t.Error("read-only write persisted")
		}
	})

	t.Run("timeout rolls back an overrunning statement", func(t *testing.T) {
		err := manager.Run(ctx, func(ctx context.Context) error {
			_, execErr := manager.Querier(ctx).ExecContext(ctx, "SELECT pg_sleep(3)")
			return execErr
		}, transaction.WithTimeout(250*time.Millisecond))
		if err == nil {
			t.Fatal("overrunning statement did not fail under the timeout")
		}
	})

	t.Run("deferred constraint fails the commit", func(t *testing.T) {
		completion := transaction.StatusCommitted
		afterCommitRan := false
		err := manager.Run(ctx, func(ctx context.Context) error {
			transaction.RegisterAfterCommit(ctx, func(context.Context) {
				afterCommitRan = true
			})
			transaction.RegisterAfterCompletion(ctx,
				func(_ context.Context, status transaction.Status) {
					completion = status
				})
			_, execErr := manager.Querier(ctx).ExecContext(ctx,
				"INSERT INTO child (id, parent_id) VALUES (1, 999)")
			return execErr
		})
		if err == nil {
			t.Fatal("commit succeeded despite a deferred constraint violation")
		}
		if afterCommitRan {
			t.Error("after-commit ran even though the commit failed")
		}
		if completion != transaction.StatusUnknown {
			t.Errorf("completion status = %s, want Unknown", completion)
		}
	})

	t.Run("mandatory propagation requires a transaction", func(t *testing.T) {
		err := manager.Run(ctx, func(context.Context) error {
			t.Error("work ran without an active transaction")
			return nil
		}, transaction.WithPropagation(transaction.Mandatory))
		if !errors.Is(err, transaction.ErrTransactionRequired) {
			t.Fatalf("error = %v, want ErrTransactionRequired", err)
		}
	})

	t.Run("mark rollback only discards without an error", func(t *testing.T) {
		err := manager.Run(ctx, func(ctx context.Context) error {
			if err := pgInsertWidget(ctx, manager.Querier(ctx), "marked"); err != nil {
				return err
			}
			transaction.MarkRollbackOnly(ctx)
			return nil
		})
		if !errors.Is(err, transaction.ErrRollbackOnly) {
			t.Fatalf("error = %v, want ErrRollbackOnly", err)
		}
		if pgWidgetExists(t, database, "marked") {
			t.Error("write survived a rollback-only transaction")
		}
	})

	t.Run("synchronization phases run in order on commit", func(t *testing.T) {
		var phases []string
		err := manager.Run(ctx, func(ctx context.Context) error {
			transaction.RegisterBeforeCommit(ctx, func(context.Context) error {
				phases = append(phases, "beforeCommit")
				return nil
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
		if err != nil {
			t.Fatalf("run: %v", err)
		}
		want := []string{"beforeCommit", "afterCommit", "afterCompletion:Committed"}
		if len(phases) != len(want) || phases[0] != want[0] || phases[1] != want[1] || phases[2] != want[2] {
			t.Errorf("phases = %v, want %v", phases, want)
		}
	})
}
