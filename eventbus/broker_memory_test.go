package eventbus_test

import (
	"context"
	"errors"
	"testing"
	"time"

	"github.com/cgardev/gokeel/eventbus"
	"github.com/cgardev/gokeel/eventbus/bustest"
)

// memoryHarness adapts the in-memory engine to the conformance suite.
type memoryHarness struct{}

func (memoryHarness) NewBroker(t *testing.T) eventbus.Broker {
	t.Helper()
	broker := eventbus.NewMemoryBroker()
	t.Cleanup(broker.Stop)
	return broker
}

func (memoryHarness) SettleWithin() time.Duration { return 3 * time.Second }

func TestMemoryBrokerConformance(t *testing.T) {
	bustest.Run(t, memoryHarness{})
}

func TestMemoryBrokerParksAPanickingHandlerAsADeadLetter(t *testing.T) {
	broker := eventbus.NewMemoryBroker()
	t.Cleanup(broker.Stop)

	err := eventbus.Consume(t.Context(), broker, "panicking",
		func(ctx context.Context, event bustest.Event) error {
			panic("handler bug")
		},
		eventbus.WithMaximumAttempts(2),
		eventbus.WithRetryDelay(func(int) time.Duration { return time.Millisecond }))
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := broker.Publish(t.Context(), bustest.Event{Value: 1}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	deadline := time.Now().Add(3 * time.Second)
	for {
		letters, err := broker.FindExhausted(t.Context(), 1)
		if err != nil {
			t.Fatalf("find exhausted: %v", err)
		}
		if len(letters) == 1 {
			if letters[0].Attempts != 2 || letters[0].LastError == "" {
				t.Fatalf("dead letter = %+v, want two attempts and a recorded cause", letters[0])
			}
			break
		}
		if time.Now().After(deadline) {
			t.Fatal("panicking handler did not park as a dead letter")
		}
		time.Sleep(5 * time.Millisecond)
	}
}

func TestMemoryBrokerRejectsOperationsAfterStop(t *testing.T) {
	broker := eventbus.NewMemoryBroker()
	broker.Stop()

	if err := broker.Publish(t.Context(), bustest.Event{Value: 1}); !errors.Is(err, eventbus.ErrBrokerStopped) {
		t.Fatalf("publish after stop error = %v, want ErrBrokerStopped", err)
	}
	err := eventbus.Consume(t.Context(), broker, "late",
		func(ctx context.Context, event bustest.Event) error { return nil })
	if !errors.Is(err, eventbus.ErrBrokerStopped) {
		t.Fatalf("subscribe after stop error = %v, want ErrBrokerStopped", err)
	}
}

func TestMemoryBrokerStopWaitsForTheInFlightDelivery(t *testing.T) {
	broker := eventbus.NewMemoryBroker()

	started := make(chan struct{})
	finished := make(chan struct{})
	err := eventbus.Consume(t.Context(), broker, "slow",
		func(ctx context.Context, event bustest.Event) error {
			close(started)
			time.Sleep(50 * time.Millisecond)
			close(finished)
			return nil
		})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}
	if err := broker.Publish(t.Context(), bustest.Event{Value: 1}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	<-started
	broker.Stop()
	select {
	case <-finished:
	default:
		t.Fatal("Stop returned while a delivery was still in flight")
	}
}

func TestMemoryBrokerRejectsADuplicateConsumerIdentifier(t *testing.T) {
	broker := eventbus.NewMemoryBroker()
	t.Cleanup(broker.Stop)

	handle := func(ctx context.Context, event bustest.Event) error { return nil }
	if err := eventbus.Consume(t.Context(), broker, "duplicated", handle); err != nil {
		t.Fatalf("first subscription: %v", err)
	}
	err := eventbus.Consume(t.Context(), broker, "duplicated", handle)
	if !errors.Is(err, eventbus.ErrDuplicateListener) {
		t.Fatalf("duplicate subscription error = %v, want ErrDuplicateListener", err)
	}
}
