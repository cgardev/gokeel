package eventbus_test

import (
	"context"
	"errors"
	"fmt"
	"runtime"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cgardev/gokeel/eventbus"
)

// TestMemoryBrokerReclaimsEveryGoroutineAfterStop is the goroutine-leak
// regression: workers, retry waiters, and internal machinery must all return
// on Stop, even after a load that schedules many retries.
func TestMemoryBrokerReclaimsEveryGoroutineAfterStop(t *testing.T) {
	baseline := runtime.NumGoroutine()

	broker := eventbus.NewMemoryBroker()
	var delivered atomic.Int64
	attempts := sync.Map{}
	err := eventbus.Consume(t.Context(), broker, "flaky",
		func(ctx context.Context, event stressEvent) error {
			count, _ := attempts.LoadOrStore(event.Value, new(atomic.Int64))
			if count.(*atomic.Int64).Add(1) == 1 {
				return errors.New("first attempt fails, scheduling a retry")
			}
			delivered.Add(1)
			return nil
		},
		eventbus.WithUnorderedDelivery(),
		eventbus.WithWorkers(8),
		eventbus.WithRetryDelay(func(int) time.Duration { return time.Millisecond }))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	const events = 500
	for value := 0; value < events; value++ {
		if err := broker.Publish(t.Context(), stressEvent{Value: value}); err != nil {
			t.Fatalf("publish %d: %v", value, err)
		}
	}
	waitUntil(t, 20*time.Second, "every event settled after one retry", func() bool {
		return delivered.Load() == events
	})

	broker.Stop()

	// The runtime needs a moment to unwind the stopped goroutines.
	waitUntil(t, 10*time.Second, "goroutines returned to the baseline", func() bool {
		runtime.GC()
		return runtime.NumGoroutine() <= baseline+2
	})
}

// stressEvent is a local event payload for the stress tests.
type stressEvent struct {
	Value int
}

// TestUnorderedConsumerIsNotStrandedBehindABusyWorker is the lost-wakeup
// regression: with one worker occupied by a blocking handler, the remaining
// workers must still pick up newly enqueued events. Before the condition-
// variable wakeup protocol, a dropped signal could leave a ready event
// stranded while a worker slept.
func TestUnorderedConsumerIsNotStrandedBehindABusyWorker(t *testing.T) {
	broker := eventbus.NewMemoryBroker()
	t.Cleanup(broker.Stop)

	release := make(chan struct{})
	var second atomic.Bool
	err := eventbus.Consume(t.Context(), broker, "blocking",
		func(ctx context.Context, event stressEvent) error {
			if event.Value == 1 {
				// The first event blocks its worker until the second event
				// has been delivered by the other worker.
				select {
				case <-release:
					return nil
				case <-ctx.Done():
					return ctx.Err()
				}
			}
			second.Store(true)
			close(release)
			return nil
		},
		eventbus.WithUnorderedDelivery(), eventbus.WithWorkers(2))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	if err := broker.Publish(t.Context(), stressEvent{Value: 1}); err != nil {
		t.Fatalf("publish first: %v", err)
	}
	// Let the first event occupy its worker before the second arrives, so the
	// wakeup for the second event must reach the remaining idle worker.
	time.Sleep(20 * time.Millisecond)
	if err := broker.Publish(t.Context(), stressEvent{Value: 2}); err != nil {
		t.Fatalf("publish second: %v", err)
	}

	waitUntil(t, 5*time.Second, "the second event was delivered while the first blocked", func() bool {
		return second.Load()
	})
}

// TestMemoryBrokerSurvivesConcurrentChaos hammers one broker from every
// public entry point at once — publishers, subscribers, dead-letter readers,
// resubmitters, and a late Stop — and asserts the exactly-once count on a
// stable consumer. Its value multiplies under the race detector.
func TestMemoryBrokerSurvivesConcurrentChaos(t *testing.T) {
	broker := eventbus.NewMemoryBroker()

	var delivered atomic.Int64
	err := eventbus.Consume(t.Context(), broker, "stable",
		func(ctx context.Context, event stressEvent) error {
			delivered.Add(1)
			return nil
		},
		eventbus.WithUnorderedDelivery(), eventbus.WithWorkers(4))
	if err != nil {
		t.Fatalf("subscribe stable: %v", err)
	}
	err = eventbus.Consume(t.Context(), broker, "poison",
		func(ctx context.Context, event stressEvent) error {
			return errors.New("always fails")
		},
		eventbus.WithUnorderedDelivery(),
		eventbus.WithMaximumAttempts(2),
		eventbus.WithRetryDelay(func(int) time.Duration { return time.Millisecond }))
	if err != nil {
		t.Fatalf("subscribe poison: %v", err)
	}

	const publishers = 8
	const eventsPerPublisher = 50
	var group sync.WaitGroup

	for publisher := 0; publisher < publishers; publisher++ {
		group.Add(1)
		go func(publisher int) {
			defer group.Done()
			for event := 0; event < eventsPerPublisher; event++ {
				_ = broker.Publish(context.Background(), stressEvent{Value: publisher*eventsPerPublisher + event})
			}
		}(publisher)
	}

	// Concurrent operators inspect and revive dead letters while the
	// publishers run.
	stopOperators := make(chan struct{})
	for operator := 0; operator < 2; operator++ {
		group.Add(1)
		go func() {
			defer group.Done()
			for {
				select {
				case <-stopOperators:
					return
				default:
				}
				letters, err := broker.FindExhausted(context.Background(), 10)
				if err != nil {
					return
				}
				for _, letter := range letters {
					_, _ = broker.Resubmit(context.Background(), letter.Reference)
				}
			}
		}()
	}

	// Concurrent subscribers keep registering fresh consumers.
	for subscriber := 0; subscriber < 4; subscriber++ {
		group.Add(1)
		go func(subscriber int) {
			defer group.Done()
			id := eventbus.ListenerID(fmt.Sprintf("late-%d", subscriber))
			_ = eventbus.Consume(context.Background(), broker, id,
				func(ctx context.Context, event stressEvent) error { return nil },
				eventbus.WithUnorderedDelivery())
		}(subscriber)
	}

	waitUntil(t, 20*time.Second, "the stable consumer received every event", func() bool {
		return delivered.Load() == publishers*eventsPerPublisher
	})
	close(stopOperators)
	group.Wait()

	// Stop must terminate cleanly despite everything above; a second Stop
	// must be a no-op.
	broker.Stop()
	broker.Stop()

	if got := delivered.Load(); got != publishers*eventsPerPublisher {
		t.Fatalf("stable consumer deliveries = %d, want exactly %d", got, publishers*eventsPerPublisher)
	}
}
