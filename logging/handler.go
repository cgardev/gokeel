package logging

import (
	"context"
	"log/slog"
)

// levelHandler decorates a delegate slog.Handler with the level tree of its
// Manager. Enabled consults the tree and nothing else, making the tree the
// single authority on what is logged, the way the effective level of a
// Logback logger decides before any appender runs.
type levelHandler struct {
	manager  *Manager
	delegate slog.Handler

	// chain holds the canonical name followed by its ancestors, computed
	// once so a level check performs only map lookups.
	chain []string
}

var _ slog.Handler = (*levelHandler)(nil)

// Enabled reports whether a record at the level passes the effective level
// the current tree snapshot resolves for the handler's name. It takes no
// lock: the snapshot is read through one atomic load.
func (handler *levelHandler) Enabled(_ context.Context, level slog.Level) bool {
	return level >= handler.manager.state.Load().effective(handler.chain)
}

// Handle forwards the record to the delegate. slog.Logger consults Enabled
// before Handle, so a record that reaches this method already passed the
// level tree.
func (handler *levelHandler) Handle(ctx context.Context, record slog.Record) error {
	return handler.delegate.Handle(ctx, record)
}

// WithAttrs returns a handler whose delegate carries the attributes; the
// level decision still follows the name of the receiver.
func (handler *levelHandler) WithAttrs(attributes []slog.Attr) slog.Handler {
	if len(attributes) == 0 {
		return handler
	}
	return &levelHandler{
		manager:  handler.manager,
		delegate: handler.delegate.WithAttrs(attributes),
		chain:    handler.chain,
	}
}

// WithGroup returns a handler whose delegate opens the attribute group; an
// empty name returns the receiver, as the slog.Handler contract requires.
func (handler *levelHandler) WithGroup(name string) slog.Handler {
	if name == "" {
		return handler
	}
	return &levelHandler{
		manager:  handler.manager,
		delegate: handler.delegate.WithGroup(name),
		chain:    handler.chain,
	}
}
