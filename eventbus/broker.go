package eventbus

import (
	"context"
	"fmt"
	"time"
)

// Ordering selects how a consumer works through its queue.
type Ordering string

const (
	// OrderingFIFO processes the events of the consumer strictly in
	// publication order, one at a time. A failing event blocks its successors
	// while it retries; once it exhausts its attempt budget it is parked as a
	// dead letter and the queue continues.
	OrderingFIFO Ordering = "FIFO"

	// OrderingUnordered processes the events of the consumer concurrently,
	// with no ordering guarantee. A failing event retries on its own schedule
	// without delaying the others.
	OrderingUnordered Ordering = "UNORDERED"
)

const (
	defaultConsumerMaximumAttempts = 5
	defaultConsumerWorkers         = 8
	defaultInitialRetryDelay       = 5 * time.Second
	defaultMaximumRetryDelay       = 5 * time.Minute
)

// ConsumerConfiguration is the resolved behavior of one consumer. Construct
// it through DefaultConsumerConfiguration and the ConsumerOption functions.
type ConsumerConfiguration struct {
	// Ordering selects FIFO (the default) or unordered processing.
	Ordering Ordering

	// MaximumAttempts bounds how many delivery attempts one event may consume
	// before it is parked as a dead letter (default 5).
	MaximumAttempts int

	// RetryDelay returns how long to wait before the next attempt, given the
	// attempt count just spent (1 for the first attempt). The default doubles
	// from 5 seconds up to 5 minutes.
	RetryDelay func(attempt int) time.Duration

	// Workers bounds how many events an unordered consumer processes at once
	// (default 8). A FIFO consumer always processes one event at a time.
	Workers int

	// Broadcast requests one delivery per application node instead of one
	// delivery per consumer. Engines confined to a single process treat it as
	// regular consumption, because the node and the consumer coincide.
	Broadcast bool
}

// DefaultConsumerConfiguration returns the configuration every consumer
// starts from: FIFO ordering, five attempts, and a doubling backoff.
func DefaultConsumerConfiguration() ConsumerConfiguration {
	return ConsumerConfiguration{
		Ordering:        OrderingFIFO,
		MaximumAttempts: defaultConsumerMaximumAttempts,
		RetryDelay:      defaultConsumerRetryDelay,
		Workers:         defaultConsumerWorkers,
	}
}

// defaultConsumerRetryDelay doubles the delay per attempt, from the initial
// delay up to the maximum.
func defaultConsumerRetryDelay(attempt int) time.Duration {
	delay := defaultInitialRetryDelay
	for count := 1; count < attempt; count++ {
		delay *= 2
		if delay >= defaultMaximumRetryDelay {
			return defaultMaximumRetryDelay
		}
	}
	return delay
}

// ConsumerOption customizes one consumer at subscription time.
type ConsumerOption func(*ConsumerConfiguration)

// WithUnorderedDelivery opts the consumer out of FIFO ordering: events are
// processed concurrently and a failing event retries without delaying the
// others.
func WithUnorderedDelivery() ConsumerOption {
	return func(c *ConsumerConfiguration) {
		c.Ordering = OrderingUnordered
	}
}

// WithMaximumAttempts overrides how many delivery attempts one event may
// consume before it is parked as a dead letter (default 5).
func WithMaximumAttempts(attempts int) ConsumerOption {
	return func(c *ConsumerConfiguration) {
		if attempts > 0 {
			c.MaximumAttempts = attempts
		}
	}
}

// WithRetryDelay overrides the backoff schedule of failed deliveries: the
// function receives the attempt count just spent (1 for the first attempt)
// and returns how long to wait before the next one.
func WithRetryDelay(delay func(attempt int) time.Duration) ConsumerOption {
	return func(c *ConsumerConfiguration) {
		if delay != nil {
			c.RetryDelay = delay
		}
	}
}

// WithWorkers overrides how many events an unordered consumer processes at
// once (default 8). A FIFO consumer ignores it and processes one at a time.
func WithWorkers(workers int) ConsumerOption {
	return func(c *ConsumerConfiguration) {
		if workers > 0 {
			c.Workers = workers
		}
	}
}

