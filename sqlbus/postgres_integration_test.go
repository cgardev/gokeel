//go:build integration

package sqlbus

import (
	"context"
	"database/sql"
	"fmt"
	"slices"
	"sync"
	"testing"
	"time"

	"github.com/cgardev/gokeel/eventbus"
	"github.com/cgardev/gokeel/transaction"

	_ "github.com/jackc/pgx/v5/stdlib"
	"github.com/testcontainers/testcontainers-go"
	"github.com/testcontainers/testcontainers-go/modules/postgres"
	"github.com/testcontainers/testcontainers-go/wait"
)

// postgresImages lists the two most recent PostgreSQL releases the store is
// verified against. Adjust the tags here if a registry lacks one of them.
var postgresImages = []string{"postgres:18-alpine", "postgres:17-alpine"}

// TestPostgresSQLBusIntegration runs the bus against a real PostgreSQL server,
// exercising the native SQL on the dialect that needs $N placeholders. It is
// gated behind the integration build tag because it needs a Docker daemon. Run
// it with: go test -tags=integration ./...
func TestPostgresSQLBusIntegration(t *testing.T) {
	for _, image := range postgresImages {
		t.Run(image, func(t *testing.T) {
			database := startPostgres(t, image)
			t.Run("text timestamps order chronologically",
				func(t *testing.T) { testTimestampOrdering(t, database) })
			t.Run("cross-node competing delivery",
				func(t *testing.T) { testCrossNodeCompetingDelivery(t, database) })
			t.Run("broadcast delivery per node",
				func(t *testing.T) { testBroadcastDeliveryPerNode(t, database) })
			t.Run("failure, exhaustion, and resubmission",
				func(t *testing.T) { testFailureAndResubmission(t, database) })
		})
	}
}

// testTimestampOrdering asserts, on the real server and its collation, the
// property the schema relies on: ORDER BY over the TEXT timestamp column
// returns chronological order.
func testTimestampOrdering(t *testing.T, database *sql.DB) {
	resetPostgresSchema(t, database)
	store := NewPostgresStore(database)
	if err := store.Initialize(t.Context()); err != nil {
		t.Fatalf("initialize store: %v", err)
	}

	base := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)
	times := []time.Time{
		base.Add(500 * time.Millisecond), // the RFC3339Nano-breaking shape
		base,
		base.Add(123456789 * time.Nanosecond),
		base.Add(-time.Second),
		base.Add(time.Minute),
	}
	for index, value := range times {
		_, err := database.ExecContext(t.Context(),
			"INSERT INTO "+messageTableName+
				" (id, event_type, serialized_event, publisher_node, publication_date)"+
				" VALUES ($1, $2, $3, $4, $5)",
			fmt.Sprintf("00000000-0000-0000-0000-00000000000%d", index),
			"order.placed", "{}", "test", formatTime(value))
		if err != nil {
			t.Fatalf("insert message %d: %v", index, err)
		}
	}

	rows, err := database.QueryContext(t.Context(),
		"SELECT publication_date FROM "+messageTableName+" ORDER BY publication_date ASC")
	if err != nil {
		t.Fatalf("select ordered publication dates: %v", err)
	}
	defer func() {
		if err := rows.Close(); err != nil {
			t.Logf("close rows: %v", err)
		}
	}()
	var ordered []time.Time
	for rows.Next() {
		var raw string
		if err := rows.Scan(&raw); err != nil {
			t.Fatalf("scan publication date: %v", err)
		}
		parsed, err := parseTime(raw)
		if err != nil {
			t.Fatalf("parse publication date %s: %v", raw, err)
		}
		ordered = append(ordered, parsed)
	}
	if err := rows.Err(); err != nil {
		t.Fatalf("iterate publication dates: %v", err)
	}

	chronological := slices.Clone(times)
	slices.SortFunc(chronological, func(a, b time.Time) int { return a.Compare(b) })
	for index, want := range chronological {
		if !ordered[index].Equal(want) {
			t.Fatalf("server order diverges from chronological order at position %d: %v != %v",
				index, ordered[index], want)
		}
	}
}

func testCrossNodeCompetingDelivery(t *testing.T, database *sql.DB) {
	publisherNode := freshPostgresNode(t, database, true)
	listenerNode := freshPostgresNode(t, database, false)

	var mu sync.Mutex
	handled := make(map[string]int)
	record := func(ctx context.Context, event orderPlaced) error {
		mu.Lock()
		defer mu.Unlock()
		handled[event.OrderID]++
		return nil
	}
	if err := AttachCompetingListener(t.Context(), listenerNode.bridge, "billing", record); err != nil {
		t.Fatalf("attach billing: %v", err)
	}
	startDispatcher(t, NewDispatcher(listenerNode.bridge, WithPollInterval(10*time.Millisecond)))

	const published = 10
	for index := range published {
		if err := publisherNode.publish(t.Context(), orderPlaced{OrderID: fmt.Sprintf("o-%d", index)}); err != nil {
			t.Fatalf("publish o-%d: %v", index, err)
		}
	}

	waitFor(t, 15*time.Second, "the remote node delivers every event", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(handled) == published
	})
	time.Sleep(100 * time.Millisecond)
	mu.Lock()
	defer mu.Unlock()
	for order, count := range handled {
		if count != 1 {
			t.Errorf("order %s was handled %d times, want exactly 1", order, count)
		}
	}
}

