// Package eventbus provides a generic, synchronous in-memory event bus.
// It carries no persistence concern: libraries that need delivery
// guarantees, such as the outbox package, build on top of it.
//
// A Bus is safe for concurrent use. No lock is held while a handler runs,
// so handlers may subscribe or publish reentrantly without deadlocking.
package eventbus

import (
	"context"
	"errors"
	"fmt"
	"runtime/debug"
	"sync"
)

var (
	// ErrDuplicateListener reports a subscription under an identifier that
	// is already taken.
	ErrDuplicateListener = errors.New("listener identifier is already subscribed")

	// ErrUnknownListener reports a delivery towards an identifier with no
	// subscription behind it.
	ErrUnknownListener = errors.New("listener is not subscribed")

	// ErrListenerPanic reports a handler that panicked while processing an
	// event; the panic is recovered and surfaced through this error.
	ErrListenerPanic = errors.New("listener panicked")
)

// ListenerID identifies a subscribed listener.
type ListenerID string

// Handler processes one event delivered to a listener.
type Handler func(ctx context.Context, event any) error

type subscription struct {
	id      ListenerID
	matches func(event any) bool
	handle  Handler
}

// Bus delivers events synchronously to subscribed listeners. Listeners are
// identified by a unique ListenerID, so callers can address one listener
// individually or multicast an event to every matching listener.
type Bus struct {
	mu            sync.RWMutex
	subscriptions []subscription
}

// NewBus constructs an empty Bus.
func NewBus() *Bus {
	return &Bus{}
}

// Subscribe registers a listener under a unique identifier. The matches
// predicate decides which events the listener receives.
func (b *Bus) Subscribe(id ListenerID, matches func(event any) bool, handle Handler) error {
	if id == "" {
		return errors.New("listener identifier must not be empty")
	}
	if matches == nil || handle == nil {
		return fmt.Errorf("subscription of %s requires a matches predicate and a handler", id)
	}

	b.mu.Lock()
	defer b.mu.Unlock()
	for _, existing := range b.subscriptions {
		if existing.id == id {
			return fmt.Errorf("%w: %s", ErrDuplicateListener, id)
		}
	}
	b.subscriptions = append(b.subscriptions, subscription{id: id, matches: matches, handle: handle})
	return nil
}

// SubscribeTo registers a listener that receives every event of type T.
func SubscribeTo[T any](bus *Bus, id ListenerID, handle func(ctx context.Context, event T) error) error {
	return bus.Subscribe(id,
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
}

// Publish multicasts the event to every matching listener, in subscription
// order. The returned error joins the failures of every listener that
// rejected the event; the remaining listeners are still invoked.
func (b *Bus) Publish(ctx context.Context, event any) error {
	var failures []error
	for _, id := range b.ListenersFor(event) {
		if err := b.Deliver(ctx, id, event); err != nil {
			failures = append(failures, err)
		}
	}
	return errors.Join(failures...)
}

// ListenersFor returns the identifiers of every listener subscribed to the
// event, in subscription order.
func (b *Bus) ListenersFor(event any) []ListenerID {
	b.mu.RLock()
	defer b.mu.RUnlock()
	var listeners []ListenerID
	for _, candidate := range b.subscriptions {
		if candidate.matches(event) {
			listeners = append(listeners, candidate.id)
		}
	}
	return listeners
}

// Deliver hands the event to the identified listener. A panicking handler
// is recovered and reported as an error wrapping ErrListenerPanic, so one
// misbehaving listener cannot take down the publishing caller.
func (b *Bus) Deliver(ctx context.Context, id ListenerID, event any) error {
	b.mu.RLock()
	var handle Handler
	for _, candidate := range b.subscriptions {
		if candidate.id == id {
			handle = candidate.handle
			break
		}
	}
	b.mu.RUnlock()

	if handle == nil {
		return fmt.Errorf("%w: %s", ErrUnknownListener, id)
	}
	return invoke(ctx, handle, event)
}

func invoke(ctx context.Context, handle Handler, event any) (err error) {
	defer func() {
		if recovered := recover(); recovered != nil {
			err = fmt.Errorf("%w: %v\n%s", ErrListenerPanic, recovered, debug.Stack())
		}
	}()
	return handle(ctx, event)
}
