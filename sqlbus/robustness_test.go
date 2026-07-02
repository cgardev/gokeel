package sqlbus

import (
	"context"
	"fmt"
	"math/rand/v2"
	"slices"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/google/uuid"
)

// TestFormattedTimeOrderMatchesChronologicalOrder is the property the schema
// relies on: frontiers, due-delivery ordering, and retention all compare the
// persisted TEXT timestamps in SQL, so their lexicographic order must equal
// their chronological order. time.RFC3339Nano trims trailing fractional
// zeros and breaks the property; the fixed-width layout must not.
func TestFormattedTimeOrderMatchesChronologicalOrder(t *testing.T) {
	base := time.Date(2026, time.July, 1, 12, 0, 0, 0, time.UTC)
	times := []time.Time{
		base,
		base.Add(500 * time.Millisecond), // the RFC3339Nano-breaking shape: ".5Z" sorts before "Z"
		base.Add(123456789 * time.Nanosecond),
		base.Add(time.Second),
		base.Add(-time.Nanosecond),
	}
	for range 500 {
		offset := time.Duration(rand.Int64N(int64(48 * time.Hour)))
		times = append(times, base.Add(offset-24*time.Hour))
	}

	chronological := slices.Clone(times)
	slices.SortFunc(chronological, func(a, b time.Time) int { return a.Compare(b) })

	formatted := make([]string, len(times))
	for index, value := range times {
		formatted[index] = formatTime(value)
	}
	slices.Sort(formatted)

	for index, value := range chronological {
		if formatted[index] != formatTime(value) {
			t.Fatalf("lexicographic order diverges from chronological order at position %d: %s != %s",
				index, formatted[index], formatTime(value))
		}
	}
}

func TestFormattedTimeRoundTrips(t *testing.T) {
	value := time.Date(2026, time.July, 1, 12, 30, 45, 500000000, time.UTC)
	parsed, err := parseTime(formatTime(value))
	if err != nil {
		t.Fatalf("parse formatted time: %v", err)
	}
	if !parsed.Equal(value) {
		t.Fatalf("round trip = %v, want %v", parsed, value)
	}
	if !strings.HasSuffix(formatTime(value), ".500000000Z") {
		t.Fatalf("formatted time %s does not keep its trailing fractional zeros", formatTime(value))
	}
}

// seedDelivery stores one message and one pending competing delivery for it,
// bypassing the bridge, so store-level tests control the rows directly.
func seedDelivery(t *testing.T, n *node) DeliveryKey {
	t.Helper()
	message := Message{
		ID:              uuid.New(),
		EventType:       "order.placed",
		SerializedEvent: `{"OrderID":"o-1"}`,
		PublisherNode:   "seed",
		PublicationDate: time.Now().UTC(),
	}
	if err := n.store.CreateMessage(t.Context(), n.database, message); err != nil {
		t.Fatalf("create message: %v", err)
	}
	key := DeliveryKey{MessageID: message.ID, ListenerID: "billing", Instance: ""}
	if err := n.store.CreateDeliveries(t.Context(), n.database, []DeliveryKey{key}); err != nil {
		t.Fatalf("create delivery: %v", err)
	}
	return key
}

func TestClaimArbitrationLetsExactlyOneClaimantWin(t *testing.T) {
	n := newSQLiteNode(t, newSQLitePath(t))
	key := seedDelivery(t, n)

	const claimants = 8
	now := time.Now().UTC()
	var wins int32
	var mu sync.Mutex
	var group sync.WaitGroup
	for index := range claimants {
		group.Add(1)
		go func() {
			defer group.Done()
			claimed, err := n.store.ClaimDelivery(t.Context(), key,
				fmt.Sprintf("token-%d", index), now, now.Add(-time.Hour), 0)
			if err != nil {
				t.Errorf("claim: %v", err)
				return
			}
			if claimed {
				mu.Lock()
				wins++
				mu.Unlock()
			}
		}()
	}
	group.Wait()

	if wins != 1 {
		t.Fatalf("concurrent claim winners = %d, want exactly 1", wins)
	}
}

