package eventbus_test

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

// fuzzEvent is the payload the broker fuzz targets publish; the byte value
// drives the deterministic failure behavior of the consumers.
type fuzzEvent struct {
	Sequence int
	Value    byte
}

func quickRetry(int) time.Duration { return time.Millisecond }

// waitUntil polls the condition until it holds or the deadline expires.
func waitUntil(t *testing.T, timeout time.Duration, description string, condition func() bool) {
	t.Helper()
	deadline := time.Now().Add(timeout)
	for !condition() {
		if time.Now().After(deadline) {
			t.Fatalf("condition not reached: %s", description)
		}
		time.Sleep(time.Millisecond)
	}
}

// FuzzMemoryBrokerContractInvariants drives the memory broker with an
// arbitrary event sequence and asserts the contract on every input: exactly
// one delivery per consumer per event, publication order preserved by FIFO
// consumers across transient failures, permanent failures parked as dead
// letters without blocking their successors, and resubmission reviving every
// dead letter exactly once.
func FuzzMemoryBrokerContractInvariants(f *testing.F) {
	f.Add([]byte{0, 1, 2, 3, 4, 5, 6, 7, 8, 9})
	f.Add([]byte{0, 0, 0, 0})
	f.Add([]byte{5})
	f.Add([]byte{4, 8, 12, 16, 4, 8})
	f.Add([]byte{})

	f.Fuzz(func(t *testing.T, values []byte) {
		if len(values) > 32 {
			values = values[:32]
		}
		broker := eventbus.NewMemoryBroker()
		t.Cleanup(broker.Stop)

		// The recorder accepts everything: its log must equal the exact
		// publication order.
		var recorderMu sync.Mutex
		var recorded []int
		err := eventbus.Consume(t.Context(), broker, "recorder",
			func(ctx context.Context, event fuzzEvent) error {
				recorderMu.Lock()
				defer recorderMu.Unlock()
				recorded = append(recorded, event.Sequence)
				return nil
			}, eventbus.WithRetryDelay(quickRetry))
		if err != nil {
			t.Fatalf("subscribe recorder: %v", err)
		}

		// The flaky consumer fails the first attempt of every multiple of
		// four: FIFO must hold its successors back and still deliver
		// everything in publication order.
		var flakyMu sync.Mutex
		var flaky []int
		failed := make(map[int]bool)
		err = eventbus.Consume(t.Context(), broker, "flaky",
			func(ctx context.Context, event fuzzEvent) error {
				flakyMu.Lock()
				defer flakyMu.Unlock()
				if event.Value%4 == 0 && !failed[event.Sequence] {
					failed[event.Sequence] = true
					return errors.New("transient failure")
				}
				flaky = append(flaky, event.Sequence)
				return nil
			}, eventbus.WithRetryDelay(quickRetry))
		if err != nil {
			t.Fatalf("subscribe flaky: %v", err)
		}

		// The counter is unordered: it must still count every event exactly
		// once.
		var counted atomic.Int64
		err = eventbus.Consume(t.Context(), broker, "counter",
			func(ctx context.Context, event fuzzEvent) error {
				counted.Add(1)
				return nil
			}, eventbus.WithUnorderedDelivery(), eventbus.WithRetryDelay(quickRetry))
		if err != nil {
			t.Fatalf("subscribe counter: %v", err)
		}

		// The rejecting consumer permanently fails every multiple of five
		// until the operator revives it, exercising park-and-continue.
		var revived atomic.Bool
		var acceptedMu sync.Mutex
		accepted := make(map[int]int)
		expectedDead := 0
		for _, value := range values {
			if value%5 == 0 {
				expectedDead++
			}
		}
		err = eventbus.Consume(t.Context(), broker, "rejecting",
			func(ctx context.Context, event fuzzEvent) error {
				if event.Value%5 == 0 && !revived.Load() {
					return errors.New("permanent failure")
				}
				acceptedMu.Lock()
				defer acceptedMu.Unlock()
				accepted[event.Sequence]++
				return nil
			}, eventbus.WithRetryDelay(quickRetry), eventbus.WithMaximumAttempts(2))
		if err != nil {
			t.Fatalf("subscribe rejecting: %v", err)
		}

		for sequence, value := range values {
			if err := broker.Publish(t.Context(), fuzzEvent{Sequence: sequence, Value: value}); err != nil {
				t.Fatalf("publish %d: %v", sequence, err)
			}
		}
		total := len(values)

		waitUntil(t, 10*time.Second, "every consumer settled", func() bool {
			recorderMu.Lock()
			recordedCount := len(recorded)
			recorderMu.Unlock()
			flakyMu.Lock()
			flakyCount := len(flaky)
			flakyMu.Unlock()
			letters, err := broker.FindExhausted(context.Background(), total+1)
			if err != nil {
				t.Fatalf("find exhausted: %v", err)
			}
			acceptedMu.Lock()
			acceptedCount := len(accepted)
			acceptedMu.Unlock()
			return recordedCount == total && flakyCount == total &&
				counted.Load() == int64(total) &&
				len(letters) == expectedDead &&
				acceptedCount == total-expectedDead
		})

		assertAscending := func(name string, log []int) {
			t.Helper()
			for index, sequence := range log {
				if sequence != index {
					t.Fatalf("%s order = %v, want strict publication order", name, log)
				}
			}
		}
		recorderMu.Lock()
		assertAscending("recorder", recorded)
		recorderMu.Unlock()
		flakyMu.Lock()
		assertAscending("flaky", flaky)
		flakyMu.Unlock()

		// Reviving every dead letter must deliver each parked event exactly
		// once and leave no dead letters behind.
		revived.Store(true)
		letters, err := broker.FindExhausted(t.Context(), total+1)
		if err != nil {
			t.Fatalf("find exhausted before revival: %v", err)
		}
		for _, letter := range letters {
			ok, err := broker.Resubmit(t.Context(), letter.Reference)
			if err != nil || !ok {
				t.Fatalf("resubmit %s = %v, %v, want true", letter.Reference, ok, err)
			}
		}
		waitUntil(t, 10*time.Second, "every revived event was accepted", func() bool {
			acceptedMu.Lock()
			defer acceptedMu.Unlock()
			return len(accepted) == total
		})
		acceptedMu.Lock()
		for sequence, count := range accepted {
			if count != 1 {
				t.Fatalf("event %d accepted %d times, want exactly 1", sequence, count)
			}
		}
		acceptedMu.Unlock()
		remaining, err := broker.FindExhausted(t.Context(), total+1)
		if err != nil {
			t.Fatalf("find exhausted after revival: %v", err)
		}
		if len(remaining) != 0 {
			t.Fatalf("dead letters after revival = %d, want 0", len(remaining))
		}

		// An unknown reference must be rejected without effect.
		ok, err := broker.Resubmit(t.Context(), fmt.Sprintf("unknown-%d", total))
		if err != nil {
			t.Fatalf("resubmit unknown: %v", err)
		}
		if ok {
			t.Fatal("resubmission of an unknown reference reported true")
		}
	})
}
