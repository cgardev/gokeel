//go:build integration

package outbox

import (
	"context"
	"database/sql"
	"errors"
	"testing"
	"time"

	"github.com/cgardev/gokeel/eventbus"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// postgresImages lists the two most recent PostgreSQL releases the store is
// verified against. Adjust the tags here if a registry lacks one of them.
var postgresImages = []string{"postgres:18-alpine", "postgres:17-alpine"}

// TestPostgresOutboxIntegration runs the store against a real PostgreSQL server,
// exercising the native SQL on the dialect that needs $N placeholders. It is
// gated behind the integration build tag because it needs a Docker daemon. Run
// it with: go test -tags=integration ./...
func TestPostgresOutboxIntegration(t *testing.T) {
	for _, image := range postgresImages {
		t.Run(image, func(t *testing.T) {
			database := startPostgres(t, image)
			t.Run("archive flow", func(t *testing.T) { testArchiveFlow(t, database) })
			t.Run("resubmit flow", func(t *testing.T) { testResubmitFlow(t, database) })
		})
	}
}

// testArchiveFlow covers the create, claim, deliver-after-commit, and archive
// path: the archive insert is the most dialect-sensitive query (ON CONFLICT).
func testArchiveFlow(t *testing.T, database *sql.DB) {
	f := freshPostgresFixture(t, database, CompletionModeArchive)

	var billing, shipping []orderPlaced
	subscribeRecorder(t, f.bus, "billing", &billing)
	subscribeRecorder(t, f.bus, "shipping", &shipping)

	if err := f.publish(t.Context(), orderPlaced{OrderID: "o-1"}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	if len(billing) != 1 || len(shipping) != 1 {
		t.Fatalf("delivered billing=%d shipping=%d, want 1 and 1", len(billing), len(shipping))
	}
	if got := countRows(t, database, tableName); got != 0 {
		t.Errorf("source rows = %d, want 0 (archived)", got)
	}
	if got := countRows(t, database, archiveTableName); got != 2 {
		t.Errorf("archive rows = %d, want 2", got)
	}
}

// testResubmitFlow covers the failure path: a failed delivery stays incomplete
// and a resubmission recovers it, exercising MarkFailed, FindIncomplete, and the
// attempt-checked MarkResubmitted update.
func testResubmitFlow(t *testing.T, database *sql.DB) {
	f := freshPostgresFixture(t, database, CompletionModeUpdate)

	attempts := 0
	err := eventbus.SubscribeTo(f.bus, "billing",
		func(ctx context.Context, event orderPlaced) error {
			attempts++
			if attempts == 1 {
				return errors.New("temporary listener failure")
			}
			return nil
		})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	if err := f.publish(t.Context(), orderPlaced{OrderID: "o-2"}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	incomplete, err := f.store.FindIncomplete(t.Context())
	if err != nil {
		t.Fatalf("find incomplete: %v", err)
	}
	if len(incomplete) != 1 {
		t.Fatalf("incomplete after failed delivery = %d, want 1", len(incomplete))
	}

	if err := f.registry.ResubmitIncomplete(t.Context(), 0); err != nil {
		t.Fatalf("resubmit: %v", err)
	}

	incomplete, err = f.store.FindIncomplete(t.Context())
	if err != nil {
		t.Fatalf("find incomplete after resubmit: %v", err)
	}
	if len(incomplete) != 0 {
		t.Errorf("incomplete after resubmit = %d, want 0", len(incomplete))
	}
	if attempts != 2 {
		t.Errorf("delivery attempts = %d, want 2", attempts)
	}
}

// freshPostgresFixture drops the outbox tables and rebuilds the schema, so each
// flow starts from an empty database, then wires a PostgresStore-backed fixture.
func freshPostgresFixture(t *testing.T, database *sql.DB, mode CompletionMode) *fixture {
	t.Helper()
	_, err := database.ExecContext(t.Context(), "DROP TABLE IF EXISTS "+
		tableName+", "+archiveTableName+", event_publication_schema_history")
	if err != nil {
		t.Fatalf("reset schema: %v", err)
	}

	store := NewPostgresStore(database, mode)
	if err := store.Initialize(t.Context()); err != nil {
		t.Fatalf("initialize store: %v", err)
	}
	return assembleFixture(t, database, store)
}

func startPostgres(t *testing.T, image string) *sql.DB {
	t.Helper()
	ctx := context.Background()
	container, err := postgres.Run(ctx, image,
		postgres.WithDatabase("outbox"),
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
