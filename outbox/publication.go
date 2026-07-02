// Package outbox implements the transactional outbox pattern for domain
// events: publications are stored in the same database transaction as the
// business change, then delivered through the in-memory eventbus bus to their
// subscribed listeners, and their outcome is settled back in the store.
//
// The package decorates the generic eventbus library and carries only the
// persistence concern, mirroring how the Spring Modulith event publication
// registry decorates the application event infrastructure of the framework.
package outbox

import (
	"time"

	"github.com/cgardev/gokeel/eventbus"

	"github.com/google/uuid"
)

// Status models the lifecycle of an event publication.
type Status string

const (
	// StatusPublished marks a publication that has been stored initially.
	StatusPublished Status = "PUBLISHED"

	// StatusProcessing marks a publication picked up by its listener.
	StatusProcessing Status = "PROCESSING"

	// StatusCompleted marks a publication processed successfully.
	StatusCompleted Status = "COMPLETED"

	// StatusFailed marks a publication whose processing failed.
	StatusFailed Status = "FAILED"

	// StatusResubmitted marks a previously failed publication that has been
	// resubmitted for processing.
	StatusResubmitted Status = "RESUBMITTED"
)

// Publication is one outbox entry: the publication of one event to one
// target listener.
type Publication struct {
	ID              uuid.UUID
	ListenerID      eventbus.ListenerID
	EventType       string
	SerializedEvent string

	// Event holds the in-memory event instance. It is populated when the
	// publication is created or when the serialized event is deserialized
	// for resubmission; it is never persisted as such.
	Event any

	PublicationDate time.Time
	CompletionDate  *time.Time
	Status          Status

	// CompletionAttempts counts the delivery attempts of the publication,
	// starting at one for the initial dispatch and incremented on every
	// resubmission. It doubles as the fencing generation: a dispatcher settles
	// its outcome only while the counter still holds the value under which it
	// claimed the publication.
	CompletionAttempts int

	// LastResubmissionDate records when the latest delivery attempt started.
	// It is seeded with the publication date and overwritten on every
	// resubmission, so the resubmitter measures its grace window against the
	// attempt in flight, not against the original publication.
	LastResubmissionDate *time.Time
}
