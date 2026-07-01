package sqlbus

import (
	"context"
	"fmt"
	"sync"
	"testing"
	"time"
)

func TestEventPublishedOnOneNodeReachesAListenerHostedOnlyOnAnotherNode(t *testing.T) {
	path := newSQLitePath(t)
	publisherNode := newSQLiteNode(t, path)
	listenerNode := newSQLiteNode(t, path)

	var received recorder
	if err := AttachCompetingListener(t.Context(), listenerNode.bridge, "billing", received.handle); err != nil {
		t.Fatalf("attach billing: %v", err)
	}
	startDispatcher(t, NewDispatcher(listenerNode.bridge, WithPollInterval(10*time.Millisecond)))

	if err := publisherNode.publish(t.Context(), orderPlaced{OrderID: "o-1"}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	waitFor(t, 5*time.Second, "the remote node delivers the event", func() bool {
		return received.count() == 1
	})
	if got := received.snapshot()[0].OrderID; got != "o-1" {
		t.Errorf("remote listener received order %s, want o-1", got)
	}
	waitFor(t, 5*time.Second, "the delivery settles as completed", func() bool {
		status, _, _ := readSingleDeliveryState(t, listenerNode.database)
		return status == StatusCompleted
	})
}

func TestCompetingListenerHandlesEachEventExactlyOnceAcrossNodes(t *testing.T) {
	path := newSQLitePath(t)
	first := newSQLiteNode(t, path)
	second := newSQLiteNode(t, path)

	var mu sync.Mutex
	handled := make(map[string]int)
	record := func(ctx context.Context, event orderPlaced) error {
		mu.Lock()
		defer mu.Unlock()
		handled[event.OrderID]++
		return nil
	}
	if err := AttachCompetingListener(t.Context(), first.bridge, "billing", record); err != nil {
		t.Fatalf("attach billing on the first node: %v", err)
	}
	if err := AttachCompetingListener(t.Context(), second.bridge, "billing", record); err != nil {
		t.Fatalf("attach billing on the second node: %v", err)
	}
	startDispatcher(t, NewDispatcher(first.bridge, WithPollInterval(5*time.Millisecond)))
	startDispatcher(t, NewDispatcher(second.bridge, WithPollInterval(5*time.Millisecond)))

	const published = 20
	for index := range published {
		origin := first
		if index%2 == 1 {
			origin = second
		}
		if err := origin.publish(t.Context(), orderPlaced{OrderID: fmt.Sprintf("o-%d", index)}); err != nil {
			t.Fatalf("publish o-%d: %v", index, err)
		}
	}

	waitFor(t, 10*time.Second, "every event is handled", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(handled) == published
	})
	// A duplicate would arrive shortly after the first delivery; give the
	// cluster a moment to produce one before asserting exactly-once.
	time.Sleep(100 * time.Millisecond)

	mu.Lock()
	defer mu.Unlock()
	for order, count := range handled {
		if count != 1 {
			t.Errorf("order %s was handled %d times, want exactly 1", order, count)
		}
	}
}

func TestBroadcastListenerHandlesEachEventOncePerNode(t *testing.T) {
	path := newSQLitePath(t)
	first := newSQLiteNode(t, path)
	second := newSQLiteNode(t, path)

	var firstReceived, secondReceived recorder
	if err := AttachBroadcastListener(t.Context(), first.bridge, "cache", firstReceived.handle); err != nil {
		t.Fatalf("attach cache on the first node: %v", err)
	}
	if err := AttachBroadcastListener(t.Context(), second.bridge, "cache", secondReceived.handle); err != nil {
		t.Fatalf("attach cache on the second node: %v", err)
	}
	startDispatcher(t, NewDispatcher(first.bridge, WithPollInterval(5*time.Millisecond)))
	startDispatcher(t, NewDispatcher(second.bridge, WithPollInterval(5*time.Millisecond)))

	const published = 5
	for index := range published {
		if err := first.publish(t.Context(), orderPlaced{OrderID: fmt.Sprintf("o-%d", index)}); err != nil {
			t.Fatalf("publish o-%d: %v", index, err)
		}
	}

	waitFor(t, 10*time.Second, "both nodes deliver every event", func() bool {
		return firstReceived.count() == published && secondReceived.count() == published
	})
	time.Sleep(100 * time.Millisecond)
	if firstReceived.count() != published || secondReceived.count() != published {
		t.Errorf("deliveries first=%d second=%d, want %d on each node",
			firstReceived.count(), secondReceived.count(), published)
	}
}

