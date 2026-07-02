package outbox

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/cgardev/gokeel/eventbus"

	"github.com/google/uuid"
)

// storedPublication persists one publication directly through the store, so
// store-level tests can interleave claims, settlements, and resubmissions the
// way concurrent application instances would.
func storedPublication(t *testing.T, f *fixture, orderID string, publicationDate time.Time) Publication {
	t.Helper()
	publication := Publication{
		ID:                   uuid.New(),
		ListenerID:           "billing",
		EventType:            "order.placed",
		SerializedEvent:      `{"OrderID":"` + orderID + `"}`,
		PublicationDate:      publicationDate,
		Status:               StatusPublished,
		CompletionAttempts:   1,
		LastResubmissionDate: &publicationDate,
	}
	if err := f.store.Create(t.Context(), f.database, publication); err != nil {
		t.Fatalf("create publication: %v", err)
	}
	return publication
}

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

func TestLateSettlementOfALostClaimIsFencedOut(t *testing.T) {
	f := newFixture(t, CompletionModeUpdate)
	publication := storedPublication(t, f, "o-fenced", time.Now().UTC())

	// The first dispatcher claims the initial attempt and stalls mid-delivery.
	claimed, err := f.store.ClaimProcessing(t.Context(), publication.ID, 1)
	if err != nil || !claimed {
		t.Fatalf("first claim = %v, %v, want true", claimed, err)
	}

	// A resubmitter deems the attempt stale and hands the publication to a
	// second dispatcher under the next generation.
	attempts, resubmitted, err := f.store.MarkResubmitted(t.Context(), publication.ID, 1, time.Now().UTC())
	if err != nil || !resubmitted {
		t.Fatalf("resubmission of the stalled attempt = %v, %v, want true", resubmitted, err)
	}
	if attempts != 2 {
		t.Fatalf("attempts after resubmission = %d, want 2", attempts)
	}
	claimed, err = f.store.ClaimProcessing(t.Context(), publication.ID, 2)
	if err != nil || !claimed {
		t.Fatalf("second claim = %v, %v, want true", claimed, err)
	}

	// The stalled dispatcher wakes up: both of its settlements are fenced out
	// by the generation guard and must not disturb the current holder.
	settled, err := f.store.MarkCompleted(t.Context(), publication.ID, 1, time.Now().UTC())
	if err != nil {
		t.Fatalf("late completion: %v", err)
	}
	if settled {
		t.Error("late completion of a lost claim settled the publication")
	}
	settled, err = f.store.MarkFailed(t.Context(), publication.ID, 1)
	if err != nil {
		t.Fatalf("late failure mark: %v", err)
	}
	if settled {
		t.Error("late failure mark of a lost claim settled the publication")
	}

	// The current holder settles normally.
	settled, err = f.store.MarkCompleted(t.Context(), publication.ID, 2, time.Now().UTC())
	if err != nil || !settled {
		t.Fatalf("completion by the current holder = %v, %v, want true", settled, err)
	}
	incomplete, err := f.store.FindIncomplete(t.Context())
	if err != nil {
		t.Fatalf("find incomplete: %v", err)
	}
	if len(incomplete) != 0 {
		t.Errorf("incomplete publications = %d, want 0", len(incomplete))
	}
}

func TestConcurrentClaimsAdmitExactlyOneWinner(t *testing.T) {
	f := newFixture(t, CompletionModeUpdate)
	publication := storedPublication(t, f, "o-contended", time.Now().UTC())

	const claimers = 8
	var winners atomic.Int64
	var group sync.WaitGroup
	failures := make(chan error, claimers)
	for range claimers {
		group.Add(1)
		go func() {
			defer group.Done()
			claimed, err := f.store.ClaimProcessing(context.Background(), publication.ID, 1)
			if err != nil {
				failures <- err
				return
			}
			if claimed {
				winners.Add(1)
			}
		}()
	}
	group.Wait()
	close(failures)
	for err := range failures {
		t.Fatalf("concurrent claim: %v", err)
	}

	if got := winners.Load(); got != 1 {
		t.Errorf("claim winners = %d, want exactly 1", got)
	}
}

