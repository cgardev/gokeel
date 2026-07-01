// Package sqlbus extends the in-process eventbus across application nodes by
// using a shared SQL database (PostgreSQL or SQLite) as the transport. It is
// the distributed sibling of the outbox package: where the outbox guarantees
// that a locally subscribed listener eventually processes an event, sqlbus
// guarantees that an event published on one node reaches listeners attached
// on any node of the cluster.
//
// An event is stored as one message row, written inside the caller's business
// transaction when a unit of work is active. Listeners attach through a
// Bridge under one of two delivery modes: competing listeners process each
// event exactly once cluster-wide (arbitrated by a guarded claim in the
// database), while broadcast listeners process each event once per node,
// which suits node-local concerns such as cache invalidation. Every node runs
// a Dispatcher that materializes delivery rows for the listeners it hosts,
// claims them, delivers through the local in-memory bus, and settles the
// outcome.
//
// Delivery is at-least-once: a crash between delivering an event and settling
// its delivery leads to a redelivery after the claim lease expires, so
// listeners must be idempotent. Polling is the correctness mechanism; wake
// signals (for example PostgreSQL LISTEN/NOTIFY wired by the caller) are
// strictly latency hints.
//
// The package is decoupled from the eventbus core: eventbus keeps its
// zero-dependency guarantee, and applications that do not need cross-node
// delivery never import sqlbus. An event type must be routed either through
// the outbox or through sqlbus, never both, or its listeners process it
// twice.
package sqlbus

import (
	"time"

	"github.com/cgardev/gokeel/eventbus"

	"github.com/google/uuid"
)

// DeliveryMode selects how the cluster shares the work of one listener.
type DeliveryMode string

const (
	// DeliveryModeCompeting delivers each event to exactly one node hosting
	// the listener, so homogeneous replicas share the work instead of
	// repeating it. It is the safe default for scaled deployments.
	DeliveryModeCompeting DeliveryMode = "COMPETING"

	// DeliveryModeBroadcast delivers each event to every node hosting the
	// listener, once per node, for node-local concerns such as invalidating
	// an in-memory cache.
	DeliveryModeBroadcast DeliveryMode = "BROADCAST"
)

// Status models the lifecycle of one delivery.
type Status string

const (
	// StatusPending marks a delivery that awaits its first claim.
	StatusPending Status = "PENDING"

	// StatusProcessing marks a delivery claimed by a dispatcher.
	StatusProcessing Status = "PROCESSING"

	// StatusCompleted marks a delivery whose listener succeeded.
	StatusCompleted Status = "COMPLETED"

	// StatusFailed marks a delivery whose listener failed and that awaits
	// its next attempt after a backoff delay.
	StatusFailed Status = "FAILED"

	// StatusExhausted marks a delivery that consumed every configured
	// attempt. It is terminal until Bridge.Resubmit gives it a fresh budget.
	StatusExhausted Status = "EXHAUSTED"
)

// NodeID identifies one application process in the cluster. It is the
// consumer instance of every broadcast delivery the node handles.
type NodeID string

// Message is one published event as stored in the shared database: the
// payload every delivery of that event refers to.
type Message struct {
	ID              uuid.UUID
	EventType       string
	SerializedEvent string
	PublisherNode   NodeID
	PublicationDate time.Time
}

// DeliveryKey identifies one delivery: the processing of one message by one
// consumer of one listener. Instance is empty for a competing listener,
// whose single cluster-wide consumer is the listener itself, and holds the
// NodeID of the consuming node for a broadcast listener.
type DeliveryKey struct {
	MessageID  uuid.UUID
	ListenerID eventbus.ListenerID
	Instance   string
}

// Delivery is one delivery row: the unit the claim and settlement state
// machine runs on.
type Delivery struct {
	Key             DeliveryKey
	Status          Status
	Attempts        int
	ClaimToken      string
	ClaimDate       *time.Time
	NextAttemptDate *time.Time
	CompletionDate  *time.Time
	LastError       string
}

// DueDelivery pairs a claimable delivery with the message it delivers, so a
// dispatcher restores the event without a second read.
type DueDelivery struct {
	Delivery Delivery
	Message  Message
}

// Consumer is one durable registration: the declaration that a listener,
// under a delivery mode, consumes one event type starting at a boundary.
// Instance follows the DeliveryKey convention. Frontier is the advancing
// publication-date floor below which every visible message already has its
// delivery row, which bounds the materialization scan; StartBoundary is the
// fixed registration-time floor used to decide whether the consumer covers a
// message at all.
type Consumer struct {
	ListenerID       eventbus.ListenerID
	Instance         string
	EventType        string
	DeliveryMode     DeliveryMode
	StartBoundary    time.Time
	Frontier         time.Time
	RegistrationDate time.Time
	HeartbeatDate    time.Time
}

// ConsumerKey identifies one durable consumer registration.
type ConsumerKey struct {
	ListenerID eventbus.ListenerID
	Instance   string
	EventType  string
}

// Key returns the identifying part of the consumer.
func (c Consumer) Key() ConsumerKey {
	return ConsumerKey{ListenerID: c.ListenerID, Instance: c.Instance, EventType: c.EventType}
}