func TestWakeSignalTriggersAnImmediatePass(t *testing.T) {
	path := newSQLitePath(t)
	publisherNode := newSQLiteNode(t, path)
	listenerNode := newSQLiteNode(t, path)

	var received recorder
	if err := AttachCompetingListener(t.Context(), listenerNode.bridge, "billing", received.handle); err != nil {
		t.Fatalf("attach billing: %v", err)
	}
	wake := make(chan struct{}, 1)
	startDispatcher(t, NewDispatcher(listenerNode.bridge,
		WithPollInterval(time.Hour), WithWakeSignal(wake)))

	// Let the immediate first pass finish before publishing, so only the wake
	// signal can trigger the delivery within the test's patience.
	time.Sleep(100 * time.Millisecond)
	if err := publisherNode.publish(t.Context(), orderPlaced{OrderID: "o-1"}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	wake <- struct{}{}

	waitFor(t, 5*time.Second, "the wake signal delivers the event without waiting for the poll", func() bool {
		return received.count() == 1
	})
}

func TestMaterializationDoesNotReplayHistoryFromBeforeAttachment(t *testing.T) {
	path := newSQLitePath(t)
	publisherNode := newSQLiteNode(t, path)
	if err := publisherNode.publish(t.Context(), orderPlaced{OrderID: "before-attachment"}); err != nil {
		t.Fatalf("publish history: %v", err)
	}
	time.Sleep(200 * time.Millisecond)

	listenerNode := newSQLiteNode(t, path, WithMaterializationGrace(50*time.Millisecond))
	var received recorder
	if err := AttachCompetingListener(t.Context(), listenerNode.bridge, "billing", received.handle); err != nil {
		t.Fatalf("attach billing: %v", err)
	}
	startDispatcher(t, NewDispatcher(listenerNode.bridge, WithPollInterval(10*time.Millisecond)))

	if err := publisherNode.publish(t.Context(), orderPlaced{OrderID: "after-attachment"}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	waitFor(t, 5*time.Second, "the fresh event is delivered", func() bool {
		return received.count() >= 1
	})
	time.Sleep(100 * time.Millisecond)
	for _, event := range received.snapshot() {
		if event.OrderID == "before-attachment" {
			t.Error("an event published before the attachment boundary was replayed")
		}
	}
	if received.count() != 1 {
		t.Errorf("delivered events = %d, want 1", received.count())
	}
}

func TestDispatcherDeliversInPublicationOrder(t *testing.T) {
	path := newSQLitePath(t)
	publisherNode := newSQLiteNode(t, path)
	listenerNode := newSQLiteNode(t, path)

	var received recorder
	if err := AttachCompetingListener(t.Context(), listenerNode.bridge, "billing", received.handle); err != nil {
		t.Fatalf("attach billing: %v", err)
	}

	const published = 5
	for index := range published {
		if err := publisherNode.publish(t.Context(), orderPlaced{OrderID: fmt.Sprintf("o-%d", index)}); err != nil {
			t.Fatalf("publish o-%d: %v", index, err)
		}
	}
	startDispatcher(t, NewDispatcher(listenerNode.bridge, WithPollInterval(10*time.Millisecond)))

	waitFor(t, 5*time.Second, "every event is delivered", func() bool {
		return received.count() == published
	})
	for index, event := range received.snapshot() {
		if want := fmt.Sprintf("o-%d", index); event.OrderID != want {
			t.Errorf("delivery %d received order %s, want %s", index, event.OrderID, want)
		}
	}
}

func TestBroadcastConsumerOfADeadNodeExpires(t *testing.T) {
	path := newSQLitePath(t)
	survivor := newSQLiteNode(t, path)
	dead := newSQLiteNode(t, path)

	var receivedSurvivor, receivedDead recorder
	if err := AttachCompetingListener(t.Context(), survivor.bridge, "billing", receivedSurvivor.handle); err != nil {
		t.Fatalf("attach billing: %v", err)
	}
	if err := AttachBroadcastListener(t.Context(), dead.bridge, "cache", receivedDead.handle); err != nil {
		t.Fatalf("attach cache: %v", err)
	}
	// The dead node never starts a dispatcher, so its broadcast consumer
	// stops heartbeating at its attachment time.
	startDispatcher(t, NewDispatcher(survivor.bridge,
		WithPollInterval(10*time.Millisecond),
		WithMaintenanceInterval(20*time.Millisecond),
		WithConsumerExpiry(100*time.Millisecond)))

	waitFor(t, 5*time.Second, "the dead node's broadcast consumer is reaped", func() bool {
		var remaining int64
		row := survivor.database.QueryRowContext(t.Context(),
			"SELECT COUNT(*) FROM "+consumerTableName+" WHERE delivery_mode = ?",
			string(DeliveryModeBroadcast))
		if err := row.Scan(&remaining); err != nil {
			t.Fatalf("count broadcast consumers: %v", err)
		}
		return remaining == 0
	})
	if got := countRows(t, survivor.database, consumerTableName); got != 1 {
		t.Errorf("consumer rows after expiry = %d, want only the survivor's competing registration", got)
	}
}
