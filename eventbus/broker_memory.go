package eventbus

import (
	"context"
	"errors"
	"fmt"
	"strconv"
	"sync"
	"time"
)

// ErrBrokerStopped reports an operation against a memory broker whose Stop
// was already called.
var ErrBrokerStopped = errors.New("memory broker is stopped")

// MemoryBroker is the in-process Broker engine: every consumer owns a queue
// in memory and nothing survives the process. Delivery is exactly once per
// consumer for the lifetime of the process; there is no crash recovery, which
// is the concern the persistent engines add behind the same contract.
//
// The broker composes the synchronous Bus of this package as its delivery
// fabric, so handler panics are recovered per delivery and cannot take a
// queue worker down. A MemoryBroker is safe for concurrent use.
type MemoryBroker struct {
	bus    *Bus
	ctx    context.Context
	cancel context.CancelFunc
	wait   sync.WaitGroup

	mu                 sync.Mutex
	consumers          map[ListenerID]*memoryConsumer
	deadLetters        []DeadLetter
	deadLetterSequence int64
	stopped            bool
}

// NewMemoryBroker constructs an empty MemoryBroker.
func NewMemoryBroker() *MemoryBroker {
	ctx, cancel := context.WithCancel(context.Background())
	return &MemoryBroker{
		bus:       NewBus(),
		ctx:       ctx,
		cancel:    cancel,
		consumers: make(map[ListenerID]*memoryConsumer),
	}
}

var _ Broker = (*MemoryBroker)(nil)

// memoryConsumer is the in-memory state of one consumer: its ready queue and
// the condition its workers block on. Deliveries waiting out a retry delay
// live outside the queue (FIFO consumers wait in the worker itself, unordered
// ones re-enqueue after their backoff), so the queue holds only ready work.
// The condition variable makes every enqueue observable: a lossy wakeup
// channel could strand ready events behind a busy worker while its siblings
// sleep.
type memoryConsumer struct {
	id            ListenerID
	configuration ConsumerConfiguration

	mu      sync.Mutex
	ready   *sync.Cond
	queue   []*memoryDelivery
	stopped bool
}

func newMemoryConsumer(id ListenerID, configuration ConsumerConfiguration) *memoryConsumer {
	consumer := &memoryConsumer{id: id, configuration: configuration}
	consumer.ready = sync.NewCond(&consumer.mu)
	return consumer
}

// memoryDelivery is one event queued for one consumer.
type memoryDelivery struct {
	event           any
	attempts        int
	publicationDate time.Time
}

// enqueue inserts the delivery at its position in publication order and wakes
// one blocked worker. Published events carry ascending dates, so the common
// case appends; a revived dead letter re-enters at the position its original
// publication had, matching how the persistent engines order resubmissions.
func (c *memoryConsumer) enqueue(delivery *memoryDelivery) {
	c.mu.Lock()
	position := len(c.queue)
	for position > 0 && delivery.publicationDate.Before(c.queue[position-1].publicationDate) {
		position--
	}
	c.queue = append(c.queue, nil)
	copy(c.queue[position+1:], c.queue[position:])
	c.queue[position] = delivery
	c.mu.Unlock()
	c.ready.Signal()
}

// await blocks until the queue holds ready work and pops its head, reporting
// false when the consumer stopped instead.
func (c *memoryConsumer) await() (*memoryDelivery, bool) {
	c.mu.Lock()
	defer c.mu.Unlock()
	for len(c.queue) == 0 && !c.stopped {
		c.ready.Wait()
	}
	if c.stopped {
		return nil, false
	}
	head := c.queue[0]
	c.queue = c.queue[1:]
	return head, true
}

// stop wakes every blocked worker into their exit path.
func (c *memoryConsumer) stop() {
	c.mu.Lock()
	c.stopped = true
	c.mu.Unlock()
	c.ready.Broadcast()
}

// Subscribe registers the consumer and starts its workers: one for a FIFO
// consumer, Configuration.Workers for an unordered one.
func (b *MemoryBroker) Subscribe(_ context.Context, registration ConsumerRegistration) error {
	consumer := newMemoryConsumer(registration.ID, registration.Configuration)
	workers := 1
	if registration.Configuration.Ordering == OrderingUnordered {
		workers = registration.Configuration.Workers
	}

	// The stopped check, the registrations, and the worker accounting share
	// one critical section: an Add outside it could start from zero
	// concurrently with Stop's Wait, which the WaitGroup contract forbids.
	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		return fmt.Errorf("subscribe %s: %w", registration.ID, ErrBrokerStopped)
	}
	if err := b.bus.Subscribe(registration.ID, registration.Matches, registration.Handle); err != nil {
		b.mu.Unlock()
		return err
	}
	b.consumers[registration.ID] = consumer
	b.wait.Add(workers)
	b.mu.Unlock()

	for count := 0; count < workers; count++ {
		go func() {
			defer b.wait.Done()
			b.work(consumer)
		}()
	}
	return nil
}

// Publish hands the event to the queue of every matching consumer. It never
// waits for handlers: outcomes settle asynchronously through the per-consumer
// retry machinery.
func (b *MemoryBroker) Publish(_ context.Context, event any) error {
	now := time.Now().UTC()

	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		return ErrBrokerStopped
	}
	var targets []*memoryConsumer
	for _, id := range b.bus.ListenersFor(event) {
		if consumer, ok := b.consumers[id]; ok {
			targets = append(targets, consumer)
		}
	}
	b.mu.Unlock()

	for _, consumer := range targets {
		consumer.enqueue(&memoryDelivery{event: event, publicationDate: now})
	}
	return nil
}

