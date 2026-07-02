package outbox

import (
	"context"
	"database/sql"
	"errors"
	"path/filepath"
	"testing"
	"time"

	"github.com/cgardev/gokeel/eventbus"
	"github.com/cgardev/gokeel/transaction"

	_ "modernc.org/sqlite"
)

type orderPlaced struct {
	OrderID string
}

type fixture struct {
	database  *sql.DB
	store     Store
	bus       *eventbus.Bus
	registry  *Registry
	manager   *transaction.Manager
	publisher *Publisher
}

// publish runs the publication of an event inside a unit of work, the path the
// stores take in production: the rows join the transaction and are delivered
// after it commits.
func (f *fixture) publish(ctx context.Context, event any) error {
	return f.manager.Run(ctx, func(ctx context.Context) error {
		return f.publisher.Publish(ctx, event)
	})
}

func newFixture(t *testing.T, completionMode CompletionMode) *fixture {
	t.Helper()
	path := filepath.Join(t.TempDir(), "outbox.db")
	dataSourceName := "file:" + path +
		"?_txlock=immediate&_pragma=journal_mode(WAL)&_pragma=busy_timeout(5000)"
	database, err := sql.Open("sqlite", dataSourceName)
	if err != nil {
		t.Fatalf("open database: %v", err)
	}
	t.Cleanup(func() { database.Close() })

	store := NewSQLiteStore(database, completionMode)
	if err := store.Initialize(t.Context()); err != nil {
		t.Fatalf("initialize store: %v", err)
	}
	return assembleFixture(t, database, store)
}

// assembleFixture wires the registry, bus, manager, and publisher around an
// already-initialized store, so the SQLite and PostgreSQL fixtures share it.
func assembleFixture(t *testing.T, database *sql.DB, store Store) *fixture {
	t.Helper()
	serializer := NewJSONSerializer()
	if err := RegisterEventType[orderPlaced](serializer, "order.placed"); err != nil {
		t.Fatalf("register event type: %v", err)
	}

	bus := eventbus.NewBus()
	registry := NewRegistry(store, bus, serializer)
	manager := transaction.NewManager(database)
	return &fixture{
		database:  database,
		store:     store,
		bus:       bus,
		registry:  registry,
		manager:   manager,
		publisher: NewPublisher(registry, manager),
	}
}