func TestSettlementIsFencedAgainstAStolenClaim(t *testing.T) {
	n := newSQLiteNode(t, newSQLitePath(t))
	key := seedDelivery(t, n)
	now := time.Now().UTC()

	claimed, err := n.store.ClaimDelivery(t.Context(), key, "zombie", now, now.Add(-time.Hour), 0)
	if err != nil || !claimed {
		t.Fatalf("first claim = %v, %v; want a successful claim", claimed, err)
	}

	// A lease cutoff in the future treats the first claim as expired, which
	// is how another node steals a delivery whose dispatcher stalled. The
	// thief observed the zombie's attempt count when it found the delivery.
	stolen, err := n.store.ClaimDelivery(t.Context(), key, "thief", now, now.Add(time.Second), 1)
	if err != nil || !stolen {
		t.Fatalf("stealing claim = %v, %v; want a successful steal", stolen, err)
	}

	settled, err := n.store.CompleteDelivery(t.Context(), key, "zombie", now)
	if err != nil {
		t.Fatalf("zombie completion: %v", err)
	}
	if settled {
		t.Fatal("a zombie settlement with a stolen token affected rows")
	}
	failed, err := n.store.FailDelivery(t.Context(), key, "zombie", "late failure", now, 1, 5)
	if err != nil {
		t.Fatalf("zombie failure: %v", err)
	}
	if failed {
		t.Fatal("a zombie failure with a stolen token affected rows")
	}

	settled, err = n.store.CompleteDelivery(t.Context(), key, "thief", now)
	if err != nil || !settled {
		t.Fatalf("legitimate completion = %v, %v; want a successful settlement", settled, err)
	}
	status, _, _ := readSingleDeliveryState(t, n.database)
	if status != StatusCompleted {
		t.Fatalf("delivery status = %s, want COMPLETED", status)
	}
}

func TestPanickingListenerLeavesTheDeliveryRecoverable(t *testing.T) {
	n := newSQLiteNode(t, newSQLitePath(t),
		WithRetryDelay(func(attempt int) time.Duration { return 0 }))
	var mu sync.Mutex
	calls := 0
	err := AttachCompetingListener(t.Context(), n.bridge, "billing",
		func(ctx context.Context, event orderPlaced) error {
			mu.Lock()
			defer mu.Unlock()
			calls++
			if calls == 1 {
				panic("listener exploded")
			}
			return nil
		})
	if err != nil {
		t.Fatalf("attach billing: %v", err)
	}
	startDispatcher(t, NewDispatcher(n.bridge, WithPollInterval(10*time.Millisecond)))

	if err := n.publish(t.Context(), orderPlaced{OrderID: "o-1"}); err != nil {
		t.Fatalf("publish must survive a panicking listener, got %v", err)
	}

	waitFor(t, 5*time.Second, "the panicked delivery is retried to completion", func() bool {
		status, _, _ := readSingleDeliveryState(t, n.database)
		return status == StatusCompleted
	})
	mu.Lock()
	defer mu.Unlock()
	if calls != 2 {
		t.Errorf("listener calls = %d, want 2", calls)
	}
}

