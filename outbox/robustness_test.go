package outbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"

	"github.com/cgardev/gokeel/eventbus"
)

func TestConcurrentPublishersDeliverEveryEvent(t *testing.T) {
	f := newFixture(t, CompletionModeUpdate)
	var delivered atomic.Int64
	err := eventbus.SubscribeTo(f.bus, "billing", func(ctx context.Context, event orderPlaced) error {
		delivered.Add(1)
		return nil
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	const publishers = 8
	const eventsPerPublisher = 5
	var group sync.WaitGroup
	failures := make(chan error, publishers)
	for publisher := range publishers {
		group.Add(1)
		go func() {
			defer group.Done()
			for event := range eventsPerPublisher {
				err := f.publish(context.Background(),
					orderPlaced{OrderID: fmt.Sprintf("o-%d-%d", publisher, event)})
				if err != nil {
					failures <- err
					return
				}
			}
		}()
	}
	group.Wait()
	close(failures)
	for err := range failures {
		t.Fatalf("concurrent publisher: %v", err)
	}

	if got := delivered.Load(); got != publishers*eventsPerPublisher {
		t.Errorf("deliveries = %d, want %d", got, publishers*eventsPerPublisher)
	}
	incomplete, err := f.store.FindIncomplete(t.Context())
	if err != nil {
		t.Fatalf("find incomplete: %v", err)
	}
	if len(incomplete) != 0 {
		t.Errorf("incomplete publications = %d, want 0", len(incomplete))
	}
}

func TestConcurrentResubmissionsConvergeWithoutLosingTheEvent(t *testing.T) {
	f := newFixture(t, CompletionModeUpdate)
	var deliveries atomic.Int64
	failFirst := true
	err := eventbus.SubscribeTo(f.bus, "billing", func(ctx context.Context, event orderPlaced) error {
		if failFirst {
			failFirst = false
			return errors.New("first delivery fails")
		}
		deliveries.Add(1)
		return nil
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	err = f.publish(t.Context(), orderPlaced{OrderID: "o-1"})
	if err != nil {
		t.Fatalf("within transaction: %v", err)
	}

	const resubmitters = 8
	var group sync.WaitGroup
	for range resubmitters {
		group.Add(1)
		go func() {
			defer group.Done()
			// Errors are tolerated here: a resubmitter may race another one,
			// and convergence is asserted on the final state below.
			_ = f.registry.ResubmitIncomplete(context.Background(), 0)
		}()
	}
	group.Wait()

	if got := deliveries.Load(); got < 1 {
		t.Errorf("successful deliveries = %d, want at least 1", got)
	}
	incomplete, err := f.store.FindIncomplete(t.Context())
	if err != nil {
		t.Fatalf("find incomplete: %v", err)
	}
	if len(incomplete) != 0 {
		t.Errorf("incomplete publications after convergence = %d, want 0", len(incomplete))
	}
}

func TestPanickingListenerLeavesThePublicationRecoverable(t *testing.T) {
	f := newFixture(t, CompletionModeUpdate)
	var delivered atomic.Int64
	panicking := true
	err := eventbus.SubscribeTo(f.bus, "billing", func(ctx context.Context, event orderPlaced) error {
		if panicking {
			panic("listener bug")
		}
		delivered.Add(1)
		return nil
	})
	if err != nil {
		t.Fatalf("subscribe: %v", err)
	}

	err = f.publish(t.Context(), orderPlaced{OrderID: "o-1"})
	if err != nil {
		t.Fatalf("a panicking listener must not fail the publishing call: %v", err)
	}

	incomplete, err := f.store.FindIncomplete(t.Context())
	if err != nil {
		t.Fatalf("find incomplete: %v", err)
	}
	if len(incomplete) != 1 || incomplete[0].Status != StatusFailed {
		t.Fatalf("publications after panic = %+v, want one failed entry", incomplete)
	}

	panicking = false
	if err := f.registry.ResubmitIncomplete(t.Context(), 0); err != nil {
		t.Fatalf("resubmit incomplete: %v", err)
	}
	if got := delivered.Load(); got != 1 {
		t.Errorf("deliveries after recovery = %d, want 1", got)
	}
}

func TestRegisterEventTypeRejectsConflictingRegistrations(t *testing.T) {
	serializer := NewJSONSerializer()
	if err := RegisterEventType[orderPlaced](serializer, "order.placed"); err != nil {
		t.Fatalf("first registration: %v", err)
	}

	if err := RegisterEventType[orderPlaced](serializer, "order.placed"); err != nil {
		t.Errorf("idempotent re-registration rejected: %v", err)
	}

	err := RegisterEventType[orderPlaced](serializer, "order.renamed")
	if !errors.Is(err, ErrConflictingRegistration) {
		t.Errorf("re-binding a type error = %v, want ErrConflictingRegistration", err)
	}

	type otherEvent struct{}
	err = RegisterEventType[otherEvent](serializer, "order.placed")
	if !errors.Is(err, ErrConflictingRegistration) {
		t.Errorf("re-binding a name error = %v, want ErrConflictingRegistration", err)
	}

	if err := RegisterEventType[otherEvent](serializer, ""); err == nil {
		t.Error("empty event type name accepted")
	}
}