func TestFailedPublicationReentersDeliveryOnlyThroughResubmission(t *testing.T) {
	f := newFixture(t, CompletionModeUpdate)
	publication := storedPublication(t, f, "o-failed", time.Now().UTC())

	claimed, err := f.store.ClaimProcessing(t.Context(), publication.ID, 1)
	if err != nil || !claimed {
		t.Fatalf("claim = %v, %v, want true", claimed, err)
	}
	settled, err := f.store.MarkFailed(t.Context(), publication.ID, 1)
	if err != nil || !settled {
		t.Fatalf("failure mark = %v, %v, want true", settled, err)
	}

	// The failed state is terminal for dispatchers: no claim succeeds until a
	// compare-and-set resubmission re-opens the publication.
	claimed, err = f.store.ClaimProcessing(t.Context(), publication.ID, 1)
	if err != nil {
		t.Fatalf("claim of the failed publication: %v", err)
	}
	if claimed {
		t.Error("failed publication was claimable without a resubmission")
	}

	attempts, resubmitted, err := f.store.MarkResubmitted(t.Context(), publication.ID, 1, time.Now().UTC())
	if err != nil || !resubmitted {
		t.Fatalf("resubmission = %v, %v, want true", resubmitted, err)
	}
	claimed, err = f.store.ClaimProcessing(t.Context(), publication.ID, attempts)
	if err != nil || !claimed {
		t.Fatalf("claim after resubmission = %v, %v, want true", claimed, err)
	}
}

func TestAbandonedResubmittedPublicationIsRecoveredByTheResubmitter(t *testing.T) {
	f := newFixture(t, CompletionModeUpdate)
	var received []orderPlaced
	subscribeRecorder(t, f.bus, "billing", &received)
	publication := storedPublication(t, f, "o-stranded", time.Now().UTC())

	// A dispatcher resubmitted the publication and crashed before claiming it,
	// leaving the row stranded in the resubmitted state.
	_, resubmitted, err := f.store.MarkResubmitted(t.Context(), publication.ID, 1, time.Now().UTC())
	if err != nil || !resubmitted {
		t.Fatalf("resubmission = %v, %v, want true", resubmitted, err)
	}

	if err := f.registry.ResubmitIncomplete(t.Context(), 0); err != nil {
		t.Fatalf("resubmit incomplete: %v", err)
	}

	if len(received) != 1 || received[0].OrderID != "o-stranded" {
		t.Fatalf("recovered deliveries = %+v, want the stranded order", received)
	}
	incomplete, err := f.store.FindIncomplete(t.Context())
	if err != nil {
		t.Fatalf("find incomplete: %v", err)
	}
	if len(incomplete) != 0 {
		t.Errorf("incomplete publications = %d, want 0", len(incomplete))
	}
}

func TestResubmissionGraceProtectsTheLatestAttemptNotTheFirstDispatch(t *testing.T) {
	f := newFixture(t, CompletionModeUpdate)
	anHourAgo := time.Now().UTC().Add(-time.Hour)
	publication := storedPublication(t, f, "o-aged", anHourAgo)

	// A fresh resubmission starts a new attempt now, even though the
	// publication itself is an hour old.
	_, resubmitted, err := f.store.MarkResubmitted(t.Context(), publication.ID, 1, time.Now().UTC())
	if err != nil || !resubmitted {
		t.Fatalf("resubmission = %v, %v, want true", resubmitted, err)
	}

	aMinuteAgo := time.Now().UTC().Add(-time.Minute)
	aged, err := f.store.FindIncompletePublishedBefore(t.Context(), aMinuteAgo)
	if err != nil {
		t.Fatalf("find incomplete before: %v", err)
	}
	if len(aged) != 0 {
		t.Errorf("publications considered stale = %d, want 0: the in-flight attempt is fresh", len(aged))
	}

	inAMinute := time.Now().UTC().Add(time.Minute)
	due, err := f.store.FindIncompletePublishedBefore(t.Context(), inAMinute)
	if err != nil {
		t.Fatalf("find incomplete before: %v", err)
	}
	if len(due) != 1 {
		t.Errorf("publications past the grace = %d, want 1", len(due))
	}
}

