package sqlbus

import (
	"context"
	"log/slog"
	"time"

	"github.com/cgardev/gokeel/transaction"
)

// dispatchTimeout bounds one background delivery round, so a stalled listener
// cannot hold its goroutine forever. Deliveries that miss the deadline stay
// incomplete and are recovered by a Dispatcher once their claim lease
// expires.
const dispatchTimeout = 30 * time.Second

// QuerierSource resolves the querier the message rows are written through.
// It is satisfied by *transaction.Manager, whose Querier returns the active
// transaction when a unit of work is in progress.
type QuerierSource interface {
	Querier(ctx context.Context) transaction.Querier
}

// Publisher is the bridge between a business write and the shared bus: it
// stores the message inside the current unit of work and delivers it to the
// locally attached listeners only after that unit commits. Listeners on
// other nodes receive the event through their own Dispatcher.
//
// When no unit of work is active, the originating write has already
// auto-committed, so the local deliveries are dispatched immediately instead.
// A Publisher is immutable after construction and safe for concurrent use.
type Publisher struct {
	bridge       *Bridge
	querier      QuerierSource
	asynchronous bool
	timeout      time.Duration
}

// NewPublisher constructs a Publisher that writes through the querier source
// and dispatches the local deliveries synchronously after commit.
func NewPublisher(bridge *Bridge, querier QuerierSource) *Publisher {
	return &Publisher{bridge: bridge, querier: querier, timeout: dispatchTimeout}
}

// WithAsynchronousDispatch returns a Publisher that hands committed local
// deliveries to a background goroutine, so callers in a request path do not
// wait for slow listeners. The at-least-once guarantee is unchanged:
// deliveries settle only after their listener succeeds, and incomplete ones
// are recovered by a Dispatcher.
func (p *Publisher) WithAsynchronousDispatch() *Publisher {
	configured := *p
	configured.asynchronous = true
	return &configured
}

// Publish stores the event as one message through the current querier,
// pre-creates the delivery rows of the listeners attached on this node, and
// schedules their delivery for after the outermost transaction commits. When
// no unit of work is active the rows are already durable, so local delivery
// happens at once. Delivery failures do not fail the call: the affected
// deliveries stay incomplete and are recovered by a Dispatcher.
//
// The message is stored even when no listener is attached locally, because
// listeners on other nodes are unknown at publish time; a message no
// registered consumer covers is removed by retention.
func (p *Publisher) Publish(ctx context.Context, event any) error {
	_, keys, err := p.bridge.Publish(ctx, p.querier.Querier(ctx), event)
	if err != nil {
		return err
	}
	if len(keys) == 0 {
		return nil
	}

	dispatch := func(ctx context.Context) { p.dispatch(ctx, event, keys) }
	if transaction.RegisterAfterCommit(ctx, dispatch) {
		return nil
	}
	dispatch(ctx)
	return nil
}

func (p *Publisher) dispatch(ctx context.Context, event any, keys []DeliveryKey) {
	if p.asynchronous {
		// The dispatch context is detached from the caller, so cancelling the
		// request that produced the event does not abort its delivery.
		dispatchContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.timeout)
		go func() {
			defer cancel()
			p.report(dispatchContext, event, keys)
		}()
		return
	}
	p.report(ctx, event, keys)
}

func (p *Publisher) report(ctx context.Context, event any, keys []DeliveryKey) {
	if err := p.bridge.dispatchLocal(ctx, event, keys); err != nil {
		slog.Warn("event delivery failed; deliveries remain incomplete for redelivery", "error", err)
	}
}