func testBroadcastDeliveryPerNode(t *testing.T, database *sql.DB) {
	first := freshPostgresNode(t, database, true)
	second := freshPostgresNode(t, database, false)

	var firstReceived, secondReceived recorder
	if err := AttachBroadcastListener(t.Context(), first.bridge, "cache", firstReceived.handle); err != nil {
		t.Fatalf("attach cache on the first node: %v", err)
	}
	if err := AttachBroadcastListener(t.Context(), second.bridge, "cache", secondReceived.handle); err != nil {
		t.Fatalf("attach cache on the second node: %v", err)
	}
	startDispatcher(t, NewDispatcher(first.bridge, WithPollInterval(10*time.Millisecond)))
	startDispatcher(t, NewDispatcher(second.bridge, WithPollInterval(10*time.Millisecond)))

	if err := first.publish(t.Context(), orderPlaced{OrderID: "o-1"}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	waitFor(t, 15*time.Second, "both nodes deliver the event", func() bool {
		return firstReceived.count() == 1 && secondReceived.count() == 1
	})
	time.Sleep(100 * time.Millisecond)
	if firstReceived.count() != 1 || secondReceived.count() != 1 {
		t.Errorf("deliveries first=%d second=%d, want exactly 1 on each node",
			firstReceived.count(), secondReceived.count())
	}
}

func testFailureAndResubmission(t *testing.T, database *sql.DB) {
	n := freshPostgresNode(t, database, true,
		WithMaximumAttempts(2),
		WithRetryDelay(func(attempt int) time.Duration { return 0 }))
	var mu sync.Mutex
	fixed := false
	err := AttachCompetingListener(t.Context(), n.bridge, "billing",
		func(ctx context.Context, event orderPlaced) error {
			mu.Lock()
			defer mu.Unlock()
			if !fixed {
				return fmt.Errorf("listener is broken")
			}
			return nil
		})
	if err != nil {
		t.Fatalf("attach billing: %v", err)
	}
	startDispatcher(t, NewDispatcher(n.bridge, WithPollInterval(10*time.Millisecond)))

	if err := n.publish(t.Context(), orderPlaced{OrderID: "o-1"}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	waitFor(t, 15*time.Second, "the delivery exhausts its attempt budget", func() bool {
		status, _, _ := readSingleDeliveryState(t, database)
		return status == StatusExhausted
	})

	mu.Lock()
	fixed = true
	mu.Unlock()
	key := DeliveryKey{
		MessageID:  readSingleMessageID(t, database),
		ListenerID: "billing",
		Instance:   "",
	}
	revived, err := n.bridge.Resubmit(t.Context(), key)
	if err != nil || !revived {
		t.Fatalf("resubmit = %v, %v; want a successful revival", revived, err)
	}
	waitFor(t, 15*time.Second, "the revived delivery completes", func() bool {
		status, _, _ := readSingleDeliveryState(t, database)
		return status == StatusCompleted
	})
}

func resetPostgresSchema(t *testing.T, database *sql.DB) {
	t.Helper()
	// The native record table must fall with the data tables: if it survives,
	// the migrator considers every script applied and skips rebuilding the
	// schema on the next Initialize.
	_, err := database.ExecContext(t.Context(), "DROP TABLE IF EXISTS "+
		messageTableName+", "+listenerTableName+", "+consumerTableName+", "+
		deliveryTableName+", "+SchemaHistoryTable+", "+nativeAppliedTable)
	if err != nil {
		t.Fatalf("reset schema: %v", err)
	}
}

// freshPostgresNode wires one node over the shared PostgreSQL server. The
// first node of a test resets and rebuilds the schema, so each flow starts
// from an empty database.
func freshPostgresNode(t *testing.T, database *sql.DB, reset bool, options ...BridgeOption) *node {
	t.Helper()
	if reset {
		resetPostgresSchema(t, database)
	}
	store := NewPostgresStore(database)
	if err := store.Initialize(t.Context()); err != nil {
		t.Fatalf("initialize store: %v", err)
	}

	serializer := NewJSONSerializer()
	if err := RegisterEventType[orderPlaced](serializer, "order.placed"); err != nil {
		t.Fatalf("register event type: %v", err)
	}

	bus := eventbus.NewBus()
	bridge := NewBridge(store, bus, serializer, options...)
	manager := transaction.NewManager(database)
	return &node{
		database:  database,
		store:     store,
		bus:       bus,
		bridge:    bridge,
		manager:   manager,
		publisher: NewPublisher(bridge, manager),
	}
}

func startPostgres(t *testing.T, image string) *sql.DB {
	t.Helper()
	ctx := context.Background()
	container, err := postgres.Run(ctx, image,
		postgres.WithDatabase("sqlbus"),
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
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Logf("close database: %v", err)
		}
	})
	if err := database.PingContext(ctx); err != nil {
		t.Fatalf("ping postgres: %v", err)
	}
	return database
}
