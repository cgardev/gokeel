// Package bustest provides the conformance suite every Broker engine must
// pass. The in-memory engine of the eventbus package and the persistent
// engines (such as the sqlbus module) run the same suite, so the contract
// stays uniform across engines the way a database/sql driver test keeps
// drivers honest.
package bustest

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cgardev/gokeel/eventbus"
)

// Event is the payload the conformance suite publishes. Engines that persist
// events must register it on their serializer inside Harness.NewBroker.
type Event struct {
	Value int
}

// EventTypeName is the durable type name persistent engines should register
// Event under.
const EventTypeName = "bustest.event"

// Harness adapts one Broker engine to the conformance suite.
type Harness interface {
	// NewBroker constructs a fresh, started broker. The harness owns the
	// cleanup: register it on the test with t.Cleanup.
	NewBroker(t *testing.T) eventbus.Broker

	// SettleWithin returns how long the suite waits for asynchronous
	// deliveries to settle before failing an expectation. In-memory engines
	// settle in milliseconds; polling engines need room for their intervals
	// and ordering watermarks.
	SettleWithin() time.Duration
}

// retryQuickly keeps the suite fast: engines honor the configured schedule.
func retryQuickly(int) time.Duration { return 10 * time.Millisecond }

// Run exercises the full conformance suite against the engine behind the
// harness.
func Run(t *testing.T, harness Harness) {
	t.Run("DeliversEveryEventToEveryMatchingConsumerExactlyOnce", func(t *testing.T) {
		deliversExactlyOnce(t, harness)
	})
	t.Run("PreservesPublicationOrderForAFIFOConsumerAcrossRetries", func(t *testing.T) {
		preservesFIFOOrder(t, harness)
	})
	t.Run("UnorderedConsumerDoesNotBlockBehindAFailingEvent", func(t *testing.T) {
		unorderedDoesNotBlock(t, harness)
	})
	t.Run("ConsumersRetryIndependentlyOfEachOther", func(t *testing.T) {
		retriesAreIndependent(t, harness)
	})
	t.Run("ExhaustedEventParksAsADeadLetterAndTheQueueContinues", func(t *testing.T) {
		parksAndContinues(t, harness)
	})
	t.Run("ResubmitRevivesADeadLetter", func(t *testing.T) {
		resubmitRevives(t, harness)
	})
}

// waitFor polls the condition until it holds or the settle budget elapses.
func waitFor(t *testing.T, harness Harness, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(harness.SettleWithin())
	for {
		if condition() {
			return
		}
		if time.Now().After(deadline) {
			t.Fatalf("condition not reached within the settle budget: %s", description)
		}
		time.Sleep(10 * time.Millisecond)
	}
}

// settle waits one settle budget and asserts the condition still holds, for
// expectations of the form "and nothing else happened".
func settle(t *testing.T, harness Harness, description string, condition func() bool) {
	t.Helper()
	time.Sleep(harness.SettleWithin())
	if !condition() {
		t.Fatalf("condition did not hold after settling: %s", description)
	}
}

func deliversExactlyOnce(t *testing.T, harness Harness) {
	broker := harness.NewBroker(t)
	const events = 20

	var first, second atomic.Int64
	err := eventbus.Consume(t.Context(), broker, "conformance-first",
		func(ctx context.Context, event Event) error {
			first.Add(1)
			return nil
		}, eventbus.WithRetryDelay(retryQuickly))
	if err != nil {
		t.Fatalf("subscribe first: %v", err)
	}
	err = eventbus.Consume(t.Context(), broker, "conformance-second",
		func(ctx context.Context, event Event) error {
			second.Add(1)
			return nil
		}, eventbus.WithUnorderedDelivery(), eventbus.WithRetryDelay(retryQuickly))
	if err != nil {
		t.Fatalf("subscribe second: %v", err)
	}

	for value := 0; value < events; value++ {
		if err := broker.Publish(t.Context(), Event{Value: value}); err != nil {
			t.Fatalf("publish %d: %v", value, err)
		}
	}

	waitFor(t, harness, "both consumers received every event", func() bool {
		return first.Load() == events && second.Load() == events
	})
	settle(t, harness, "no duplicate deliveries arrived", func() bool {
		return first.Load() == events && second.Load() == events
	})
}

func preservesFIFOOrder(t *testing.T, harness Harness) {
	broker := harness.NewBroker(t)
	const events = 10
	const failing = 4

	var mu sync.Mutex
	var order []int
	failuresLeft := 2
	err := eventbus.Consume(t.Context(), broker, "conformance-fifo",
		func(ctx context.Context, event Event) error {
			mu.Lock()
			defer mu.Unlock()
			// One event in the middle fails twice: FIFO must hold its
			// successors back and still deliver everything in order.
			if event.Value == failing && failuresLeft > 0 {
				failuresLeft--
				return errors.New("transient failure")
			}
			order = append(order, event.Value)
			return nil
		}, eventbus.WithRetryDelay(retryQuickly))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	for value := 0; value < events; value++ {
		if err := broker.Publish(t.Context(), Event{Value: value}); err != nil {
			t.Fatalf("publish %d: %v", value, err)
		}
	}

	waitFor(t, harness, "every event was delivered", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(order) == events
	})

	mu.Lock()
	defer mu.Unlock()
	for position, value := range order {
		if value != position {
			t.Fatalf("delivery order = %v, want strictly ascending despite the retries", order)
		}
	}
}

