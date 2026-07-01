package sqlbus

import (
	"context"
	"errors"
	"fmt"
	"sync"
	"time"

	"github.com/cgardev/gokeel/eventbus"

	"github.com/google/uuid"
)

const (
	defaultMaximumAttempts      = 5
	defaultLeaseDuration        = 5 * time.Minute
	defaultMaterializationGrace = 10 * time.Minute
	defaultInitialRetryDelay    = 5 * time.Second
	defaultMaximumRetryDelay    = 5 * time.Minute
)

// EventBus is the slice of the in-process bus the bridge relies on. It is
// satisfied by *eventbus.Bus.
type EventBus interface {
	Subscribe(id eventbus.ListenerID, matches func(event any) bool, handle eventbus.Handler) error
	Deliver(ctx context.Context, id eventbus.ListenerID, event any) error
}

// attachment is the in-memory record of one listener this node hosts: the
// dispatcher materializes, claims, and delivers only for attached listeners.
type attachment struct {
	id        eventbus.ListenerID
	mode      DeliveryMode
	eventType string
}

// Bridge connects the local in-process bus to the shared database: it stores
// published events as messages, pre-creates the delivery rows of the
// listeners attached on this node, and runs the claim, deliver, and settle
// state machine both for the Publisher's after-commit path and for the
// Dispatcher's polling path.
//
// Attach every listener and register every event type before publishing or
// starting a Dispatcher. A Bridge is safe for concurrent use.
type Bridge struct {
	store      Store
	bus        EventBus
	serializer Serializer
	node       NodeID

	maximumAttempts      int
	leaseDuration        time.Duration
	materializationGrace time.Duration
	retryDelay           func(attempt int) time.Duration

	mu          sync.RWMutex
	attachments []attachment
}

// BridgeOption customizes a Bridge at construction time.
type BridgeOption func(*Bridge)

// WithNodeIdentifier overrides the generated per-process node identifier.
// A stable identifier makes a restarted node resume its broadcast consumers
// instead of starting fresh ones; it must be unique per running process.
func WithNodeIdentifier(node NodeID) BridgeOption {
	return func(b *Bridge) {
		if node != "" {
			b.node = node
		}
	}
}

// WithMaximumAttempts overrides how many delivery attempts a delivery may
// consume before it becomes exhausted (default 5). Configure the same value
// on every node: the budget is evaluated against the shared attempt counter.
func WithMaximumAttempts(attempts int) BridgeOption {
	return func(b *Bridge) {
		if attempts > 0 {
			b.maximumAttempts = attempts
		}
	}
}

// WithLeaseDuration overrides how long a claim protects a processing delivery
// before another dispatcher may steal it (default 5 minutes). It must exceed
// the slowest listener, or a slow delivery is repeated on another node.
// Configure the same value on every node.
func WithLeaseDuration(lease time.Duration) BridgeOption {
	return func(b *Bridge) {
		if lease > 0 {
			b.leaseDuration = lease
		}
	}
}

// WithMaterializationGrace overrides the overlap window that protects
// materialization against publications whose transaction commits after later
// events became visible (default 10 minutes). It must exceed the longest
// business transaction that publishes events; a freshly attached listener may
// receive events published up to this long before its attachment. Configure
// the same value on every node.
func WithMaterializationGrace(grace time.Duration) BridgeOption {
	return func(b *Bridge) {
		if grace > 0 {
			b.materializationGrace = grace
		}
	}
}

// WithRetryDelay overrides the backoff schedule of failed deliveries: the
// function receives the attempt count just spent (1 for the first attempt)
// and returns how long to wait before the next one. The default doubles from
// 5 seconds up to 5 minutes.
func WithRetryDelay(delay func(attempt int) time.Duration) BridgeOption {
	return func(b *Bridge) {
		if delay != nil {
			b.retryDelay = delay
		}
	}
}

// NewBridge constructs a Bridge over the store, the local bus, and the
// serializer. Without WithNodeIdentifier the node identity is generated per
// process, so a restarted node joins as a fresh broadcast consumer while its
// previous identity expires through the heartbeat.
func NewBridge(store Store, bus EventBus, serializer Serializer, options ...BridgeOption) *Bridge {
	bridge := &Bridge{
		store:                store,
		bus:                  bus,
		serializer:           serializer,
		node:                 NodeID(uuid.NewString()),
		maximumAttempts:      defaultMaximumAttempts,
		leaseDuration:        defaultLeaseDuration,
		materializationGrace: defaultMaterializationGrace,
		retryDelay:           defaultRetryDelay,
	}
	for _, option := range options {
		option(bridge)
	}
	return bridge
}

