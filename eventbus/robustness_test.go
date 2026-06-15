package eventbus

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
)

func TestDeliverRecoversAPanickingListener(t *testing.T) {
	bus := NewBus()
	err := SubscribeTo(bus, "billing", func(ctx context.Context, event orderPlaced) error {
		panic("listener bug")
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	if err := bus.Deliver(t.Context(), "billing", orderPlaced{}); !errors.Is(err, ErrListenerPanic) {
		t.Fatalf("deliver to panicking listener error = %v, want ErrListenerPanic", err)
	}
}

func TestPublishContinuesPastAPanickingListener(t *testing.T) {
	bus := NewBus()
	var received []orderPlaced

	err := SubscribeTo(bus, "billing", func(ctx context.Context, event orderPlaced) error {
		panic("listener bug")
	})
	if err != nil {
		t.Fatalf("subscribe billing: %v", err)
	}
	err = SubscribeTo(bus, "shipping", func(ctx context.Context, event orderPlaced) error {
		received = append(received, event)
		return nil
	})
	if err != nil {
		t.Fatalf("subscribe shipping: %v", err)
	}

	if err := bus.Publish(t.Context(), orderPlaced{OrderID: "o-1"}); !errors.Is(err, ErrListenerPanic) {
		t.Fatalf("publish error = %v, want ErrListenerPanic", err)
	}
	if len(received) != 1 {
		t.Errorf("deliveries past the panicking listener = %d, want 1", len(received))
	}
}

func TestSubscribeValidatesItsArguments(t *testing.T) {
	bus := NewBus()
	matches := func(event any) bool { return true }
	handle := func(ctx context.Context, event any) error { return nil }

	if err := bus.Subscribe("", matches, handle); err == nil {
		t.Error("empty listener identifier accepted")
	}
	if err := bus.Subscribe("billing", nil, handle); err == nil {
		t.Error("nil matches predicate accepted")
	}
	if err := bus.Subscribe("billing", matches, nil); err == nil {
		t.Error("nil handler accepted")
	}
}

func TestConcurrentSubscribeAndPublish(t *testing.T) {
	bus := NewBus()
	var delivered atomic.Int64

	const workers = 8
	var group sync.WaitGroup
	for worker := range workers {
		group.Add(1)
		go func() {
			defer group.Done()
			id := ListenerID(fmt.Sprintf("listener-%d", worker))
			err := SubscribeTo(bus, id, func(ctx context.Context, event orderPlaced) error {
				delivered.Add(1)
				return nil
			})
			if err != nil {
				t.Errorf("subscribe %s: %v", id, err)
				return
			}
			if err := bus.Publish(context.Background(), orderPlaced{OrderID: "o-1"}); err != nil {
				t.Errorf("publish from %s: %v", id, err)
			}
		}()
	}
	group.Wait()

	// Every worker publishes once after subscribing itself, so its own
	// listener receives the event; concurrent subscriptions may add more.
	if got := delivered.Load(); got < workers {
		t.Errorf("deliveries = %d, want at least %d", got, workers)
	}

	if err := bus.Publish(t.Context(), orderPlaced{OrderID: "final"}); err != nil {
		t.Fatalf("final publish: %v", err)
	}
	if got := len(bus.ListenersFor(orderPlaced{})); got != workers {
		t.Errorf("subscribed listeners = %d, want %d", got, workers)
	}
}
