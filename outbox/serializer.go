package outbox

import (
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sync"
)

var (
	// ErrUnknownEventType reports a serialization or deserialization of an
	// event type that was never registered.
	ErrUnknownEventType = errors.New("event type is not registered")

	// ErrConflictingRegistration reports a second registration that binds
	// an already used name or type to something different.
	ErrConflictingRegistration = errors.New("conflicting event type registration")
)

// Serializer converts events to and from their persistent representation.
type Serializer interface {
	Serialize(event any) (eventType string, payload string, err error)
	Deserialize(eventType string, payload string) (event any, err error)
}

// JSONSerializer is a Serializer based on encoding/json. Event types must be
// registered with RegisterEventType under a stable name, which decouples the
// persisted representation from Go type names across refactorings.
type JSONSerializer struct {
	mu        sync.RWMutex
	names     map[reflect.Type]string
	factories map[string]func(payload []byte) (any, error)
}

var _ Serializer = (*JSONSerializer)(nil)

// NewJSONSerializer constructs a JSONSerializer with no registered types.
func NewJSONSerializer() *JSONSerializer {
	return &JSONSerializer{
		names:     make(map[reflect.Type]string),
		factories: make(map[string]func(payload []byte) (any, error)),
	}
}

// RegisterEventType maps the event type T to a stable persistent name.
// Registering the same pair again is allowed; binding a name or a type that
// is already bound differently is rejected, because it would silently
// deserialize stored publications into the wrong type.
func RegisterEventType[T any](serializer *JSONSerializer, name string) error {
	if name == "" {
		return errors.New("event type name must not be empty")
	}
	eventType := reflect.TypeFor[T]()

	serializer.mu.Lock()
	defer serializer.mu.Unlock()
	if existing, bound := serializer.names[eventType]; bound && existing != name {
		return fmt.Errorf("%w: %s is already registered as %s", ErrConflictingRegistration,
			eventType, existing)
	}
	if _, taken := serializer.factories[name]; taken && serializer.names[eventType] != name {
		return fmt.Errorf("%w: name %s is already bound to another type", ErrConflictingRegistration,
			name)
	}

	serializer.names[eventType] = name
	serializer.factories[name] = func(payload []byte) (any, error) {
		var event T
		if err := json.Unmarshal(payload, &event); err != nil {
			return nil, err
		}
		return event, nil
	}
	return nil
}

// Serialize renders the event as JSON under its registered name.
func (s *JSONSerializer) Serialize(event any) (string, string, error) {
	s.mu.RLock()
	name, known := s.names[reflect.TypeOf(event)]
	s.mu.RUnlock()
	if !known {
		return "", "", fmt.Errorf("%w: %T", ErrUnknownEventType, event)
	}

	payload, err := json.Marshal(event)
	if err != nil {
		return "", "", fmt.Errorf("serialize %s: %w", name, err)
	}
	return name, string(payload), nil
}

// Deserialize reconstructs the event registered under the given name.
func (s *JSONSerializer) Deserialize(eventType string, payload string) (any, error) {
	s.mu.RLock()
	factory, known := s.factories[eventType]
	s.mu.RUnlock()
	if !known {
		return nil, fmt.Errorf("%w: %s", ErrUnknownEventType, eventType)
	}

	event, err := factory([]byte(payload))
	if err != nil {
		return nil, fmt.Errorf("deserialize %s: %w", eventType, err)
	}
	return event, nil
}
