package sqlbus

import (
	"context"
	"database/sql"
	"errors"
	"time"

	"github.com/cgardev/gokeel/eventbus"
)

var (
	// ErrConflictingDeliveryMode reports an attachment whose delivery mode
	// disagrees with the mode another node already registered for the same
	// listener. Without this arbitration two nodes could silently run one
	// listener as competing and broadcast at the same time, processing every
	// event twice.
	ErrConflictingDeliveryMode = errors.New("listener is already registered under a different delivery mode")

	// ErrConflictingOrdering reports an attachment whose ordering disagrees
	// with the ordering another node already registered for the same
	// listener. Without this arbitration an unordered node would claim
	// deliveries out of order behind the back of the FIFO nodes.
	ErrConflictingOrdering = errors.New("listener is already registered under a different ordering")
)

// Querier is the minimal execution surface the store runs its statements
// against. It is satisfied by *sql.DB, *sql.Tx, and *sql.Conn, and so by the
// querier transaction resolves from the context.
type Querier interface {
	QueryContext(ctx context.Context, query string, args ...any) (*sql.Rows, error)
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

// Store defines the outbound port for persisting messages, consumer
// registrations, and deliveries.
//
// CreateMessage and CreateDeliveries receive the querier of the caller, so
// the rows are written inside the same transaction as the business change
// that produced the event. Every other method runs on its own connection,
// because claims, settlements, and maintenance must be settled independently
// of any business transaction.
//
// ClaimDelivery reports whether the caller obtained the delivery: it returns
// false when another dispatcher already holds or settled it, which is the
// arbitration that keeps competing consumption at exactly one node. The
// settlement methods are fenced by the claim token, so a dispatcher whose
// claim lease expired and was stolen affects zero rows.
type Store interface {
	Initialize(ctx context.Context) error

	CreateMessage(ctx context.Context, querier Querier, message Message) error
	CreateDeliveries(ctx context.Context, querier Querier, keys []DeliveryKey) error

	RegisterListenerMode(ctx context.Context, id eventbus.ListenerID,
		mode DeliveryMode, ordering Ordering) (DeliveryMode, Ordering, error)
	RegisterConsumer(ctx context.Context, consumer Consumer) error
	Heartbeat(ctx context.Context, key ConsumerKey, at time.Time) (bool, error)
	RemoveListener(ctx context.Context, id eventbus.ListenerID) error

	MaterializeDeliveries(ctx context.Context, key ConsumerKey) (int64, error)
	AdvanceFrontier(ctx context.Context, key ConsumerKey, frontier time.Time) error

	// FindDueDeliveries returns the claimable deliveries of the consumer,
	// oldest publication first. For a FIFO consumer only the head of the
	// queue is claimable, and only below the materialization frontier, where
	// the publication order is known to be complete; the total order is
	// (publication_date, message_id), deterministic across the cluster.
	FindDueDeliveries(ctx context.Context, key ConsumerKey, ordering Ordering,
		now time.Time, leaseCutoff time.Time, limit int) ([]DueDelivery, error)
	FindExhaustedDeliveries(ctx context.Context, limit int) ([]DueDelivery, error)

	// ClaimDelivery atomically transitions a due delivery into processing
	// under the token, counting the attempt. The attempts value the claimant
	// observed is re-checked by the update, so it doubles as a fencing
	// generation: a claim based on a stale read affects zero rows.
	ClaimDelivery(ctx context.Context, key DeliveryKey, token string,
		now time.Time, leaseCutoff time.Time, attempts int) (bool, error)

	// ClaimDeliveryInOrder claims like ClaimDelivery but re-verifies inside
	// the update that no earlier incomplete delivery of the same consumer
	// exists, so a claim races neither a resubmitted predecessor nor another
	// node claiming ahead; publicationDate is the position of the claimed
	// message in the total order.
	ClaimDeliveryInOrder(ctx context.Context, key DeliveryKey, token string,
		now time.Time, leaseCutoff time.Time, attempts int, publicationDate time.Time) (bool, error)
	CompleteDelivery(ctx context.Context, key DeliveryKey, token string, completionDate time.Time) (bool, error)

	// FailDelivery settles a failed delivery under the token fence; attempts
	// is the attempt count this dispatcher's claim recorded, from which the
	// exhaustion decision is computed against maximumAttempts.
	FailDelivery(ctx context.Context, key DeliveryKey, token string, cause string,
		nextAttemptDate time.Time, attempts int, maximumAttempts int) (bool, error)
	ResubmitDelivery(ctx context.Context, key DeliveryKey) (bool, error)

	ExpireBroadcastConsumers(ctx context.Context, cutoff time.Time) (int64, error)
	DeleteSettledMessages(ctx context.Context, olderThan time.Time) (int64, error)
	DeleteMessagesOlderThan(ctx context.Context, cutoff time.Time) (int64, error)
	DeleteOrphanDeliveries(ctx context.Context) (int64, error)
}
