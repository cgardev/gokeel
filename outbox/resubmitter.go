package outbox

import (
	"context"
	"log/slog"
	"time"
)

// Resubmitter periodically re-delivers incomplete event publications, so a
// delivery that failed against a temporarily unavailable collaborator is
// retried while the application runs instead of waiting for a restart.
type Resubmitter struct {
	registry   *Registry
	interval   time.Duration
	minimumAge time.Duration
}

// NewResubmitter constructs a Resubmitter that runs one resubmission pass
// per interval, considering only publications older than minimumAge so it
// does not race against dispatches that are still in flight.
func NewResubmitter(registry *Registry, interval, minimumAge time.Duration) *Resubmitter {
	return &Resubmitter{registry: registry, interval: interval, minimumAge: minimumAge}
}

// Start launches the background loop: one pass immediately, which recovers
// the leftovers of a previous run, then one pass per interval. The returned
// stop function cancels the loop and waits for an in-flight pass to finish.
func (r *Resubmitter) Start() (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		defer close(done)
		ticker := time.NewTicker(r.interval)
		defer ticker.Stop()
		for {
			if err := r.registry.ResubmitIncomplete(ctx, r.minimumAge); err != nil {
				slog.Warn("resubmission of incomplete event publications failed", "error", err)
			}
			select {
			case <-ctx.Done():
				return
			case <-ticker.C:
			}
		}
	}()

	return func() {
		cancel()
		<-done
	}
}