func unorderedDoesNotBlock(t *testing.T, harness Harness) {
	broker := harness.NewBroker(t)
	const events = 8

	var mu sync.Mutex
	delivered := make(map[int]bool)
	var blockedDelivered atomic.Bool
	release := make(chan struct{})
	var releaseOnce sync.Once

	err := eventbus.Consume(t.Context(), broker, "conformance-unordered",
		func(ctx context.Context, event Event) error {
			if event.Value == 0 {
				select {
				case <-release:
					blockedDelivered.Store(true)
					return nil
				default:
					return errors.New("not ready yet")
				}
			}
			mu.Lock()
			delivered[event.Value] = true
			mu.Unlock()
			return nil
		}, eventbus.WithUnorderedDelivery(), eventbus.WithRetryDelay(retryQuickly),
		eventbus.WithMaximumAttempts(1000))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	for value := 0; value < events; value++ {
		if err := broker.Publish(t.Context(), Event{Value: value}); err != nil {
			t.Fatalf("publish %d: %v", value, err)
		}
	}

	// Every other event completes while the first keeps failing: an
	// unordered consumer must not block behind it.
	waitFor(t, harness, "the later events were delivered while the first still fails", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(delivered) == events-1
	})
	if blockedDelivered.Load() {
		t.Fatal("the failing event completed before it was released")
	}

	releaseOnce.Do(func() { close(release) })
	waitFor(t, harness, "the failing event eventually recovered", func() bool {
		return blockedDelivered.Load()
	})
}

func retriesAreIndependent(t *testing.T, harness Harness) {
	broker := harness.NewBroker(t)

	var healthy atomic.Int64
	var flakyAttempts atomic.Int64
	err := eventbus.Consume(t.Context(), broker, "conformance-healthy",
		func(ctx context.Context, event Event) error {
			healthy.Add(1)
			return nil
		}, eventbus.WithRetryDelay(retryQuickly))
	if err != nil {
		t.Fatalf("subscribe healthy: %v", err)
	}
	err = eventbus.Consume(t.Context(), broker, "conformance-flaky",
		func(ctx context.Context, event Event) error {
			if flakyAttempts.Add(1) < 3 {
				return errors.New("transient failure")
			}
			return nil
		}, eventbus.WithRetryDelay(retryQuickly))
	if err != nil {
		t.Fatalf("subscribe flaky: %v", err)
	}

	if err := broker.Publish(t.Context(), Event{Value: 1}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	waitFor(t, harness, "the flaky consumer retried until success", func() bool {
		return flakyAttempts.Load() == 3
	})
	settle(t, harness, "the healthy consumer was invoked exactly once", func() bool {
		return healthy.Load() == 1
	})
}

func parksAndContinues(t *testing.T, harness Harness) {
	broker := harness.NewBroker(t)

	var mu sync.Mutex
	var order []int
	err := eventbus.Consume(t.Context(), broker, "conformance-parking",
		func(ctx context.Context, event Event) error {
			if event.Value == 0 {
				return errors.New("permanent failure")
			}
			mu.Lock()
			order = append(order, event.Value)
			mu.Unlock()
			return nil
		}, eventbus.WithRetryDelay(retryQuickly), eventbus.WithMaximumAttempts(2))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	for value := 0; value < 3; value++ {
		if err := broker.Publish(t.Context(), Event{Value: value}); err != nil {
			t.Fatalf("publish %d: %v", value, err)
		}
	}

	// The poisoned head consumes its budget, parks, and the successors flow.
	waitFor(t, harness, "the successors were delivered after the head parked", func() bool {
		mu.Lock()
		defer mu.Unlock()
		return len(order) == 2
	})
	waitFor(t, harness, "the exhausted event became a dead letter", func() bool {
		letters, err := broker.FindExhausted(t.Context(), 10)
		if err != nil {
			t.Fatalf("find exhausted: %v", err)
		}
		return len(letters) == 1 && letters[0].Attempts == 2
	})

	mu.Lock()
	defer mu.Unlock()
	if order[0] != 1 || order[1] != 2 {
		t.Fatalf("successor order = %v, want [1 2]", order)
	}
}

func resubmitRevives(t *testing.T, harness Harness) {
	broker := harness.NewBroker(t)

	var failing atomic.Bool
	failing.Store(true)
	var delivered atomic.Int64
	err := eventbus.Consume(t.Context(), broker, "conformance-revival",
		func(ctx context.Context, event Event) error {
			if failing.Load() {
				return errors.New("collaborator unavailable")
			}
			delivered.Add(1)
			return nil
		}, eventbus.WithRetryDelay(retryQuickly), eventbus.WithMaximumAttempts(2))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	if err := broker.Publish(t.Context(), Event{Value: 42}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	var reference string
	waitFor(t, harness, "the event was parked as a dead letter", func() bool {
		letters, err := broker.FindExhausted(t.Context(), 10)
		if err != nil {
			t.Fatalf("find exhausted: %v", err)
		}
		if len(letters) != 1 {
			return false
		}
		reference = letters[0].Reference
		return true
	})

	failing.Store(false)
	revived, err := broker.Resubmit(t.Context(), reference)
	if err != nil {
		t.Fatalf("resubmit: %v", err)
	}
	if !revived {
		t.Fatal("resubmission of a known dead letter reported false")
	}

	waitFor(t, harness, "the revived event was delivered", func() bool {
		return delivered.Load() == 1
	})
	waitFor(t, harness, "the dead letter was consumed by the resubmission", func() bool {
		letters, err := broker.FindExhausted(t.Context(), 10)
		if err != nil {
			t.Fatalf("find exhausted: %v", err)
		}
		return len(letters) == 0
	})

	unknown, err := broker.Resubmit(t.Context(), fmt.Sprintf("unknown-%d", time.Now().UnixNano()))
	if err != nil {
		t.Fatalf("resubmit unknown reference: %v", err)
	}
	if unknown {
		t.Fatal("resubmission of an unknown reference reported true")
	}
}
