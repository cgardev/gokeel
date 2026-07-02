package sqlbus

import (
	"context"
	"errors"
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
	// Once every delivery row is completed, the claim guards forbid any
	// further handler invocation, so the counts below are final.
	waitFor(t, 10*time.Second, "every delivery settles as completed", func() bool {
		return countDeliveriesWithStatus(t, first.database, StatusCompleted) == published
	})

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
	// One completed delivery row per message and node makes the counts final.
	waitFor(t, 10*time.Second, "every delivery settles as completed", func() bool {
		return countDeliveriesWithStatus(t, first.database, StatusCompleted) == 2*published
	})
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

	// Without a wake signal the next pass is an hour away: the event must
	// still be undelivered, or the assertion below would pass vacuously.
	time.Sleep(200 * time.Millisecond)
	if received.count() != 0 {
		t.Fatal("the event was delivered before the wake signal; the poll interval did not isolate the wake path")
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

	waitFor(t, 5*time.Second, "the fresh event is delivered and settled", func() bool {
		return received.count() >= 1 &&
			countDeliveriesWithStatus(t, listenerNode.database, StatusCompleted) == 1
	})
	for _, event := range received.snapshot() {
		if event.OrderID == "before-attachment" {
			t.Error("an event published before the attachment boundary was replayed")
		}
	}
	if got := countRows(t, listenerNode.database, deliveryTableName); got != 1 {
		t.Errorf("delivery rows = %d, want 1 (history must not be materialized)", got)
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

func TestAnotherNodeStealsAnExpiredClaim(t *testing.T) {
	path := newSQLitePath(t)
	lease := WithLeaseDuration(100 * time.Millisecond)
	stalled := newSQLiteNode(t, path, lease)
	rescuer := newSQLiteNode(t, path, lease)

	started := make(chan struct{})
	release := make(chan struct{})
	err := AttachCompetingListener(t.Context(), stalled.bridge, "billing",
		func(ctx context.Context, event orderPlaced) error {
			close(started)
			<-release
			return nil
		})
	if err != nil {
		t.Fatalf("attach billing on the stalled node: %v", err)
	}
	var rescued recorder
	if err := AttachCompetingListener(t.Context(), rescuer.bridge, "billing", rescued.handle); err != nil {
		t.Fatalf("attach billing on the rescuing node: %v", err)
	}

	// The asynchronous publisher claims the pre-created delivery on the
	// stalled node, whose listener then hangs past the claim lease. The
	// rescuing dispatcher starts only once that claim is held, so the steal
	// path is the only way the event can complete.
	if err := stalled.manager.Run(t.Context(), func(ctx context.Context) error {
		return stalled.publisher.WithAsynchronousDispatch().Publish(ctx, orderPlaced{OrderID: "o-1"})
	}); err != nil {
		t.Fatalf("publish: %v", err)
	}
	select {
	case <-started:
	case <-time.After(5 * time.Second):
		t.Fatal("the stalled node never claimed its local delivery")
	}
	startDispatcher(t, NewDispatcher(rescuer.bridge, WithPollInterval(10*time.Millisecond)))

	waitFor(t, 5*time.Second, "the rescuing node steals the expired claim and completes", func() bool {
		return rescued.count() == 1 &&
			countDeliveriesWithStatus(t, rescuer.database, StatusCompleted) == 1
	})

	// Releasing the stalled listener lets its zombie settlement run; the
	// claim-token fence must discard it without reverting the completion.
	close(release)
	time.Sleep(100 * time.Millisecond)
	status, attempts, _ := readSingleDeliveryState(t, rescuer.database)
	if status != StatusCompleted {
		t.Fatalf("delivery status after the zombie settlement = %s, want COMPLETED", status)
	}
	if attempts != 2 {
		t.Errorf("recorded attempts = %d, want 2 (original claim and steal)", attempts)
	}
}

func TestFrontierAndDeliveriesSurviveANodeRestart(t *testing.T) {
	path := newSQLitePath(t)
	publisherNode := newSQLiteNode(t, path)

	firstIncarnation := newSQLiteNode(t, path)
	var beforeRestart recorder
	if err := AttachCompetingListener(t.Context(), firstIncarnation.bridge, "billing", beforeRestart.handle); err != nil {
		t.Fatalf("attach billing before the restart: %v", err)
	}
	stop := NewDispatcher(firstIncarnation.bridge, WithPollInterval(10*time.Millisecond)).Start()
	if err := publisherNode.publish(t.Context(), orderPlaced{OrderID: "before-restart"}); err != nil {
		t.Fatalf("publish before the restart: %v", err)
	}
	waitFor(t, 5*time.Second, "the first incarnation delivers and settles", func() bool {
		return beforeRestart.count() == 1 &&
			countDeliveriesWithStatus(t, publisherNode.database, StatusCompleted) == 1
	})
	stop()

	// The restarted node re-attaches the same durable competing group; the
	// completed delivery row and the stored frontier must prevent any replay.
	secondIncarnation := newSQLiteNode(t, path)
	var afterRestart recorder
	if err := AttachCompetingListener(t.Context(), secondIncarnation.bridge, "billing", afterRestart.handle); err != nil {
		t.Fatalf("attach billing after the restart: %v", err)
	}
	startDispatcher(t, NewDispatcher(secondIncarnation.bridge, WithPollInterval(10*time.Millisecond)))

	if err := publisherNode.publish(t.Context(), orderPlaced{OrderID: "after-restart"}); err != nil {
		t.Fatalf("publish after the restart: %v", err)
	}
	waitFor(t, 5*time.Second, "the second incarnation delivers the fresh event", func() bool {
		return afterRestart.count() >= 1 &&
			countDeliveriesWithStatus(t, publisherNode.database, StatusCompleted) == 2
	})
	for _, event := range afterRestart.snapshot() {
		if event.OrderID == "before-restart" {
			t.Error("the restarted node replayed an event its previous incarnation had completed")
		}
	}
	if got := countRows(t, publisherNode.database, deliveryTableName); got != 2 {
		t.Errorf("delivery rows = %d, want 2", got)
	}
}

func TestListenersReceiveOnlyTheirEventType(t *testing.T) {
	path := newSQLitePath(t)
	publisherNode := newSQLiteNode(t, path)
	listenerNode := newSQLiteNode(t, path)

	var received recorder
	if err := AttachCompetingListener(t.Context(), listenerNode.bridge, "billing", received.handle); err != nil {
		t.Fatalf("attach billing: %v", err)
	}
	startDispatcher(t, NewDispatcher(listenerNode.bridge, WithPollInterval(10*time.Millisecond)))

	if err := publisherNode.publish(t.Context(), orderCancelled{OrderID: "c-1"}); err != nil {
		t.Fatalf("publish the cancellation: %v", err)
	}
	if err := publisherNode.publish(t.Context(), orderPlaced{OrderID: "o-1"}); err != nil {
		t.Fatalf("publish the placement: %v", err)
	}

	waitFor(t, 5*time.Second, "the placement is delivered and settled", func() bool {
		return countDeliveriesWithStatus(t, listenerNode.database, StatusCompleted) == 1
	})
	events := received.snapshot()
	if len(events) != 1 || events[0].OrderID != "o-1" {
		t.Fatalf("listener received %+v, want only the placement o-1", events)
	}
	if got := countRows(t, listenerNode.database, deliveryTableName); got != 1 {
		t.Errorf("delivery rows = %d, want 1 (the cancellation must not be materialized)", got)
	}
	if got := countRows(t, listenerNode.database, messageTableName); got != 2 {
		t.Errorf("message rows = %d, want 2", got)
	}
}

func TestOrderedListenerProcessesEventsInPublicationOrderAcrossNodes(t *testing.T) {
	path := newSQLitePath(t)
	grace := WithMaterializationGrace(50 * time.Millisecond)
	first := newSQLiteNode(t, path, grace)
	second := newSQLiteNode(t, path, grace)

	var mu sync.Mutex
	var order []string
	failuresLeft := 2
	record := func(ctx context.Context, event orderPlaced) error {
		mu.Lock()
		defer mu.Unlock()
		// One event in the middle fails twice: the FIFO queue must hold its
		// successors back cluster-wide and still deliver everything in order.
		if event.OrderID == "o-3" && failuresLeft > 0 {
			failuresLeft--
			return errors.New("transient failure")
		}
		order = append(order, event.OrderID)
		return nil
	}
	quickRetry := WithListenerRetryDelay(func(int) time.Duration { return 5 * time.Millisecond })
	err := AttachCompetingListener(t.Context(), first.bridge, "billing", record,
		WithOrderedDelivery(), quickRetry)
	if err != nil {
		t.Fatalf("attach billing on the first node: %v", err)
	}
	err = AttachCompetingListener(t.Context(), second.bridge, "billing", record,
		WithOrderedDelivery(), quickRetry)
	if err != nil {
		t.Fatalf("attach billing on the second node: %v", err)
	}
	startDispatcher(t, NewDispatcher(first.bridge, WithPollInterval(5*time.Millisecond)))
	startDispatcher(t, NewDispatcher(second.bridge, WithPollInterval(5*time.Millisecond)))

	const published = 10
	for index := range published {
		origin := first
		if index%2 == 1 {
			origin = second
		}
		if err := origin.publish(t.Context(), orderPlaced{OrderID: fmt.Sprintf("o-%d", index)}); err != nil {
			t.Fatalf("publish o-%d: %v", index, err)
		}
	}

	waitFor(t, 20*time.Second, "every event is handled in order", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(order) == published
	})

	mu.Lock()
	defer mu.Unlock()
	for index, orderID := range order {
		if expected := fmt.Sprintf("o-%d", index); orderID != expected {
			t.Fatalf("processing order = %v, want strictly ascending publication order", order)
		}
	}
}

func TestConflictingOrderingRegistrationIsRejected(t *testing.T) {
	path := newSQLitePath(t)
	first := newSQLiteNode(t, path)
	second := newSQLiteNode(t, path)

	record := func(ctx context.Context, event orderPlaced) error { return nil }
	err := AttachCompetingListener(t.Context(), first.bridge, "billing", record, WithOrderedDelivery())
	if err != nil {
		t.Fatalf("ordered attachment: %v", err)
	}
	err = AttachCompetingListener(t.Context(), second.bridge, "billing", record)
	if !errors.Is(err, ErrConflictingOrdering) {
		t.Fatalf("conflicting ordering error = %v, want ErrConflictingOrdering", err)
	}
}
