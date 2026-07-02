package sqlbus

import (
	"context"
	"errors"
	"fmt"
	"log/slog"
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

	// settlementTimeout bounds the database call that settles a delivery
	// outcome. Settlements run on a context that survives the caller's
	// cancellation, so without a deadline a wedged database call could hold
	// a stopping dispatcher forever.
	settlementTimeout = 30 * time.Second
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
	id            eventbus.ListenerID
	mode          DeliveryMode
	eventType     string
	configuration attachmentConfiguration
}

// attachmentConfiguration is the per-listener delivery behavior. The zero
// value defers to the bridge-level defaults.
type attachmentConfiguration struct {
	ordering        Ordering
	maximumAttempts int
	retryDelay      func(attempt int) time.Duration
}

// AttachOption customizes one listener at attachment time.
type AttachOption func(*attachmentConfiguration)

// WithOrderedDelivery attaches the listener as a FIFO consumer: its events
// are processed strictly in publication order, one at a time, cluster-wide
// for a competing listener and per node for a broadcast one. Ordered
// deliveries wait below the materialization frontier, so their latency is at
// least the materialization grace; every node must attach the listener with
// the same ordering.
func WithOrderedDelivery() AttachOption {
	return func(c *attachmentConfiguration) {
		c.ordering = OrderingFIFO
	}
}

// WithListenerMaximumAttempts overrides, for this listener, how many delivery
// attempts a delivery may consume before it becomes exhausted. Configure the
// same value on every node hosting the listener.
func WithListenerMaximumAttempts(attempts int) AttachOption {
	return func(c *attachmentConfiguration) {
		if attempts > 0 {
			c.maximumAttempts = attempts
		}
	}
}

// WithListenerRetryDelay overrides, for this listener, the backoff schedule
// of failed deliveries. Configure the same schedule on every node hosting the
// listener.
func WithListenerRetryDelay(delay func(attempt int) time.Duration) AttachOption {
	return func(c *attachmentConfiguration) {
		if delay != nil {
			c.retryDelay = delay
		}
	}
}

// AttachmentRegistration describes one listener to Attach: the untyped seam
// the typed attachment functions and the Broker adapter both go through.
type AttachmentRegistration struct {
	// ID identifies the listener; it must be unique within the bus.
	ID eventbus.ListenerID

	// Matches decides which events the listener receives on the local bus.
	Matches func(event any) bool

	// Probe carries the zero value of the consumed event type, so the bridge
	// resolves the persistent event type name through its serializer without
	// invoking the handler.
	Probe any

	// Handle processes one delivered event.
	Handle eventbus.Handler

	// Mode selects competing or broadcast consumption.
	Mode DeliveryMode

	// Options carry the per-listener delivery behavior.
	Options []AttachOption
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
	// subscribed remembers the local bus subscriptions this bridge created,
	// so an Attach that failed after subscribing can be retried without
	// tripping over its own leftover subscription.
	subscribed map[eventbus.ListenerID]bool
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
		subscribed:           make(map[eventbus.ListenerID]bool),
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
// event is delivered exactly once cluster-wide, by whichever hosting node
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
	options ...AttachOption,
) error {
	return attachTyped(ctx, bridge, id, DeliveryModeCompeting, handle, options)
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
	options ...AttachOption,
) error {
	return attachTyped(ctx, bridge, id, DeliveryModeBroadcast, handle, options)
}

// attachTyped assembles the untyped registration from the type parameter and
// hands it to Attach.
func attachTyped[T any](
	ctx context.Context,
	bridge *Bridge,
	id eventbus.ListenerID,
	mode DeliveryMode,
	handle func(ctx context.Context, event T) error,
	options []AttachOption,
) error {
	var probe T
	return bridge.Attach(ctx, AttachmentRegistration{
		ID: id,
		Matches: func(event any) bool {
			_, ok := event.(T)
			return ok
		},
		Probe: probe,
		Handle: func(ctx context.Context, event any) error {
			typed, ok := event.(T)
			if !ok {
				return fmt.Errorf("listener %s received an event of unexpected type %T", id, event)
			}
			return handle(ctx, typed)
		},
		Mode:    mode,
		Options: options,
	})
}