func TestRetentionRemovesOnlySettledMessages(t *testing.T) {
	n := newSQLiteNode(t, newSQLitePath(t),
		WithRetryDelay(func(attempt int) time.Duration { return time.Hour }))
	err := AttachCompetingListener(t.Context(), n.bridge, "billing",
		func(ctx context.Context, event orderPlaced) error {
			if event.OrderID == "poison" {
				return fmt.Errorf("listener rejects %s", event.OrderID)
			}
			return nil
		})
	if err != nil {
		t.Fatalf("attach billing: %v", err)
	}

	if err := n.publish(t.Context(), orderPlaced{OrderID: "settled"}); err != nil {
		t.Fatalf("publish settled: %v", err)
	}
	if err := n.publish(t.Context(), orderPlaced{OrderID: "poison"}); err != nil {
		t.Fatalf("publish poison: %v", err)
	}

	reference := time.Now().UTC().Add(time.Minute)
	deleted, err := n.store.DeleteSettledMessages(t.Context(), reference)
	if err != nil {
		t.Fatalf("delete settled messages: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("settled messages deleted = %d, want 1", deleted)
	}
	if got := countRows(t, n.database, messageTableName); got != 1 {
		t.Fatalf("message rows after settled retention = %d, want the pinned poison message", got)
	}

	// Decommissioning the listener unpins the poison message: with no
	// covering consumer left, the message is vacuously settled.
	if err := n.bridge.Detach(t.Context(), "billing"); err != nil {
		t.Fatalf("detach: %v", err)
	}
	deleted, err = n.store.DeleteSettledMessages(t.Context(), reference)
	if err != nil {
		t.Fatalf("delete settled messages after detach: %v", err)
	}
	if deleted != 1 {
		t.Fatalf("messages deleted after detach = %d, want 1", deleted)
	}

	swept, err := n.store.DeleteOrphanDeliveries(t.Context())
	if err != nil {
		t.Fatalf("delete orphan deliveries: %v", err)
	}
	if swept == 0 {
		t.Error("orphan delivery cleanup removed nothing")
	}
	if got := countRows(t, n.database, deliveryTableName); got != 0 {
		t.Errorf("delivery rows after cleanup = %d, want 0", got)
	}
}

func TestHardAgeCapRemovesUnsettledMessages(t *testing.T) {
	n := newSQLiteNode(t, newSQLitePath(t))
	seedDelivery(t, n)

	cutoff := time.Now().UTC().Add(time.Minute)
	forced, err := n.store.DeleteMessagesOlderThan(t.Context(), cutoff)
	if err != nil {
		t.Fatalf("delete messages past the maximum age: %v", err)
	}
	if forced != 1 {
		t.Fatalf("force-deleted messages = %d, want 1", forced)
	}
	if _, err := n.store.DeleteOrphanDeliveries(t.Context()); err != nil {
		t.Fatalf("delete orphan deliveries: %v", err)
	}
	if got := countRows(t, n.database, deliveryTableName); got != 0 {
		t.Errorf("delivery rows after the hard cap = %d, want 0", got)
	}
}

func TestHeartbeatReregistersAReapedConsumer(t *testing.T) {
	path := newSQLitePath(t)
	// The event is published from a node without the listener, so delivery
	// can only flow through the re-registered consumer's materialization,
	// never through the publisher's local fast path.
	publisherNode := newSQLiteNode(t, path)
	listenerNode := newSQLiteNode(t, path)
	var received recorder
	if err := AttachBroadcastListener(t.Context(), listenerNode.bridge, "cache", received.handle); err != nil {
		t.Fatalf("attach cache: %v", err)
	}
	startDispatcher(t, NewDispatcher(listenerNode.bridge,
		WithPollInterval(10*time.Millisecond),
		WithMaintenanceInterval(20*time.Millisecond)))

	// Simulate a wrong reap: another node expired this consumer while the
	// process was stalled.
	if _, err := listenerNode.database.ExecContext(t.Context(),
		"DELETE FROM "+consumerTableName); err != nil {
		t.Fatalf("reap consumer: %v", err)
	}

	waitFor(t, 5*time.Second, "the heartbeat re-registers the reaped consumer", func() bool {
		return countRows(t, listenerNode.database, consumerTableName) == 1
	})

	if err := publisherNode.publish(t.Context(), orderPlaced{OrderID: "o-1"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	waitFor(t, 5*time.Second, "the re-registered consumer keeps receiving events", func() bool {
		return received.count() == 1
	})
}

func TestExhaustedDeliveryPinsItsMessageUntilTheHardCap(t *testing.T) {
	n := newSQLiteNode(t, newSQLitePath(t),
		WithMaximumAttempts(1),
		WithRetryDelay(func(attempt int) time.Duration { return 0 }))
	err := AttachCompetingListener(t.Context(), n.bridge, "billing",
		func(ctx context.Context, event orderPlaced) error {
			return fmt.Errorf("listener rejects %s", event.OrderID)
		})
	if err != nil {
		t.Fatalf("attach billing: %v", err)
	}

	if err := n.publish(t.Context(), orderPlaced{OrderID: "poison"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	waitFor(t, 5*time.Second, "the delivery exhausts its single attempt", func() bool {
		status, _, _ := readSingleDeliveryState(t, n.database)
		return status == StatusExhausted
	})

	// The dead letter keeps its message out of settled retention, so the
	// payload stays available for Resubmit; only the hard cap removes it.
	reference := time.Now().UTC().Add(time.Minute)
	deleted, err := n.store.DeleteSettledMessages(t.Context(), reference)
	if err != nil {
		t.Fatalf("delete settled messages: %v", err)
	}
	if deleted != 0 {
		t.Fatalf("settled retention deleted %d messages, want 0 (the dead letter must pin it)", deleted)
	}

	deadLetters, err := n.bridge.FindExhausted(t.Context(), 10)
	if err != nil {
		t.Fatalf("find exhausted deliveries: %v", err)
	}
	if len(deadLetters) != 1 {
		t.Fatalf("dead letters = %d, want 1", len(deadLetters))
	}
	if deadLetters[0].Delivery.LastError == "" {
		t.Error("the dead letter carries no failure cause")
	}

	forced, err := n.store.DeleteMessagesOlderThan(t.Context(), reference)
	if err != nil {
		t.Fatalf("delete messages past the maximum age: %v", err)
	}
	if forced != 1 {
		t.Fatalf("hard age cap removed %d messages, want 1", forced)
	}
}

func TestDispatcherMaintenanceRemovesSettledMessages(t *testing.T) {
	n := newSQLiteNode(t, newSQLitePath(t))
	var received recorder
	if err := AttachCompetingListener(t.Context(), n.bridge, "billing", received.handle); err != nil {
		t.Fatalf("attach billing: %v", err)
	}
	startDispatcher(t, NewDispatcher(n.bridge,
		WithPollInterval(10*time.Millisecond),
		WithMaintenanceInterval(20*time.Millisecond),
		WithSettledRetention(time.Millisecond)))

	if err := n.publish(t.Context(), orderPlaced{OrderID: "o-1"}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	waitFor(t, 5*time.Second, "the dispatcher's own maintenance removes the settled message and its delivery", func() bool {
		return countRows(t, n.database, messageTableName) == 0 &&
			countRows(t, n.database, deliveryTableName) == 0
	})
	if received.count() != 1 {
		t.Errorf("delivered events = %d, want 1", received.count())
	}
}

func TestConcurrentPublishersDeliverEveryEvent(t *testing.T) {
	n := newSQLiteNode(t, newSQLitePath(t))
	var mu sync.Mutex
	handled := make(map[string]int)
	err := AttachCompetingListener(t.Context(), n.bridge, "billing",
		func(ctx context.Context, event orderPlaced) error {
			mu.Lock()
			defer mu.Unlock()
			handled[event.OrderID]++
			return nil
		})
	if err != nil {
		t.Fatalf("attach billing: %v", err)
	}
	startDispatcher(t, NewDispatcher(n.bridge, WithPollInterval(10*time.Millisecond)))

	const publishers = 4
	const perPublisher = 10
	var group sync.WaitGroup
	for publisher := range publishers {
		group.Add(1)
		go func() {
			defer group.Done()
			for index := range perPublisher {
				order := fmt.Sprintf("p%d-o%d", publisher, index)
				if err := n.publish(context.Background(), orderPlaced{OrderID: order}); err != nil {
					t.Errorf("publish %s: %v", order, err)
				}
			}
		}()
	}
	group.Wait()

	waitFor(t, 10*time.Second, "every event is handled", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(handled) == publishers*perPublisher
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

func TestOrderedClaimStaysSerialAgainstARevivedPredecessor(t *testing.T) {
	n := newSQLiteNode(t, newSQLitePath(t))

	record := func(ctx context.Context, event orderPlaced) error { return nil }
	err := AttachCompetingListener(t.Context(), n.bridge, "billing", record, WithOrderedDelivery())
	if err != nil {
		t.Fatalf("attach: %v", err)
	}

	first := Message{
		ID:              uuid.New(),
		EventType:       "order.placed",
		SerializedEvent: `{"OrderID":"o-first"}`,
		PublisherNode:   "seed",
		PublicationDate: time.Now().UTC().Add(-time.Minute),
	}
	second := Message{
		ID:              uuid.New(),
		EventType:       "order.placed",
		SerializedEvent: `{"OrderID":"o-second"}`,
		PublisherNode:   "seed",
		PublicationDate: time.Now().UTC(),
	}
	for _, message := range []Message{first, second} {
		if err := n.store.CreateMessage(t.Context(), n.database, message); err != nil {
			t.Fatalf("create message: %v", err)
		}
	}
	firstKey := DeliveryKey{MessageID: first.ID, ListenerID: "billing"}
	secondKey := DeliveryKey{MessageID: second.ID, ListenerID: "billing"}
	if err := n.store.CreateDeliveries(t.Context(), n.database, []DeliveryKey{firstKey, secondKey}); err != nil {
		t.Fatalf("create deliveries: %v", err)
	}

	now := time.Now().UTC()
	leaseCutoff := now.Add(-time.Hour)

	// The predecessor exhausts its budget and parks, so the successor's
	// ordered claim proceeds past it.
	claimed, err := n.store.ClaimDeliveryInOrder(t.Context(), firstKey, "one", now, leaseCutoff, 0, first.PublicationDate)
	if err != nil || !claimed {
		t.Fatalf("claim of the predecessor = %v, %v, want true", claimed, err)
	}
	settled, err := n.store.FailDelivery(t.Context(), firstKey, "one", "poison", now, 1, 1)
	if err != nil || !settled {
		t.Fatalf("exhaustion of the predecessor = %v, %v, want true", settled, err)
	}
	claimed, err = n.store.ClaimDeliveryInOrder(t.Context(), secondKey, "two", now, leaseCutoff, 0, second.PublicationDate)
	if err != nil || !claimed {
		t.Fatalf("claim of the successor = %v, %v, want true", claimed, err)
	}

	// An operator revives the dead letter while the successor is still
	// processing: the revived predecessor must wait, or two deliveries of one
	// FIFO consumer would run concurrently.
	revived, err := n.store.ResubmitDelivery(t.Context(), firstKey)
	if err != nil || !revived {
		t.Fatalf("resubmission = %v, %v, want true", revived, err)
	}
	claimed, err = n.store.ClaimDeliveryInOrder(t.Context(), firstKey, "three", now, leaseCutoff, 0, first.PublicationDate)
	if err != nil {
		t.Fatalf("claim of the revived predecessor: %v", err)
	}
	if claimed {
		t.Fatal("a revived predecessor was claimable while its successor was processing")
	}

	// Once the successor settles, the revived predecessor proceeds.
	settled, err = n.store.CompleteDelivery(t.Context(), secondKey, "two", now)
	if err != nil || !settled {
		t.Fatalf("completion of the successor = %v, %v, want true", settled, err)
	}
	claimed, err = n.store.ClaimDeliveryInOrder(t.Context(), firstKey, "four", now, leaseCutoff, 0, first.PublicationDate)
	if err != nil || !claimed {
		t.Fatalf("claim of the revived predecessor after settlement = %v, %v, want true", claimed, err)
	}
}
