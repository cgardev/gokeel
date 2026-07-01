package sqlbus

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/cgardev/gokeel/eventbus"
	"github.com/cgardev/gokeel/transaction"

	"github.com/google/uuid"
	_ "modernc.org/sqlite"
)

type orderPlaced struct {
	OrderID string
}

type orderCancelled struct {
	OrderID string
}

// node is one simulated application node: its own bus, bridge, transaction
// manager, and publisher, sharing the database with every other node of the
// test cluster.
type node struct {
	database  *sql.DB
	store     Store
	bus       *eventbus.Bus
	bridge    *Bridge
	manager   *transaction.Manager
	publisher *Publisher
}

// publish runs the publication of an event inside a unit of work, the path
// the stores take in production: the rows join the transaction and the local
// deliveries run after it commits.
func (n *node) publish(ctx context.Context, event any) error {
	return n.manager.Run(ctx, func(ctx context.Context) error {
		return n.publisher.Publish(ctx, event)
	})
}

func openSQLiteDatabase(t *testing.T, path string) *sql.DB {
	t.Helper()
	dataSourceName := "file:" + path +
		"?_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	database, err := sql.Open("sqlite", dataSourceName)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() {
		if err := database.Close(); err != nil {
			t.Logf("close database: %v", err)
		}
	})
	return database
}

// newSQLiteNode wires one node over the shared SQLite file. Initialize is
// idempotent, so every node of a test cluster may run it.
func newSQLiteNode(t *testing.T, path string, options ...BridgeOption) *node {
	t.Helper()
	database := openSQLiteDatabase(t, path)
	store := NewSQLiteStore(database)
	if err := store.Initialize(t.Context()); err != nil {
		t.Fatalf("initialize store: %v", err)
	}

	serializer := NewJSONSerializer()
	if err := RegisterEventType[orderPlaced](serializer, "order.placed"); err != nil {
		t.Fatalf("register event type: %v", err)
	}
	if err := RegisterEventType[orderCancelled](serializer, "order.cancelled"); err != nil {
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

func newSQLitePath(t *testing.T) string {
	t.Helper()
	return filepath.Join(t.TempDir(), "sqlbus.db")
}

// recorder collects the events a listener received, safely across the
// dispatcher goroutines of several nodes.
type recorder struct {
	mu     sync.Mutex
	events []orderPlaced
}

func (r *recorder) handle(ctx context.Context, event orderPlaced) error {
	r.mu.Lock()
	defer r.mu.Unlock()
	r.events = append(r.events, event)
	return nil
}

func (r *recorder) count() int {
	r.mu.Lock()
	defer r.mu.Unlock()
	return len(r.events)
}

func (r *recorder) snapshot() []orderPlaced {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]orderPlaced(nil), r.events...)
}

func waitFor(t *testing.T, timeout time.Duration, message string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for time.Now().Before(deadline) {
		if condition() {
			return
		}
		time.Sleep(5 * time.Millisecond)
	}
	t.Fatalf("condition not met within %v: %s", timeout, message)
}

func countRows(t *testing.T, database *sql.DB, table string) int64 {
	t.Helper()
	var count int64
	row := database.QueryRowContext(t.Context(), "SELECT COUNT(*) FROM "+table)
	if err := row.Scan(&count); err != nil {
		t.Fatalf("count rows of %s: %v", table, err)
	}
	return count
}

func countDeliveriesWithStatus(t *testing.T, database *sql.DB, status Status) int64 {
	t.Helper()
	var count int64
	row := database.QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM "+deliveryTableName+" WHERE status = ?", string(status))
	if err := row.Scan(&count); err != nil {
		t.Fatalf("count deliveries with status %s: %v", status, err)
	}
	return count
}

// readSingleDeliveryState returns the status, attempts, and last error of the
// only delivery row in the database.
func readSingleDeliveryState(t *testing.T, database *sql.DB) (Status, int, string) {
	t.Helper()
	row := database.QueryRowContext(t.Context(),
		"SELECT status, attempts, last_error FROM "+deliveryTableName)
	var status string
	var attempts int64
	var lastError string
	if err := row.Scan(&status, &attempts, &lastError); err != nil {
		t.Fatalf("read delivery state: %v", err)
	}
	return Status(status), int(attempts), lastError
}

// readSingleMessageID returns the identifier of the only message row in the
// database.
func readSingleMessageID(t *testing.T, database *sql.DB) uuid.UUID {
	t.Helper()
	row := database.QueryRowContext(t.Context(), "SELECT id FROM "+messageTableName)
	var raw string
	if err := row.Scan(&raw); err != nil {
		t.Fatalf("read message identifier: %v", err)
	}
	id, err := uuid.Parse(raw)
	if err != nil {
		t.Fatalf("parse message identifier: %v", err)
	}
	return id
}