// Attach subscribes the listener on the local bus and registers it durably
// under its delivery mode and ordering. It is the untyped seam the typed
// attachment functions and the Broker adapter go through.
func (b *Bridge) Attach(ctx context.Context, registration AttachmentRegistration) error {
	id := registration.ID
	configuration := attachmentConfiguration{
		ordering:        OrderingUnordered,
		maximumAttempts: b.maximumAttempts,
		retryDelay:      b.retryDelay,
	}
	for _, option := range registration.Options {
		option(&configuration)
	}

	// Serializing the zero value resolves the persistent event type name and
	// rejects an attachment whose type was never registered, before any
	// durable row is written.
	eventType, _, err := b.serializer.Serialize(registration.Probe)
	if err != nil {
		return fmt.Errorf("attach %s: %w", id, err)
	}

	b.mu.Lock()
	for _, existing := range b.attachments {
		if existing.id == id {
			b.mu.Unlock()
			return fmt.Errorf("%w: %s", eventbus.ErrDuplicateListener, id)
		}
	}
	alreadySubscribed := b.subscribed[id]
	b.mu.Unlock()

	modeWinner, orderingWinner, err := b.store.RegisterListenerMode(ctx, id, registration.Mode, configuration.ordering)
	if err != nil {
		return err
	}
	if modeWinner != registration.Mode {
		return fmt.Errorf("%w: %s is registered as %s", ErrConflictingDeliveryMode, id, modeWinner)
	}
	if orderingWinner != configuration.ordering {
		return fmt.Errorf("%w: %s is registered as %s", ErrConflictingOrdering, id, orderingWinner)
	}

	// The local subscription comes before the durable consumer registration:
	// a rejected subscription (for example, a duplicate identifier) is a
	// deterministic programming error, and failing on it here leaves no
	// durable row behind that would pin messages in retention. A subscription
	// left over from an Attach that failed later is reused, so the attachment
	// can be retried after a transient failure.
	if !alreadySubscribed {
		if err := b.bus.Subscribe(id, registration.Matches, registration.Handle); err != nil {
			return err
		}
		b.mu.Lock()
		b.subscribed[id] = true
		b.mu.Unlock()
	}

	now := time.Now().UTC()
	boundary := now.Add(-b.materializationGrace)
	if err := b.store.RegisterConsumer(ctx, Consumer{
		ListenerID:       id,
		Instance:         b.instanceFor(registration.Mode),
		EventType:        eventType,
		DeliveryMode:     registration.Mode,
		StartBoundary:    boundary,
		Frontier:         boundary,
		RegistrationDate: now,
		HeartbeatDate:    now,
	}); err != nil {
		return err
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	b.attachments = append(b.attachments, attachment{
		id:            id,
		mode:          registration.Mode,
		eventType:     eventType,
		configuration: configuration,
	})
	return nil
}

// attachmentFor returns the attachment of the listener, reporting false when
// the listener is not attached on this node.
func (b *Bridge) attachmentFor(id eventbus.ListenerID) (attachment, bool) {
	b.mu.RLock()
	defer b.mu.RUnlock()
	for _, attached := range b.attachments {
		if attached.id == id {
			return attached, true
		}
	}
	return attachment{}, false
}

// Detach removes the durable registrations of the listener, unpinning its
// messages from retention. It is the decommissioning step for a listener
// whose code was removed: call it after the last node hosting the listener
// stopped, because a node that still hosts it — including this one, when its
// Dispatcher is still running — re-registers the consumer on its next
// heartbeat. The subscription on the local in-process bus remains until the
// process restarts.
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

// FindExhausted returns the dead letters — deliveries that consumed their
// attempt budget — oldest first, up to the limit. Each carries its message
// and the last failure cause, so an operator can inspect them and revive one
// with Resubmit.
func (b *Bridge) FindExhausted(ctx context.Context, limit int) ([]DueDelivery, error) {
	return b.store.FindExhaustedDeliveries(ctx, limit)
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

// Publish stores the event as one message, together with the pending delivery
// rows of the listeners attached on this node, through the provided querier —
// so the rows join the caller's transaction. Deliveries of listeners hosted
// on other nodes are materialized there by their dispatchers.
//
// Publish is the low-level seam for callers that manage their own
// transactions; the stored deliveries are picked up by the dispatchers once
// the transaction commits. Most callers use Publisher instead, which resolves
// the querier from the active unit of work and dispatches the local
// deliveries immediately after commit.
func (b *Bridge) Publish(ctx context.Context, querier Querier, event any) (Message, []DeliveryKey, error) {
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
// event, the Publisher's after-commit fast path. FIFO listeners are skipped:
// their deliveries must wait below the materialization frontier, where the
// publication order is complete, so their own Dispatcher serves them. The
// returned error joins the failures of every delivery that could not be
// completed; those deliveries stay claimable and are recovered by a
// Dispatcher.
func (b *Bridge) dispatchLocal(ctx context.Context, event any, keys []DeliveryKey) error {
	var failures []error
	for _, key := range keys {
		attached, found := b.attachmentFor(key.ListenerID)
		if !found || attached.configuration.ordering == OrderingFIFO {
			continue
		}
		restore := func() (any, error) { return event, nil }
		if err := b.dispatchDelivery(ctx, attached, key, 0, time.Time{}, restore); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// dispatchDelivery runs one delivery through the state machine: claim it
// under a fresh token, restore the event, deliver through the local bus, and
// settle the outcome. A FIFO listener's delivery is claimed with the ordered
// claim, which re-verifies the head-of-line condition at its publication
// position. An unclaimable delivery is another dispatcher's work and reports
// no error. Settlements run on a bounded context that survives the caller's
// cancellation, so a shutdown mid-delivery cannot strand a processing row
// until its lease expires, and a wedged settlement cannot hold a stopping
// dispatcher forever.
func (b *Bridge) dispatchDelivery(
	ctx context.Context,
	attached attachment,
	key DeliveryKey,
	attemptsBeforeClaim int,
	publicationDate time.Time,
	restore func() (any, error),
) error {
	// The attachment travels from the caller's snapshot, so the claim-mode
	// selection and the retry policy of one dispatch always agree.
	configuration := attached.configuration

	token := uuid.NewString()
	now := time.Now().UTC()
	leaseCutoff := now.Add(-b.leaseDuration)
	var claimed bool
	var err error
	if configuration.ordering == OrderingFIFO {
		claimed, err = b.store.ClaimDeliveryInOrder(
			ctx, key, token, now, leaseCutoff, attemptsBeforeClaim, publicationDate)
	} else {
		claimed, err = b.store.ClaimDelivery(ctx, key, token, now, leaseCutoff, attemptsBeforeClaim)
	}
	if err != nil {
		return err
	}
	if !claimed {
		return nil
	}

	event, err := restore()
	if err == nil {
		// The listener must finish within the claim lease; past it, another
		// node may steal the claim, so running longer only produces work a
		// zombie cannot settle.
		deliveryContext, cancel := context.WithTimeout(ctx, b.leaseDuration)
		err = b.bus.Deliver(deliveryContext, key.ListenerID, event)
		cancel()
	}
	if err != nil {
		deliveryError := fmt.Errorf("deliver %s to %s: %w", key.MessageID, key.ListenerID, err)
		if ctx.Err() != nil {
			// The caller is shutting down: the failure says nothing about the
			// listener, so the attempt budget is not charged beyond the claim.
			// The delivery stays processing and lease expiry recovers it.
			return deliveryError
		}
		attempt := attemptsBeforeClaim + 1
		nextAttemptDate := time.Now().UTC().Add(configuration.retryDelay(attempt))
		settleContext, settleCancel := settlementContext(ctx)
		defer settleCancel()
		settled, markError := b.store.FailDelivery(
			settleContext, key, token, err.Error(), nextAttemptDate,
			attempt, configuration.maximumAttempts,
		)
		if markError != nil {
			return errors.Join(deliveryError, markError)
		}
		if !settled {
			b.reportFencedSettlement(key)
		}
		return deliveryError
	}

	settleContext, settleCancel := settlementContext(ctx)
	defer settleCancel()
	settled, err := b.store.CompleteDelivery(settleContext, key, token, time.Now().UTC())
	if err != nil {
		return err
	}
	if !settled {
		b.reportFencedSettlement(key)
	}
	return nil
}

// settlementContext detaches the settlement from the caller's cancellation
// and bounds it, so outcomes are recorded during a shutdown but a wedged
// database call cannot hold the caller forever.
func settlementContext(ctx context.Context) (context.Context, context.CancelFunc) {
	return context.WithTimeout(context.WithoutCancel(ctx), settlementTimeout)
}

// reportFencedSettlement surfaces a settlement that affected zero rows: the
// claim was stolen after its lease expired, so the listener's work was, or
// will be, repeated elsewhere. A recurring report means the lease is shorter
// than the slowest listener.
func (b *Bridge) reportFencedSettlement(key DeliveryKey) {
	slog.Warn("delivery settlement was fenced out; the claim lease may be shorter than the listener",
		"message", key.MessageID, "listener", key.ListenerID)
}