func subscribeRecorder(
	t *testing.T,
	bus *eventbus.Bus,
	id eventbus.ListenerID,
	received *[]orderPlaced,
) {
	t.Helper()
	err := eventbus.SubscribeTo(bus, id, func(ctx context.Context, event orderPlaced) error {
		*received = append(*received, event)
		return nil
	})
	if err != nil {
		t.Fatalf("subscribe %s: %v", id, err)
	}
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

func TestPublishStoresOnePublicationPerListenerInTheCallersTransaction(t *testing.T) {
	f := newFixture(t, CompletionModeUpdate)
	var first, second []orderPlaced
	subscribeRecorder(t, f.bus, "billing", &first)
	subscribeRecorder(t, f.bus, "shipping", &second)

	transaction, err := f.database.BeginTx(t.Context(), nil)
	if err != nil {
		t.Fatalf("begin transaction: %v", err)
	}
	publications, err := f.registry.Publish(t.Context(), transaction, orderPlaced{OrderID: "o-1"})
	if err != nil {
		_ = transaction.Rollback()
		t.Fatalf("publish: %v", err)
	}
	if len(publications) != 2 {
		_ = transaction.Rollback()
		t.Fatalf("publications created = %d, want 2", len(publications))
	}
	// The transaction is rolled back without committing, so the publications
	// must vanish with the business change they would have joined.
	if err := transaction.Rollback(); err != nil {
		t.Fatalf("rollback transaction: %v", err)
	}

	if got := countRows(t, f.database, tableName); got != 0 {
		t.Errorf("rows after rollback = %d, want 0", got)
	}
	if len(first)+len(second) != 0 {
		t.Error("listeners were invoked before commit")
	}
}

func TestPublisherDeliversToEveryListenerAfterCommit(t *testing.T) {
	f := newFixture(t, CompletionModeUpdate)
	var first, second []orderPlaced
	subscribeRecorder(t, f.bus, "billing", &first)
	subscribeRecorder(t, f.bus, "shipping", &second)

	err := f.publish(t.Context(), orderPlaced{OrderID: "o-1"})
	if err != nil {
		t.Fatalf("within transaction: %v", err)
	}

	if len(first) != 1 || len(second) != 1 {
		t.Fatalf("deliveries = %d and %d, want 1 and 1", len(first), len(second))
	}

	incomplete, err := f.store.FindIncomplete(t.Context())
	if err != nil {
		t.Fatalf("find incomplete: %v", err)
	}
	if len(incomplete) != 0 {
		t.Errorf("incomplete publications after delivery = %d, want 0", len(incomplete))
	}
	if got := countRows(t, f.database, tableName); got != 2 {
		t.Errorf("rows in update mode = %d, want 2", got)
	}
}

func TestAsynchronousDispatchDoesNotBlockTheCaller(t *testing.T) {
	f := newFixture(t, CompletionModeUpdate)
	publisher := NewPublisher(f.registry, f.manager).WithAsynchronousDispatch()

	release := make(chan struct{})
	delivered := make(chan orderPlaced, 1)
	err := eventbus.SubscribeTo(f.bus, "slow-listener",
		func(ctx context.Context, event orderPlaced) error {
			<-release
			delivered <- event
			return nil
		})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	// Run must return while the listener is still blocked on the release
	// channel; a synchronous dispatch would deadlock here.
	err = f.manager.Run(t.Context(), func(ctx context.Context) error {
		return publisher.Publish(ctx, orderPlaced{OrderID: "o-async"})
	})
	if err != nil {
		t.Fatalf("within transaction: %v", err)
	}

	close(release)
	select {
	case event := <-delivered:
		if event.OrderID != "o-async" {
			t.Errorf("delivered order = %q, want %q", event.OrderID, "o-async")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("event was not delivered after releasing the listener")
	}

	// The background goroutine settles the publication after delivery.
	deadline := time.Now().Add(5 * time.Second)
	for {
		incomplete, err := f.store.FindIncomplete(context.Background())
		if err != nil {
			t.Fatalf("find incomplete: %v", err)
		}
		if len(incomplete) == 0 {
			break
		}
		if time.Now().After(deadline) {
			t.Fatalf("publication still incomplete after delivery: %d remaining", len(incomplete))
		}
		time.Sleep(10 * time.Millisecond)
	}
}

func TestResubmitterRecoversAFailedDelivery(t *testing.T) {
	f := newFixture(t, CompletionModeUpdate)

	// The listener fails on its first invocation and succeeds afterwards,
	// imitating a collaborator that comes back after an outage.
	attempts := 0
	delivered := make(chan orderPlaced, 1)
	err := eventbus.SubscribeTo(f.bus, "flaky-listener",
		func(ctx context.Context, event orderPlaced) error {
			attempts++
			if attempts == 1 {
				return errors.New("collaborator unavailable")
			}
			delivered <- event
			return nil
		})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	err = f.publish(t.Context(), orderPlaced{OrderID: "o-retried"})
	if err != nil {
		t.Fatalf("within transaction: %v", err)
	}

	stop := NewResubmitter(f.registry, 10*time.Millisecond, 0).Start()
	defer stop()

	select {
	case event := <-delivered:
		if event.OrderID != "o-retried" {
			t.Errorf("redelivered order = %q, want %q", event.OrderID, "o-retried")
		}
	case <-time.After(5 * time.Second):
		t.Fatal("failed delivery was not recovered by the resubmitter")
	}
}

func TestRollbackDiscardsPublicationsWithTheBusinessChange(t *testing.T) {
	f := newFixture(t, CompletionModeUpdate)
	var received []orderPlaced
	subscribeRecorder(t, f.bus, "billing", &received)

	err := f.manager.Run(t.Context(), func(ctx context.Context) error {
		if err := f.publisher.Publish(ctx, orderPlaced{OrderID: "o-1"}); err != nil {
			return err
		}
		return errors.New("business failure")
	})
	if err == nil {
		t.Fatal("failing work reported no error")
	}

	if len(received) != 0 {
		t.Error("listener was invoked despite the rollback")
	}
	if got := countRows(t, f.database, tableName); got != 0 {
		t.Errorf("rows after rollback = %d, want 0", got)
	}
}

func TestFailedDeliveryStaysIncompleteAndIsResubmitted(t *testing.T) {
	f := newFixture(t, CompletionModeUpdate)
	deliveries := 0
	err := eventbus.SubscribeTo(f.bus, "billing", func(ctx context.Context, event orderPlaced) error {
		deliveries++
		if deliveries == 1 {
			return errors.New("temporary listener failure")
		}
		return nil
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	err = f.publish(t.Context(), orderPlaced{OrderID: "o-1"})
	if err != nil {
		t.Fatalf("within transaction: %v", err)
	}

	incomplete, err := f.store.FindIncomplete(t.Context())
	if err != nil {
		t.Fatalf("find incomplete: %v", err)
	}
	if len(incomplete) != 1 {
		t.Fatalf("incomplete publications after failure = %d, want 1", len(incomplete))
	}
	if incomplete[0].Status != StatusFailed {
		t.Errorf("status after failure = %s, want %s", incomplete[0].Status, StatusFailed)
	}
	if got := incomplete[0].CompletionAttempts; got != 1 {
		t.Errorf("completion attempts after the initial dispatch = %d, want 1", got)
	}
	if incomplete[0].LastResubmissionDate == nil {
		t.Error("last resubmission date not seeded at creation")
	}

	if err := f.registry.ResubmitIncomplete(t.Context(), 0); err != nil {
		t.Fatalf("resubmit incomplete: %v", err)
	}
	if deliveries != 2 {
		t.Errorf("total deliveries = %d, want 2", deliveries)
	}

	incomplete, err = f.store.FindIncomplete(t.Context())
	if err != nil {
		t.Fatalf("find incomplete: %v", err)
	}
	if len(incomplete) != 0 {
		t.Errorf("incomplete publications after resubmission = %d, want 0", len(incomplete))
	}
}

func TestResubmissionCountsAttemptsAndRestoresTheEventFromItsSerializedForm(t *testing.T) {
	f := newFixture(t, CompletionModeUpdate)
	var received []orderPlaced
	failing := true
	err := eventbus.SubscribeTo(f.bus, "billing", func(ctx context.Context, event orderPlaced) error {
		if failing {
			return errors.New("listener failure")
		}
		received = append(received, event)
		return nil
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	err = f.publish(t.Context(), orderPlaced{OrderID: "o-42"})
	if err != nil {
		t.Fatalf("within transaction: %v", err)
	}

	if err := f.registry.ResubmitIncomplete(t.Context(), 0); err == nil {
		t.Fatal("resubmission of a still failing listener reported no error")
	}

	incomplete, err := f.store.FindIncomplete(t.Context())
	if err != nil {
		t.Fatalf("find incomplete: %v", err)
	}
	if len(incomplete) != 1 {
		t.Fatalf("incomplete publications = %d, want 1", len(incomplete))
	}
	// The initial dispatch counts as attempt one and the resubmission as
	// attempt two, mirroring the Spring Modulith attempt accounting.
	if got := incomplete[0].CompletionAttempts; got != 2 {
		t.Errorf("completion attempts = %d, want 2", got)
	}
	if incomplete[0].LastResubmissionDate == nil {
		t.Error("last resubmission date not recorded")
	}

	failing = false
	if err := f.registry.ResubmitIncomplete(t.Context(), 0); err != nil {
		t.Fatalf("second resubmission: %v", err)
	}
	if len(received) != 1 || received[0].OrderID != "o-42" {
		t.Fatalf("event restored from serialized form = %+v, want order o-42", received)
	}
}

func TestCompletionModeDeleteRemovesSettledPublications(t *testing.T) {
	f := newFixture(t, CompletionModeDelete)
	var received []orderPlaced
	subscribeRecorder(t, f.bus, "billing", &received)

	err := f.publish(t.Context(), orderPlaced{OrderID: "o-1"})
	if err != nil {
		t.Fatalf("within transaction: %v", err)
	}

	if got := countRows(t, f.database, tableName); got != 0 {
		t.Errorf("rows in delete mode = %d, want 0", got)
	}
}

func TestCompletionModeArchiveMovesSettledPublications(t *testing.T) {
	f := newFixture(t, CompletionModeArchive)
	var received []orderPlaced
	subscribeRecorder(t, f.bus, "billing", &received)

	err := f.publish(t.Context(), orderPlaced{OrderID: "o-1"})
	if err != nil {
		t.Fatalf("within transaction: %v", err)
	}

	if got := countRows(t, f.database, tableName); got != 0 {
		t.Errorf("rows left in main table = %d, want 0", got)
	}
	if got := countRows(t, f.database, archiveTableName); got != 1 {
		t.Errorf("rows in archive table = %d, want 1", got)
	}
}

func TestPublishStoresNothingForAnEventWithoutListeners(t *testing.T) {
	f := newFixture(t, CompletionModeUpdate)

	err := f.publish(t.Context(), orderPlaced{OrderID: "o-1"})
	if err != nil {
		t.Fatalf("within transaction: %v", err)
	}

	if got := countRows(t, f.database, tableName); got != 0 {
		t.Errorf("rows without listeners = %d, want 0", got)
	}
}

func TestSerializeRejectsAnUnregisteredEventType(t *testing.T) {
	serializer := NewJSONSerializer()

	type unregistered struct{}
	if _, _, err := serializer.Serialize(unregistered{}); !errors.Is(err, ErrUnknownEventType) {
		t.Fatalf("serialize unregistered type error = %v, want ErrUnknownEventType", err)
	}
	if _, err := serializer.Deserialize("missing.type", "{}"); !errors.Is(err, ErrUnknownEventType) {
		t.Fatalf("deserialize unknown name error = %v, want ErrUnknownEventType", err)
	}
}
