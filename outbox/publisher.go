package outbox

import (
	"context"
	"log/slog"
	"time"

	"github.com/cgardev/gokeel/transaction"
)

// dispatchTimeout bounds one background delivery round, so a stalled listener
// cannot hold its goroutine forever. Publications that miss the deadline stay
// incomplete and are recovered through ResubmitIncomplete.
const dispatchTimeout = 30 * time.Second

// QuerierSource resolves the querier the publication rows are written through.
// It is satisfied by *transaction.Manager, whose Querier returns the
// active transaction when a unit of work is in progress.
type QuerierSource interface {
	Querier(ctx context.Context) transaction.Querier
}

// Publisher is the bridge between a business write and the event registry: it
// stores the publications inside the current unit of work and delivers them
// only after that unit commits. It is the analog of Spring Modulith's
// persistent application event multicaster, with the after-commit phase
// provided by transaction.
//
// When no unit of work is active, the originating write has already
// auto-committed, so the publications are dispatched immediately instead.
// A Publisher is immutable after construction and safe for concurrent use.
type Publisher struct {
	registry     *Registry
	querier      QuerierSource
	asynchronous bool
	timeout      time.Duration
}

// NewPublisher constructs a Publisher that writes through the querier source
// and dispatches synchronously after commit.
func NewPublisher(registry *Registry, querier QuerierSource) *Publisher {
	return &Publisher{registry: registry, querier: querier, timeout: dispatchTimeout}
}

// WithAsynchronousDispatch returns a Publisher that hands committed
// publications to a background goroutine, so callers in a request path do not
// wait for slow listeners. The at-least-once guarantee is unchanged:
// publications settle only after their listener succeeds, and incomplete ones
// are recovered through ResubmitIncomplete.
func (p *Publisher) WithAsynchronousDispatch() *Publisher {
	configured := *p
	configured.asynchronous = true
	return &configured
}

// Publish stores one publication per listener through the current querier and
// schedules their delivery for after the outermost transaction commits. When
// no unit of work is active the rows are already durable, so delivery happens
// at once. Delivery failures do not fail the call: the affected publications
// stay incomplete and are recovered through ResubmitIncomplete.
func (p *Publisher) Publish(ctx context.Context, event any) error {
	publications, err := p.registry.Publish(ctx, p.querier.Querier(ctx), event)
	if err != nil {
		return err
	}
	if len(publications) == 0 {
		return nil
	}

	dispatch := func(ctx context.Context) { p.dispatch(ctx, publications) }
	if transaction.RegisterAfterCommit(ctx, dispatch) {
		return nil
	}
	dispatch(ctx)
	return nil
}

func (p *Publisher) dispatch(ctx context.Context, publications []Publication) {
	if p.asynchronous {
		// The dispatch context is detached from the caller, so cancelling the
		// request that produced the event does not abort its delivery.
		dispatchContext, cancel := context.WithTimeout(context.WithoutCancel(ctx), p.timeout)
		go func() {
			defer cancel()
			p.report(dispatchContext, publications)
		}()
		return
	}
	p.report(ctx, publications)
}

func (p *Publisher) report(ctx context.Context, publications []Publication) {
	if err := p.registry.Dispatch(ctx, publications...); err != nil {
		slog.Warn("event delivery failed; publications remain incomplete for resubmission", "error", err)
	}
}
