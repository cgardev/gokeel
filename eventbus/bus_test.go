package eventbus

import (
	"context"
	"errors"
	"testing"
)

type orderPlaced struct {
	OrderID string
}

type orderCancelled struct {
	OrderID string
}

func TestPublishMulticastsToEveryMatchingListener(t *testing.T) {
	bus := NewBus()
	var placed []orderPlaced
	var cancelled []orderCancelled

	err := SubscribeTo(bus, "billing", func(ctx context.Context, event orderPlaced) error {
		placed = append(placed, event)
		return nil
	})
	if err != nil {
		t.Fatalf("subscribe billing: %v", err)
	}
	err = SubscribeTo(bus, "shipping", func(ctx context.Context, event orderCancelled) error {
		cancelled = append(cancelled, event)
		return nil
	})
	if err != nil {
		t.Fatalf("subscribe shipping: %v", err)
	}

	if err := bus.Publish(t.Context(), orderPlaced{OrderID: "o-1"}); err != nil {
		t.Fatalf("publish: %v", err)
	}

	if len(placed) != 1 || placed[0].OrderID != "o-1" {
		t.Errorf("matching listener received %+v, want order o-1", placed)
	}
	if len(cancelled) != 0 {
		t.Errorf("non-matching listener received %+v", cancelled)
	}
}

func TestPublishInvokesRemainingListenersWhenOneFails(t *testing.T) {
	bus := NewBus()
	listenerFailure := errors.New("listener failure")
	var received []orderPlaced

	err := SubscribeTo(bus, "billing", func(ctx context.Context, event orderPlaced) error {
		return listenerFailure
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

	if err := bus.Publish(t.Context(), orderPlaced{OrderID: "o-1"}); !errors.Is(err, listenerFailure) {
		t.Fatalf("publish error = %v, want the listener failure", err)
	}
	if len(received) != 1 {
		t.Errorf("deliveries to the remaining listener = %d, want 1", len(received))
	}
}

func TestSubscribeRejectsADuplicateListenerIdentifier(t *testing.T) {
	bus := NewBus()
	handle := func(ctx context.Context, event orderPlaced) error { return nil }

	if err := SubscribeTo(bus, "billing", handle); err != nil {
		t.Fatalf("first subscription: %v", err)
	}
	if err := SubscribeTo(bus, "billing", handle); !errors.Is(err, ErrDuplicateListener) {
		t.Fatalf("duplicate subscription error = %v, want ErrDuplicateListener", err)
	}
}

func TestDeliverReportsAnUnknownListener(t *testing.T) {
	bus := NewBus()

	if err := bus.Deliver(t.Context(), "missing", orderPlaced{}); !errors.Is(err, ErrUnknownListener) {
		t.Fatalf("deliver to unknown listener error = %v, want ErrUnknownListener", err)
	}
}

func TestListenersForReturnsIdentifiersInSubscriptionOrder(t *testing.T) {
	bus := NewBus()
	handle := func(ctx context.Context, event orderPlaced) error { return nil }

	for _, id := range []ListenerID{"billing", "shipping", "analytics"} {
		if err := SubscribeTo(bus, id, handle); err != nil {
			t.Fatalf("subscribe %s: %v", id, err)
		}
	}

	listeners := bus.ListenersFor(orderPlaced{})
	want := []ListenerID{"billing", "shipping", "analytics"}
	if len(listeners) != len(want) {
		t.Fatalf("listeners = %v, want %v", listeners, want)
	}
	for index, id := range want {
		if listeners[index] != id {
			t.Fatalf("listeners = %v, want %v", listeners, want)
		}
	}
}
