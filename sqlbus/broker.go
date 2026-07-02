package sqlbus

import (
	"context"
	"strings"

	"github.com/cgardev/gokeel/eventbus"

	"github.com/google/uuid"
)

// Broker adapts the sqlbus machinery to the eventbus.Broker contract: the
// same consumer-facing interface as the in-memory engine, with persistence
// and cross-node consumption added underneath. Publications go through the
// transactional Publisher, so they join the caller's unit of work; consumers
// attach durably through the Bridge, competing cluster-wide by default and
// broadcast per node on request.
//
// Delivery settles exactly once per consumer, but a crash or an expired claim
// lease re-executes the handler, so handlers must be idempotent — the
// documented cost of durability. The Workers consumer option is not
// interpreted: the concurrency of this engine is governed by the Dispatcher
// batch size and by how many nodes host the listener.
type Broker struct {
	bridge    *Bridge
	publisher *Publisher
}

// NewBroker constructs a Broker over the bridge and the publisher. The caller
// keeps running a Dispatcher per node, exactly as with the lower-level API.
func NewBroker(bridge *Bridge, publisher *Publisher) *Broker {
	return &Broker{bridge: bridge, publisher: publisher}
}

var _ eventbus.Broker = (*Broker)(nil)

// Publish stores the event through the current unit of work and schedules its
// deliveries. It reports only serialization and persistence failures; handler
// outcomes settle asynchronously and surface as dead letters.
func (b *Broker) Publish(ctx context.Context, event any) error {
	return b.publisher.Publish(ctx, event)
}

// Subscribe attaches the consumer durably. The consumed event type must be
// registered on the serializer of the bridge.
func (b *Broker) Subscribe(ctx context.Context, registration eventbus.ConsumerRegistration) error {
	configuration := registration.Configuration
	mode := DeliveryModeCompeting
	if configuration.Broadcast {
		mode = DeliveryModeBroadcast
	}
	options := []AttachOption{
		WithListenerMaximumAttempts(configuration.MaximumAttempts),
		WithListenerRetryDelay(configuration.RetryDelay),
	}
	if configuration.Ordering == eventbus.OrderingFIFO {
		options = append(options, WithOrderedDelivery())
	}
	return b.bridge.Attach(ctx, AttachmentRegistration{
		ID:      registration.ID,
		Matches: registration.Matches,
		Probe:   registration.Probe,
		Handle:  registration.Handle,
		Mode:    mode,
		Options: options,
	})
}

// FindExhausted returns the dead letters, oldest first, up to the limit. The
// event of a dead letter is restored from its serialized form when the type
// is registered on this node's serializer.
func (b *Broker) FindExhausted(ctx context.Context, limit int) ([]eventbus.DeadLetter, error) {
	exhausted, err := b.bridge.FindExhausted(ctx, limit)
	if err != nil {
		return nil, err
	}
	letters := make([]eventbus.DeadLetter, 0, len(exhausted))
	for _, work := range exhausted {
		letter := eventbus.DeadLetter{
			Reference:       encodeDeadLetterReference(work.Delivery.Key),
			ListenerID:      work.Delivery.Key.ListenerID,
			Attempts:        work.Delivery.Attempts,
			LastError:       work.Delivery.LastError,
			PublicationDate: work.Message.PublicationDate,
		}
		if event, err := b.bridge.serializer.Deserialize(
			work.Message.EventType, work.Message.SerializedEvent); err == nil {
			letter.Event = event
		}
		letters = append(letters, letter)
	}
	return letters, nil
}

// Resubmit gives the referenced dead letter a fresh attempt budget. It
// reports false when the reference is unknown or was already resubmitted.
func (b *Broker) Resubmit(ctx context.Context, reference string) (bool, error) {
	key, ok := decodeDeadLetterReference(reference)
	if !ok {
		return false, nil
	}
	return b.bridge.Resubmit(ctx, key)
}

// encodeDeadLetterReference renders the delivery key as an opaque reference.
// The listener identifier goes last because it is the only segment that may
// contain the separator.
func encodeDeadLetterReference(key DeliveryKey) string {
	return key.MessageID.String() + "/" + key.Instance + "/" + string(key.ListenerID)
}

func decodeDeadLetterReference(reference string) (DeliveryKey, bool) {
	segments := strings.SplitN(reference, "/", 3)
	if len(segments) != 3 {
		return DeliveryKey{}, false
	}
	messageID, err := uuid.Parse(segments[0])
	if err != nil {
		return DeliveryKey{}, false
	}
	return DeliveryKey{
		MessageID:  messageID,
		Instance:   segments[1],
		ListenerID: eventbus.ListenerID(segments[2]),
	}, true
}