func startDispatcher(t *testing.T, dispatcher *Dispatcher) {
	t.Helper()
	stop := dispatcher.Start()
	t.Cleanup(stop)
}

func TestPublisherDeliversToLocalListenersAfterCommit(t *testing.T) {
	n := newSQLiteNode(t, newSQLitePath(t))
	var competing, broadcast recorder
	if err := AttachCompetingListener(t.Context(), n.bridge, "billing", competing.handle); err != nil {
		t.Fatalf("attach billing: %v", err)
	}
	if err := AttachBroadcastListener(t.Context(), n.bridge, "cache", broadcast.handle); err != nil {
		t.Fatalf("attach cache: %v", err)
	}

	if err := n.publish(t.Context(), orderPlaced{OrderID: "o-1"}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	if competing.count() != 1 || broadcast.count() != 1 {
		t.Fatalf("delivered competing=%d broadcast=%d, want 1 and 1", competing.count(), broadcast.count())
	}
	if got := competing.snapshot()[0].OrderID; got != "o-1" {
		t.Errorf("competing listener received order %s, want o-1", got)
	}
	if got := countRows(t, n.database, messageTableName); got != 1 {
		t.Errorf("message rows = %d, want 1", got)
	}
	var completed int64
	row := n.database.QueryRowContext(t.Context(),
		"SELECT COUNT(*) FROM "+deliveryTableName+" WHERE status = ?", string(StatusCompleted))
	if err := row.Scan(&completed); err != nil {
		t.Fatalf("count completed deliveries: %v", err)
	}
	if completed != 2 {
		t.Errorf("completed deliveries = %d, want 2", completed)
	}
}

func TestRollbackDiscardsTheMessageWithTheBusinessChange(t *testing.T) {
	n := newSQLiteNode(t, newSQLitePath(t))
	var received recorder
	if err := AttachCompetingListener(t.Context(), n.bridge, "billing", received.handle); err != nil {
		t.Fatalf("attach billing: %v", err)
	}

	tx, err := n.database.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	if _, _, err := n.bridge.Publish(t.Context(), tx, orderPlaced{OrderID: "o-1"}); err != nil {
		_ = tx.Rollback()
		t.Fatalf("publish: %v", err)
	}
	if err := tx.Rollback(); err != nil {
		t.Fatalf("rollback: %v", err)
	}

	if got := countRows(t, n.database, messageTableName); got != 0 {
		t.Errorf("message rows after rollback = %d, want 0", got)
	}
	if got := countRows(t, n.database, deliveryTableName); got != 0 {
		t.Errorf("delivery rows after rollback = %d, want 0", got)
	}
	if received.count() != 0 {
		t.Errorf("listener received %d events after rollback, want 0", received.count())
	}
}

func TestPublishStoresTheMessageEvenWithoutLocalListeners(t *testing.T) {
	n := newSQLiteNode(t, newSQLitePath(t))

	if err := n.publish(t.Context(), orderPlaced{OrderID: "o-1"}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	if got := countRows(t, n.database, messageTableName); got != 1 {
		t.Errorf("message rows = %d, want 1", got)
	}
	if got := countRows(t, n.database, deliveryTableName); got != 0 {
		t.Errorf("delivery rows = %d, want 0", got)
	}
}

func TestPublishRejectsAnUnregisteredEventType(t *testing.T) {
	n := newSQLiteNode(t, newSQLitePath(t))

	type unregistered struct{ Value string }
	err := n.publish(t.Context(), unregistered{Value: "v"})
	if !errors.Is(err, ErrUnknownEventType) {
		t.Fatalf("publish error = %v, want ErrUnknownEventType", err)
	}
}

func TestAttachRejectsAnUnregisteredEventType(t *testing.T) {
	n := newSQLiteNode(t, newSQLitePath(t))

	type unregistered struct{ Value string }
	err := AttachCompetingListener(t.Context(), n.bridge, "billing",
		func(ctx context.Context, event unregistered) error { return nil })
	if !errors.Is(err, ErrUnknownEventType) {
		t.Fatalf("attach error = %v, want ErrUnknownEventType", err)
	}
	if got := countRows(t, n.database, consumerTableName); got != 0 {
		t.Errorf("consumer rows after rejected attach = %d, want 0", got)
	}
}

func TestAttachRejectsAConflictingDeliveryMode(t *testing.T) {
	path := newSQLitePath(t)
	first := newSQLiteNode(t, path)
	second := newSQLiteNode(t, path)
	var received recorder

	if err := AttachCompetingListener(t.Context(), first.bridge, "billing", received.handle); err != nil {
		t.Fatalf("attach competing: %v", err)
	}
	err := AttachBroadcastListener(t.Context(), second.bridge, "billing", received.handle)
	if !errors.Is(err, ErrConflictingDeliveryMode) {
		t.Fatalf("conflicting attach error = %v, want ErrConflictingDeliveryMode", err)
	}
}

func TestFailedDeliveryIsRetriedUntilItSucceeds(t *testing.T) {
	n := newSQLiteNode(t, newSQLitePath(t),
		WithRetryDelay(func(attempt int) time.Duration { return 0 }))
	var attempts sync.Map
	handled := make(chan struct{})
	err := AttachCompetingListener(t.Context(), n.bridge, "billing",
		func(ctx context.Context, event orderPlaced) error {
			count, _ := attempts.LoadOrStore(event.OrderID, new(int))
			counter := count.(*int)
			*counter++
			if *counter < 3 {
				return errors.New("temporary listener failure")
			}
			close(handled)
			return nil
		})
	if err != nil {
		t.Fatalf("attach billing: %v", err)
	}
	startDispatcher(t, NewDispatcher(n.bridge, WithPollInterval(10*time.Millisecond)))

	if err := n.publish(t.Context(), orderPlaced{OrderID: "o-1"}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	select {
	case <-handled:
	case <-time.After(5 * time.Second):
		t.Fatal("delivery was not retried to success within 5s")
	}
	waitFor(t, 5*time.Second, "delivery settles as completed", func() bool {
		status, _, _ := readSingleDeliveryState(t, n.database)
		return status == StatusCompleted
	})
	_, recorded, _ := readSingleDeliveryState(t, n.database)
	if recorded != 3 {
		t.Errorf("recorded attempts = %d, want 3", recorded)
	}
}

func TestExhaustedDeliveryIsRevivedByResubmit(t *testing.T) {
	n := newSQLiteNode(t, newSQLitePath(t),
		WithMaximumAttempts(2),
		WithRetryDelay(func(attempt int) time.Duration { return 0 }))
	var healthy sync.Mutex
	fixed := false
	err := AttachCompetingListener(t.Context(), n.bridge, "billing",
		func(ctx context.Context, event orderPlaced) error {
			healthy.Lock()
			defer healthy.Unlock()
			if !fixed {
				return errors.New("listener is broken")
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

	waitFor(t, 5*time.Second, "delivery exhausts its attempt budget", func() bool {
		status, _, _ := readSingleDeliveryState(t, n.database)
		return status == StatusExhausted
	})
	status, attempts, lastError := readSingleDeliveryState(t, n.database)
	if status != StatusExhausted || attempts != 2 {
		t.Fatalf("delivery state = %s with %d attempts, want EXHAUSTED with 2", status, attempts)
	}
	if lastError == "" {
		t.Error("exhausted delivery carries no failure cause")
	}

	healthy.Lock()
	fixed = true
	healthy.Unlock()

	key := DeliveryKey{
		MessageID:  readSingleMessageID(t, n.database),
		ListenerID: "billing",
		Instance:   "",
	}
	revived, err := n.bridge.Resubmit(t.Context(), key)
	if err != nil {
		t.Fatalf("resubmit: %v", err)
	}
	if !revived {
		t.Fatal("resubmit reported no revival for an exhausted delivery")
	}
	waitFor(t, 5*time.Second, "revived delivery completes", func() bool {
		status, _, _ := readSingleDeliveryState(t, n.database)
		return status == StatusCompleted
	})
}

func TestRegisterEventTypeRejectsConflictingRegistrations(t *testing.T) {
	serializer := NewJSONSerializer()
	if err := RegisterEventType[orderPlaced](serializer, "order.placed"); err != nil {
		t.Fatalf("first registration: %v", err)
	}
	if err := RegisterEventType[orderPlaced](serializer, "order.placed"); err != nil {
		t.Fatalf("repeated identical registration: %v", err)
	}
	if err := RegisterEventType[orderPlaced](serializer, "order.renamed"); !errors.Is(err, ErrConflictingRegistration) {
		t.Fatalf("re-binding a type error = %v, want ErrConflictingRegistration", err)
	}
	type otherEvent struct{ Value string }
	if err := RegisterEventType[otherEvent](serializer, "order.placed"); !errors.Is(err, ErrConflictingRegistration) {
		t.Fatalf("re-binding a name error = %v, want ErrConflictingRegistration", err)
	}
}
