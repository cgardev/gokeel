package sqlbus

import (
	"context"
	"errors"
	"log/slog"
	"math/rand/v2"
	"time"
)

const (
	defaultPollInterval        = time.Second
	defaultBatchSize           = 100
	defaultMaintenanceInterval = time.Minute
	defaultSettledRetention    = 72 * time.Hour
	defaultMaximumMessageAge   = 30 * 24 * time.Hour
	defaultConsumerExpiry      = 15 * time.Minute
)

// Dispatcher is the per-node delivery loop: it materializes the delivery rows
// of the listeners attached on its bridge, claims the due ones, delivers them
// through the local in-process bus, and settles the outcome. It also runs the
// shared maintenance duties — consumer heartbeats, broadcast expiry,
// retention, and orphan cleanup — on a slower cadence, so no separately
// managed component guards the tables against unbounded growth.
//
// Every node that hosts listeners must run one Dispatcher. Polling is the
// correctness mechanism; a wake signal only shortens the latency of a pass.
type Dispatcher struct {
	bridge              *Bridge
	pollInterval        time.Duration
	batchSize           int
	wake                <-chan struct{}
	maintenanceInterval time.Duration
	settledRetention    time.Duration
	maximumMessageAge   time.Duration
	consumerExpiry      time.Duration
}

// DispatcherOption customizes a Dispatcher at construction time.
type DispatcherOption func(*Dispatcher)

// WithPollInterval overrides how often the dispatcher runs a delivery pass
// (default 1 second). The interval is jittered by ±10 percent so the nodes of
// a cluster spread their load instead of polling in step.
func WithPollInterval(interval time.Duration) DispatcherOption {
	return func(d *Dispatcher) {
		if interval > 0 {
			d.pollInterval = interval
		}
	}
}

// WithBatchSize overrides how many due deliveries one pass claims per
// attached listener before looking again (default 100).
func WithBatchSize(size int) DispatcherOption {
	return func(d *Dispatcher) {
		if size > 0 {
			d.batchSize = size
		}
	}
}

// WithWakeSignal makes a receive on the channel trigger an immediate pass, so
// a caller can wire a latency hint such as PostgreSQL LISTEN/NOTIFY. The
// signal is strictly best-effort: a lost wake-up costs at most one poll
// interval, never an event.
func WithWakeSignal(wake <-chan struct{}) DispatcherOption {
	return func(d *Dispatcher) {
		if wake != nil {
			d.wake = wake
		}
	}
}

// WithMaintenanceInterval overrides how often the maintenance duties run
// (default 1 minute).
func WithMaintenanceInterval(interval time.Duration) DispatcherOption {
	return func(d *Dispatcher) {
		if interval > 0 {
			d.maintenanceInterval = interval
		}
	}
}

// WithSettledRetention overrides how long a fully settled message is kept
// before retention removes it (default 72 hours). Configure the same value on
// every node.
func WithSettledRetention(retention time.Duration) DispatcherOption {
	return func(d *Dispatcher) {
		if retention > 0 {
			d.settledRetention = retention
		}
	}
}

// WithMaximumMessageAge overrides the hard age cap after which a message is
// removed even when unsettled deliveries still pin it (default 30 days). The
// cap bounds table growth when a dead letter or an abandoned consumer would
// otherwise pin messages forever; every forced removal is reported loudly.
// Configure the same value on every node.
func WithMaximumMessageAge(age time.Duration) DispatcherOption {
	return func(d *Dispatcher) {
		if age > 0 {
			d.maximumMessageAge = age
		}
	}
}

// WithConsumerExpiry overrides how long a broadcast consumer may miss its
// heartbeats before it is considered gone and reaped (default 15 minutes).
// It must dwarf the worst scheduling or database stall a live node can
// suffer, or a paused node is wrongly reaped and re-registers with a fresh
// boundary. Configure the same value on every node.
func WithConsumerExpiry(expiry time.Duration) DispatcherOption {
	return func(d *Dispatcher) {
		if expiry > 0 {
			d.consumerExpiry = expiry
		}
	}
}

// NewDispatcher constructs a Dispatcher over the bridge.
func NewDispatcher(bridge *Bridge, options ...DispatcherOption) *Dispatcher {
	dispatcher := &Dispatcher{
		bridge:              bridge,
		pollInterval:        defaultPollInterval,
		batchSize:           defaultBatchSize,
		maintenanceInterval: defaultMaintenanceInterval,
		settledRetention:    defaultSettledRetention,
		maximumMessageAge:   defaultMaximumMessageAge,
		consumerExpiry:      defaultConsumerExpiry,
	}
	for _, option := range options {
		option(dispatcher)
	}
	return dispatcher
}

// Start launches the background loop: one pass immediately, which picks up
// the backlog of a previous run, then one pass per jittered interval or wake
// signal. The returned stop function cancels the loop and waits for an
// in-flight pass to finish; a delivery in flight at that moment still settles
// its outcome.
func (d *Dispatcher) Start() (stop func()) {
	ctx, cancel := context.WithCancel(context.Background())
	done := make(chan struct{})

	go func() {
		defer close(done)
		var lastMaintenance time.Time
		for {
			lastMaintenance = d.pass(ctx, lastMaintenance)
			timer := time.NewTimer(d.jitteredInterval())
			select {
			case <-ctx.Done():
				timer.Stop()
				return
			case <-timer.C:
			case <-d.wake:
				timer.Stop()
			}
		}
	}()

	return func() {
		cancel()
		<-done
	}
}