func TestOverlappingResubmissionPassesCannotFenceAFreshAttempt(t *testing.T) {
	f := newFixture(t, CompletionModeUpdate)
	publication := storedPublication(t, f, "o-toctou", time.Now().UTC())

	claimed, err := f.store.ClaimProcessing(t.Context(), publication.ID, 1)
	if err != nil || !claimed {
		t.Fatalf("claim = %v, %v, want true", claimed, err)
	}
	settled, err := f.store.MarkFailed(t.Context(), publication.ID, 1)
	if err != nil || !settled {
		t.Fatalf("failure mark = %v, %v, want true", settled, err)
	}

	// Two resubmitter passes on different instances both observed the failed
	// publication at generation one. The first resubmits and starts delivering.
	attempts, resubmitted, err := f.store.MarkResubmitted(t.Context(), publication.ID, 1, time.Now().UTC())
	if err != nil || !resubmitted || attempts != 2 {
		t.Fatalf("first resubmission = %d, %v, %v, want 2, true", attempts, resubmitted, err)
	}
	claimed, err = f.store.ClaimProcessing(t.Context(), publication.ID, 2)
	if err != nil || !claimed {
		t.Fatalf("claim after resubmission = %v, %v, want true", claimed, err)
	}

	// The second pass still holds the stale observation: its resubmission must
	// fail instead of fencing the delivery that just started.
	_, resubmitted, err = f.store.MarkResubmitted(t.Context(), publication.ID, 1, time.Now().UTC())
	if err != nil {
		t.Fatalf("overlapping resubmission: %v", err)
	}
	if resubmitted {
		t.Error("a resubmission based on a stale observation fenced a fresh attempt")
	}

	settled, err = f.store.MarkCompleted(t.Context(), publication.ID, 2, time.Now().UTC())
	if err != nil || !settled {
		t.Fatalf("completion of the fresh attempt = %v, %v, want true", settled, err)
	}
}

func TestRecoveredAbandonedAttemptIsProtectedByTheGraceWindow(t *testing.T) {
	f := newFixture(t, CompletionModeUpdate)
	anHourAgo := time.Now().UTC().Add(-time.Hour)
	publication := storedPublication(t, f, "o-recovered", anHourAgo)

	// A dispatcher resubmitted the publication an hour ago and crashed before
	// claiming it, so the abandoned attempt carries a stale date.
	_, resubmitted, err := f.store.MarkResubmitted(t.Context(), publication.ID, 1, anHourAgo)
	if err != nil || !resubmitted {
		t.Fatalf("resubmission = %v, %v, want true", resubmitted, err)
	}

	// The recovering dispatcher claims it now: the claim stamps the attempt
	// start, so the recovery is protected by the grace window instead of being
	// immediately eligible for another steal.
	claimed, err := f.store.ClaimProcessing(t.Context(), publication.ID, 2)
	if err != nil || !claimed {
		t.Fatalf("recovery claim = %v, %v, want true", claimed, err)
	}
	aged, err := f.store.FindIncompletePublishedBefore(t.Context(), time.Now().UTC().Add(-time.Minute))
	if err != nil {
		t.Fatalf("find incomplete before: %v", err)
	}
	if len(aged) != 0 {
		t.Errorf("publications considered stale = %d, want 0: the recovery attempt just started", len(aged))
	}
}

func TestFencedArchiveLeavesNoEarlyArchiveCopy(t *testing.T) {
	f := newFixture(t, CompletionModeArchive)
	publication := storedPublication(t, f, "o-archive-fenced", time.Now().UTC())

	claimed, err := f.store.ClaimProcessing(t.Context(), publication.ID, 1)
	if err != nil || !claimed {
		t.Fatalf("claim = %v, %v, want true", claimed, err)
	}
	// A resubmission fences the dispatcher while its listener is finishing.
	_, resubmitted, err := f.store.MarkResubmitted(t.Context(), publication.ID, 1, time.Now().UTC())
	if err != nil || !resubmitted {
		t.Fatalf("resubmission = %v, %v, want true", resubmitted, err)
	}

	settled, err := f.store.MarkCompleted(t.Context(), publication.ID, 1, time.Now().UTC())
	if err != nil {
		t.Fatalf("fenced archive completion: %v", err)
	}
	if settled {
		t.Error("fenced archive completion settled the publication")
	}
	if got := countRows(t, f.database, archiveTableName); got != 0 {
		t.Errorf("archive rows after a fenced completion = %d, want 0", got)
	}
	if got := countRows(t, f.database, tableName); got != 1 {
		t.Errorf("source rows after a fenced completion = %d, want 1", got)
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