// WithBroadcastDelivery requests one delivery per application node instead of
// one delivery per consumer, for node-local concerns such as invalidating an
// in-memory cache. Engines confined to a single process treat it as regular
// consumption.
func WithBroadcastDelivery() ConsumerOption {
	return func(c *ConsumerConfiguration) {
		c.Broadcast = true
	}
}

// ConsumerRegistration describes one consumer to a Broker engine.
type ConsumerRegistration struct {
	// ID identifies the consumer; it must be unique within the broker.
	ID ListenerID

	// Matches decides which events the consumer receives.
	Matches func(event any) bool

	// Probe carries the zero value of the consumed event type, so an engine
	// that persists events can resolve the durable type name without invoking
	// the handler. In-memory engines ignore it.
	Probe any

	// Handle processes one delivered event.
	Handle Handler

	// Configuration is the resolved consumer behavior.
	Configuration ConsumerConfiguration
}

// DeadLetter describes an event whose delivery consumed its attempt budget
// for one consumer. It stays inspectable until an operator resubmits it.
type DeadLetter struct {
	// Reference identifies the dead letter towards Resubmit. Its format is
	// engine-specific and opaque to the caller.
	Reference string

	// ListenerID names the consumer whose delivery exhausted its budget.
	ListenerID ListenerID

	// Event holds the parked event when the engine can restore it.
	Event any

	// Attempts counts the delivery attempts consumed.
	Attempts int

	// LastError describes the failure of the final attempt.
	LastError string

	// PublicationDate records when the event was published.
	PublicationDate time.Time
}

// Broker delivers each published event to every matching consumer exactly
// once per consumer, retrying independently per consumer with a bounded
// attempt budget. Consumers process FIFO by default and may opt out for
// concurrency. The contract is engine-independent: the in-memory engine of
// this package keeps everything in the process, while a persistent engine
// (such as the sqlbus module) adds durability and cross-node consumption
// behind the same interface.
//
// Publish reports only validation and persistence failures: handler outcomes
// are settled asynchronously through the per-consumer retry machinery and
// surface as dead letters once the attempt budget is consumed. In-process
// engines deliver exactly once per consumer for the lifetime of the process;
// persistent engines settle each delivery exactly once but may execute a
// handler again after a crash or an expired claim, so handlers must be
// idempotent when durability is in play.
type Broker interface {
	// Publish hands the event to every matching consumer's queue.
	Publish(ctx context.Context, event any) error

	// Subscribe registers a consumer. Engines with durable registrations use
	// the context for their writes.
	Subscribe(ctx context.Context, registration ConsumerRegistration) error

	// FindExhausted returns the dead letters, oldest first, up to the limit.
	FindExhausted(ctx context.Context, limit int) ([]DeadLetter, error)

	// Resubmit gives the referenced dead letter a fresh attempt budget. It
	// reports false when the reference is unknown or was already resubmitted.
	Resubmit(ctx context.Context, reference string) (bool, error)
}

// Consume registers a consumer of every event of type T on the broker. It is
// the typed front door of the Broker contract: the untyped registration is
// assembled from the type parameter, with FIFO ordering and the default retry
// budget unless options say otherwise.
func Consume[T any](
	ctx context.Context,
	broker Broker,
	id ListenerID,
	handle func(ctx context.Context, event T) error,
	options ...ConsumerOption,
) error {
	configuration := DefaultConsumerConfiguration()
	for _, option := range options {
		option(&configuration)
	}

	var probe T
	return broker.Subscribe(ctx, ConsumerRegistration{
		ID: id,
		Matches: func(event any) bool {
			_, ok := event.(T)
			return ok
		},
		Probe: probe,
		Handle: func(ctx context.Context, event any) error {
			typed, ok := event.(T)
			if !ok {
				return fmt.Errorf("consumer %s received an event of unexpected type %T", id, event)
			}
			return handle(ctx, typed)
		},
		Configuration: configuration,
	})
}