// jitteredInterval spreads the polls of a cluster by ±10 percent.
func (d *Dispatcher) jitteredInterval() time.Duration {
	factor := 0.9 + 0.2*rand.Float64()
	return time.Duration(float64(d.pollInterval) * factor)
}

// pass runs one delivery round over every attached listener, and the
// maintenance duties when their cadence is due. It returns the time of the
// last maintenance run.
func (d *Dispatcher) pass(ctx context.Context, lastMaintenance time.Time) time.Time {
	for _, attached := range d.bridge.snapshotAttachments() {
		if ctx.Err() != nil {
			return lastMaintenance
		}
		if err := d.processAttachment(ctx, attached); err != nil {
			slog.Warn("dispatch pass failed; deliveries remain incomplete for redelivery",
				"listener", attached.id, "error", err)
		}
	}

	now := time.Now().UTC()
	if now.Sub(lastMaintenance) < d.maintenanceInterval {
		return lastMaintenance
	}
	d.maintain(ctx, now)
	return now
}

// processAttachment materializes the missing delivery rows of the consumer,
// advances its frontier, and delivers every due delivery.
func (d *Dispatcher) processAttachment(ctx context.Context, attached attachment) error {
	instance := d.bridge.instanceFor(attached.mode)
	key := ConsumerKey{ListenerID: attached.id, Instance: instance, EventType: attached.eventType}

	// The frontier ceiling is captured before materialization runs: it may
	// only cover ground the scan has already seen, and the grace subtraction
	// keeps publications of still-open transactions ahead of the frontier.
	ceiling := time.Now().UTC().Add(-d.bridge.materializationGrace)
	if _, err := d.bridge.store.MaterializeDeliveries(ctx, key); err != nil {
		return err
	}
	if err := d.bridge.store.AdvanceFrontier(ctx, key, ceiling); err != nil {
		return err
	}

	// The batch loop is bounded so one pass cannot spin on a backlog that
	// keeps re-becoming due (for example, instant retries against a failing
	// listener); whatever remains is picked up by the next pass.
	const maximumBatchesPerPass = 16
	var failures []error
	for batch := 0; batch < maximumBatchesPerPass; batch++ {
		now := time.Now().UTC()
		due, err := d.bridge.store.FindDueDeliveries(ctx, attached.id, instance,
			now, now.Add(-d.bridge.leaseDuration), d.batchSize)
		if err != nil {
			failures = append(failures, err)
			break
		}
		if len(due) == 0 {
			break
		}
		for _, work := range due {
			if ctx.Err() != nil {
				return errors.Join(failures...)
			}
			restore := func() (any, error) {
				return d.bridge.serializer.Deserialize(work.Message.EventType, work.Message.SerializedEvent)
			}
			if err := d.bridge.dispatchDelivery(ctx, work.Delivery.Key, work.Delivery.Attempts, restore); err != nil {
				failures = append(failures, err)
			}
		}
		if len(due) < d.batchSize {
			break
		}
	}
	return errors.Join(failures...)
}

// maintain runs the shared background duties. Failures are reported and left
// for the next cadence; every duty is idempotent.
func (d *Dispatcher) maintain(ctx context.Context, now time.Time) {
	for _, attached := range d.bridge.snapshotAttachments() {
		key := ConsumerKey{
			ListenerID: attached.id,
			Instance:   d.bridge.instanceFor(attached.mode),
			EventType:  attached.eventType,
		}
		alive, err := d.bridge.store.Heartbeat(ctx, key, now)
		if err != nil {
			slog.Warn("consumer heartbeat failed", "listener", attached.id, "error", err)
			continue
		}
		if !alive {
			// The registration was reaped, for example after a stall longer
			// than the consumer expiry. Re-register with a freshly computed
			// boundary instead of resurrecting the stale one, so the consumer
			// resumes with a bounded gap rather than a full replay.
			boundary := now.Add(-d.bridge.materializationGrace)
			if err := d.bridge.store.RegisterConsumer(ctx, Consumer{
				ListenerID:       attached.id,
				Instance:         key.Instance,
				EventType:        attached.eventType,
				DeliveryMode:     attached.mode,
				StartBoundary:    boundary,
				Frontier:         boundary,
				RegistrationDate: now,
				HeartbeatDate:    now,
			}); err != nil {
				slog.Warn("consumer re-registration failed", "listener", attached.id, "error", err)
			}
		}
	}

	if _, err := d.bridge.store.ExpireBroadcastConsumers(ctx, now.Add(-d.consumerExpiry)); err != nil {
		slog.Warn("broadcast consumer expiry failed", "error", err)
	}
	if _, err := d.bridge.store.DeleteSettledMessages(ctx, now.Add(-d.settledRetention)); err != nil {
		slog.Warn("settled message retention failed", "error", err)
	}
	forced, err := d.bridge.store.DeleteMessagesOlderThan(ctx, now.Add(-d.maximumMessageAge))
	if err != nil {
		slog.Warn("maximum message age enforcement failed", "error", err)
	} else if forced > 0 {
		slog.Warn("force-removed messages past the maximum age before they were fully settled",
			"count", forced, "maximumMessageAge", d.maximumMessageAge)
	}
	// Orphan deliveries are swept only after message deletion: removing a
	// delivery row while its message survives would resurrect the message
	// through the materialization anti-join.
	if _, err := d.bridge.store.DeleteOrphanDeliveries(ctx); err != nil {
		slog.Warn("orphan delivery cleanup failed", "error", err)
	}
}