// defaultRetryDelay doubles the delay per attempt, from the initial delay up
// to the maximum.
func defaultRetryDelay(attempt int) time.Duration {
	delay := defaultInitialRetryDelay
	for count := 1; count < attempt; count++ {
		delay *= 2
		if delay >= defaultMaximumRetryDelay {
			return defaultMaximumRetryDelay
		}
	}
	return delay
}

// AttachCompetingListener subscribes the listener on the local bus and
// registers it as the competing consumer of every event of type T: each
// event is processed exactly once cluster-wide, by whichever hosting node
// claims it first. T must already be registered on the serializer.
//
// A freshly attached listener starts at the attachment time minus the
// materialization grace, so publications still in flight are never missed;
// it does not replay older history.
func AttachCompetingListener[T any](
	ctx context.Context,
	bridge *Bridge,
	id eventbus.ListenerID,
	handle func(ctx context.Context, event T) error,
) error {
	return attachListener(ctx, bridge, id, DeliveryModeCompeting, handle)
}

// AttachBroadcastListener subscribes the listener on the local bus and
// registers this node as one broadcast consumer of every event of type T:
// each event is processed once per hosting node, which suits node-local
// concerns such as cache invalidation. T must already be registered on the
// serializer.
func AttachBroadcastListener[T any](
	ctx context.Context,
	bridge *Bridge,
	id eventbus.ListenerID,
	handle func(ctx context.Context, event T) error,
) error {
	return attachListener(ctx, bridge, id, DeliveryModeBroadcast, handle)
}

func attachListener[T any](
	ctx context.Context,
	bridge *Bridge,
	id eventbus.ListenerID,
	mode DeliveryMode,
	handle func(ctx context.Context, event T) error,
) error {
	// Serializing the zero value resolves the persistent event type name and
	// rejects an attachment whose type was never registered, before any
	// durable row is written.
	var probe T
	eventType, _, err := bridge.serializer.Serialize(probe)
	if err != nil {
		return fmt.Errorf("attach %s: %w", id, err)
	}

	winner, err := bridge.store.RegisterListenerMode(ctx, id, mode)
	if err != nil {
		return err
	}
	if winner != mode {
		return fmt.Errorf("%w: %s is registered as %s", ErrConflictingDeliveryMode, id, winner)
	}

	// The local subscription comes before the durable consumer registration:
	// a rejected subscription (for example, a duplicate identifier) is a
	// deterministic programming error, and failing on it here leaves no
	// durable row behind that would pin messages in retention.
	err = bridge.bus.Subscribe(id,
		func(event any) bool {
			_, ok := event.(T)
			return ok
		},
		func(ctx context.Context, event any) error {
			typed, ok := event.(T)
			if !ok {
				return fmt.Errorf("listener %s received an event of unexpected type %T", id, event)
			}
			return handle(ctx, typed)
		})
	if err != nil {
		return err
	}

	now := time.Now().UTC()
	boundary := now.Add(-bridge.materializationGrace)
	if err := bridge.store.RegisterConsumer(ctx, Consumer{
		ListenerID:       id,
		Instance:         bridge.instanceFor(mode),
		EventType:        eventType,
		DeliveryMode:     mode,
		StartBoundary:    boundary,
		Frontier:         boundary,
		RegistrationDate: now,
		HeartbeatDate:    now,
	}); err != nil {
		return err
	}

	bridge.mu.Lock()
	defer bridge.mu.Unlock()
	bridge.attachments = append(bridge.attachments, attachment{id: id, mode: mode, eventType: eventType})
	return nil
}