// work is one queue worker: it blocks until the consumer has ready work,
// processes one delivery to its terminal state, and repeats until the broker
// stops. A FIFO consumer runs exactly one worker, so waiting out a retry
// delay inside the worker is precisely the head-of-line blocking the ordering
// promises; unordered consumers re-enqueue retries after their backoff and
// move on.
func (b *MemoryBroker) work(consumer *memoryConsumer) {
	for {
		delivery, ok := consumer.await()
		if !ok {
			return
		}
		if b.ctx.Err() != nil {
			return
		}
		b.process(consumer, delivery)
	}
}

// process runs one delivery until it settles: completed, parked as a dead
// letter, scheduled for an unordered retry, or abandoned because the broker
// stopped.
func (b *MemoryBroker) process(consumer *memoryConsumer, delivery *memoryDelivery) {
	for {
		delivery.attempts++
		err := b.bus.Deliver(b.ctx, consumer.id, delivery.event)
		if err == nil {
			return
		}
		if delivery.attempts >= consumer.configuration.MaximumAttempts {
			b.park(consumer, delivery, err)
			return
		}

		delay := consumer.configuration.RetryDelay(delivery.attempts)
		if consumer.configuration.Ordering == OrderingUnordered {
			// The retry waits in a timer, not in the worker, so the other
			// events of the consumer keep flowing during the backoff.
			b.scheduleRetry(consumer, delivery, delay)
			return
		}
		if !b.sleep(delay) {
			return
		}
	}
}

// scheduleRetry re-enqueues the delivery once its backoff elapses. The wait
// runs in a goroutine that lives exactly as long as the backoff, so pending
// retries never accumulate parked goroutines; a retry interrupted by Stop is
// dropped, like every other in-memory delivery. The accounting is safe
// because scheduleRetry runs on a worker goroutine, whose own count keeps the
// wait group above zero.
func (b *MemoryBroker) scheduleRetry(consumer *memoryConsumer, delivery *memoryDelivery, delay time.Duration) {
	b.wait.Add(1)
	go func() {
		defer b.wait.Done()
		if !b.sleep(delay) {
			return
		}
		consumer.enqueue(delivery)
	}()
}

// sleep waits for the duration, reporting false when the broker stopped
// first.
func (b *MemoryBroker) sleep(delay time.Duration) bool {
	timer := time.NewTimer(delay)
	defer timer.Stop()
	select {
	case <-b.ctx.Done():
		return false
	case <-timer.C:
		return true
	}
}

// park records the delivery as a dead letter, inspectable through
// FindExhausted and revivable through Resubmit.
func (b *MemoryBroker) park(consumer *memoryConsumer, delivery *memoryDelivery, cause error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	b.deadLetterSequence++
	b.deadLetters = append(b.deadLetters, DeadLetter{
		Reference:       strconv.FormatInt(b.deadLetterSequence, 10),
		ListenerID:      consumer.id,
		Event:           delivery.event,
		Attempts:        delivery.attempts,
		LastError:       cause.Error(),
		PublicationDate: delivery.publicationDate,
	})
}

// FindExhausted returns the dead letters, oldest first, up to the limit.
func (b *MemoryBroker) FindExhausted(_ context.Context, limit int) ([]DeadLetter, error) {
	b.mu.Lock()
	defer b.mu.Unlock()
	count := len(b.deadLetters)
	if limit > 0 && limit < count {
		count = limit
	}
	letters := make([]DeadLetter, count)
	copy(letters, b.deadLetters[:count])
	return letters, nil
}

// Resubmit removes the referenced dead letter and re-enqueues its event with
// a fresh attempt budget at the position its original publication order
// dictates, matching how the persistent engines order resubmissions. It
// reports false when the reference is unknown.
func (b *MemoryBroker) Resubmit(_ context.Context, reference string) (bool, error) {
	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		return false, ErrBrokerStopped
	}
	index := -1
	for position, letter := range b.deadLetters {
		if letter.Reference == reference {
			index = position
			break
		}
	}
	if index == -1 {
		b.mu.Unlock()
		return false, nil
	}
	letter := b.deadLetters[index]
	b.deadLetters = append(b.deadLetters[:index], b.deadLetters[index+1:]...)
	consumer := b.consumers[letter.ListenerID]
	b.mu.Unlock()

	if consumer == nil {
		return false, nil
	}
	consumer.enqueue(&memoryDelivery{
		event:           letter.Event,
		publicationDate: letter.PublicationDate,
	})
	return true, nil
}

// Stop cancels the workers, wakes every one blocked on an empty queue, and
// waits for the in-flight deliveries to return. Queued events and pending
// retries are dropped: the memory engine carries no durability by design.
func (b *MemoryBroker) Stop() {
	b.mu.Lock()
	if b.stopped {
		b.mu.Unlock()
		return
	}
	b.stopped = true
	consumers := make([]*memoryConsumer, 0, len(b.consumers))
	for _, consumer := range b.consumers {
		consumers = append(consumers, consumer)
	}
	b.mu.Unlock()

	b.cancel()
	for _, consumer := range consumers {
		consumer.stop()
	}
	b.wait.Wait()
}
