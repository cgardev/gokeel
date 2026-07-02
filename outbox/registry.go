package outbox

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
	"time"

	"github.com/cgardev/gokeel/eventbus"

	"github.com/google/uuid"
)

// EventBus is the slice of the in-memory bus the registry relies on. It is
// satisfied by *eventbus.Bus.
type EventBus interface {
	ListenersFor(event any) []eventbus.ListenerID
	Deliver(ctx context.Context, id eventbus.ListenerID, event any) error
}

// Registry coordinates the outbox pattern: it stores one publication per
// subscribed listener through the querier of the caller, so the publications
// join the transaction of the business change, and delivers each publication
// through the bus. It does not own the transaction; the Publisher writes the
// publications inside a unit of work and defers their delivery to after the
// commit.
//
// Publications follow the Spring Modulith lifecycle: created as published
// with one completion attempt counted, claimed into processing, and settled
// as completed or failed; a failed publication re-enters delivery only
// through the compare-and-set resubmission, which increments the attempt
// counter. Every transition is additionally fenced by the attempt generation,
// so concurrent registries on separate application instances never overwrite
// each other's outcomes.
//
// A Registry is immutable after construction and safe for concurrent use.
// Delivery is at-least-once: a crash or failure between delivering an event
// and settling its publication leads to a redelivery on resubmission, so
// listeners must be idempotent.
type Registry struct {
	store      Store
	bus        EventBus
	serializer Serializer
}

// NewRegistry constructs a Registry on top of the given collaborators.
func NewRegistry(store Store, bus EventBus, serializer Serializer) *Registry {
	return &Registry{store: store, bus: bus, serializer: serializer}
}

// Publish stores one publication of the event for every subscribed listener,
// writing through the provided querier so the publications join the
// transaction of the business change. The returned publications are pending:
// they must be handed to Dispatch after the transaction commits.
func (r *Registry) Publish(ctx context.Context, querier Querier, event any) ([]Publication, error) {
	listeners := r.bus.ListenersFor(event)
	if len(listeners) == 0 {
		return nil, nil
	}

	eventType, payload, err := r.serializer.Serialize(event)
	if err != nil {
		return nil, err
	}

	now := time.Now().UTC()
	publications := make([]Publication, 0, len(listeners))
	for _, listener := range listeners {
		id, err := uuid.NewRandom()
		if err != nil {
			return nil, fmt.Errorf("generate publication identifier: %w", err)
		}
		// The initial dispatch is the first completion attempt and the last
		// resubmission date starts at the publication date, mirroring how the
		// Spring Modulith repository seeds a new publication row.
		publication := Publication{
			ID:                   id,
			ListenerID:           listener,
			EventType:            eventType,
			SerializedEvent:      payload,
			Event:                event,
			PublicationDate:      now,
			Status:               StatusPublished,
			CompletionAttempts:   1,
			LastResubmissionDate: &now,
		}
		if err := r.store.Create(ctx, querier, publication); err != nil {
			return nil, err
		}
		publications = append(publications, publication)
	}
	return publications, nil
}

// Dispatch delivers each publication to its target listener and settles the
// outcome in the store: completed on success, failed otherwise. A failed
// publication stays incomplete and is recovered through ResubmitIncomplete.
// The returned error joins the failures of every undelivered publication.
func (r *Registry) Dispatch(ctx context.Context, publications ...Publication) error {
	var failures []error
	for _, publication := range publications {
		if err := r.dispatch(ctx, publication); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (r *Registry) dispatch(ctx context.Context, publication Publication) error {
	claimed, err := r.store.ClaimProcessing(ctx, publication.ID, publication.CompletionAttempts)
	if err != nil {
		return err
	}
	if !claimed {
		// Another dispatcher holds or already settled this publication.
		return nil
	}

	if err := r.bus.Deliver(ctx, publication.ListenerID, publication.Event); err != nil {
		deliveryError := fmt.Errorf(
			"deliver %s to %s: %w", publication.EventType, publication.ListenerID, err,
		)
		settled, markError := r.store.MarkFailed(ctx, publication.ID, publication.CompletionAttempts)
		if markError != nil {
			return errors.Join(deliveryError, markError)
		}
		if !settled {
			r.reportFencedSettlement(publication)
		}
		return deliveryError
	}

	settled, err := r.store.MarkCompleted(
		ctx, publication.ID, publication.CompletionAttempts, time.Now().UTC(),
	)
	if err != nil {
		return err
	}
	if !settled {
		r.reportFencedSettlement(publication)
	}
	return nil
}

// reportFencedSettlement surfaces a settlement that affected zero rows: the
// publication was resubmitted while this dispatcher was delivering it, so the
// listener's work was, or will be, repeated by the new holder. A recurring
// report means the resubmission grace is shorter than the slowest listener.
func (r *Registry) reportFencedSettlement(publication Publication) {
	slog.Warn("publication settlement was fenced out by a concurrent resubmission",
		"publication", publication.ID, "listener", publication.ListenerID)
}

// ResubmitIncomplete re-delivers every incomplete publication, restoring the
// event from its serialized representation. When olderThan is positive, only
// publications whose latest delivery attempt started before that age are
// considered, which avoids racing against dispatches that are still in
// flight. The returned error joins the failures of every publication that
// could not be re-delivered.
func (r *Registry) ResubmitIncomplete(ctx context.Context, olderThan time.Duration) error {
	var publications []Publication
	var err error
	if olderThan > 0 {
		reference := time.Now().UTC().Add(-olderThan)
		publications, err = r.store.FindIncompletePublishedBefore(ctx, reference)
	} else {
		publications, err = r.store.FindIncomplete(ctx)
	}
	if err != nil {
		return err
	}

	var failures []error
	for _, publication := range publications {
		if err := r.resubmit(ctx, publication); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

func (r *Registry) resubmit(ctx context.Context, publication Publication) error {
	event, err := r.serializer.Deserialize(publication.EventType, publication.SerializedEvent)
	if err != nil {
		return err
	}
	publication.Event = event

	// A publication read in the resubmitted state was abandoned by a
	// dispatcher that crashed between resubmitting and claiming it, so it is
	// dispatched directly under the generation it already carries. A live
	// claimer racing this dispatch is harmless: the claim admits one winner.
	if publication.Status == StatusResubmitted {
		return r.dispatch(ctx, publication)
	}

	// The resubmission carries the attempts value this pass observed as
	// stale, so it cannot fence a fresher attempt that a peer started after
	// the observation.
	attempts, resubmitted, err := r.store.MarkResubmitted(
		ctx, publication.ID, publication.CompletionAttempts, time.Now().UTC(),
	)
	if err != nil {
		return err
	}
	if !resubmitted {
		return nil
	}

	publication.CompletionAttempts = attempts
	return r.dispatch(ctx, publication)
}