// Detach removes the durable registrations of the listener, unpinning its
// messages from retention. It is the decommissioning step for a listener
// whose code was removed: call it after the last node hosting the listener
// stopped, because a node that still hosts it re-registers the consumer on
// its next heartbeat. The subscription on the local in-process bus remains
// until the process restarts.
func (b *Bridge) Detach(ctx context.Context, id eventbus.ListenerID) error {
	if err := b.store.RemoveListener(ctx, id); err != nil {
		return err
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	remaining := b.attachments[:0]
	for _, existing := range b.attachments {
		if existing.id != id {
			remaining = append(remaining, existing)
		}
	}
	b.attachments = remaining
	return nil
}

// Resubmit gives a failed or exhausted delivery a fresh attempt budget, so an
// operator can revive a dead letter after fixing its listener. It reports
// false when the delivery is not in a resubmittable state.
func (b *Bridge) Resubmit(ctx context.Context, key DeliveryKey) (bool, error) {
	return b.store.ResubmitDelivery(ctx, key)
}

// Node returns the identity this bridge participates in the cluster under.
func (b *Bridge) Node() NodeID {
	return b.node
}

func (b *Bridge) instanceFor(mode DeliveryMode) string {
	if mode == DeliveryModeBroadcast {
		return string(b.node)
	}
	return ""
}

func (b *Bridge) snapshotAttachments() []attachment {
	b.mu.RLock()
	defer b.mu.RUnlock()
	snapshot := make([]attachment, len(b.attachments))
	copy(snapshot, b.attachments)
	return snapshot
}

// publish stores the message and the pending delivery rows of the listeners
// attached on this node through the caller's querier, so they join the
// business transaction. Deliveries of listeners hosted on other nodes are
// materialized there by their dispatchers.
func (b *Bridge) publish(ctx context.Context, querier Querier, event any) (Message, []DeliveryKey, error) {
	eventType, payload, err := b.serializer.Serialize(event)
	if err != nil {
		return Message{}, nil, err
	}

	// Version 7 identifiers are time-ordered, which keeps the primary key
	// index of the message table append-friendly; ordering guarantees rest on
	// the publication date, not on the identifier.
	id, err := uuid.NewV7()
	if err != nil {
		return Message{}, nil, fmt.Errorf("generate message identifier: %w", err)
	}
	message := Message{
		ID:              id,
		EventType:       eventType,
		SerializedEvent: payload,
		PublisherNode:   b.node,
		PublicationDate: time.Now().UTC(),
	}
	if err := b.store.CreateMessage(ctx, querier, message); err != nil {
		return Message{}, nil, err
	}

	keys := b.localDeliveryKeys(message)
	if err := b.store.CreateDeliveries(ctx, querier, keys); err != nil {
		return Message{}, nil, err
	}
	return message, keys, nil
}

func (b *Bridge) localDeliveryKeys(message Message) []DeliveryKey {
	b.mu.RLock()
	defer b.mu.RUnlock()
	var keys []DeliveryKey
	for _, attached := range b.attachments {
		if attached.eventType != message.EventType {
			continue
		}
		keys = append(keys, DeliveryKey{
			MessageID:  message.ID,
			ListenerID: attached.id,
			Instance:   b.instanceFor(attached.mode),
		})
	}
	return keys
}

// dispatchLocal claims and delivers the given deliveries with the in-memory
// event, the Publisher's after-commit fast path. The returned error joins the
// failures of every delivery that could not be completed; those deliveries
// stay claimable and are recovered by a Dispatcher.
func (b *Bridge) dispatchLocal(ctx context.Context, event any, keys []DeliveryKey) error {
	var failures []error
	for _, key := range keys {
		restore := func() (any, error) { return event, nil }
		if err := b.dispatchDelivery(ctx, key, 0, restore); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// dispatchDelivery runs one delivery through the state machine: claim it
// under a fresh token, restore the event, deliver through the local bus, and
// settle the outcome. An unclaimable delivery is anothers dispatcher's work
// and reports no error. Settlements run on a context that survives the
// caller's cancellation, so a shutdown mid-delivery cannot strand a
// processing row until its lease expires.
func (b *Bridge) dispatchDelivery(
	ctx context.Context,
	key DeliveryKey,
	attemptsBeforeClaim int,
	restore func() (any, error),
) error {
	token := uuid.NewString()
	now := time.Now().UTC()
	claimed, err := b.store.ClaimDelivery(ctx, key, token, now, now.Add(-b.leaseDuration))
	if err != nil {
		return err
	}
	if !claimed {
		return nil
	}

	settleContext := context.WithoutCancel(ctx)
	event, err := restore()
	if err == nil {
		err = b.bus.Deliver(ctx, key.ListenerID, event)
	}
	if err != nil {
		deliveryError := fmt.Errorf("deliver %s to %s: %w", key.MessageID, key.ListenerID, err)
		attempt := attemptsBeforeClaim + 1
		nextAttemptDate := time.Now().UTC().Add(b.retryDelay(attempt))
		if _, markError := b.store.FailDelivery(
			settleContext, key, token, err.Error(), nextAttemptDate, b.maximumAttempts,
		); markError != nil {
			return errors.Join(deliveryError, markError)
		}
		return deliveryError
	}

	if _, err := b.store.CompleteDelivery(settleContext, key, token, time.Now().UTC()); err != nil {
		return err
	}
	return nil
}
