// Package logging provides hierarchical, dynamically adjustable log levels
// for the standard library log/slog and log packages: the Go analog of the
// logging.level configuration tree of Spring Boot. A Manager owns a tree of
// named levels in which every logger inherits the level of its nearest
// configured ancestor, so one assignment on a package path governs every
// logger beneath it, down to a single type.
//
// Logger names are hierarchical. Segments are separated by "/" and ".", so a
// Go package import path such as "github.com/cgardev/gokeel/outbox" and a
// package-qualified type name such as "github.com/cgardev/gokeel/outbox.Store"
// form the same kind of tree Spring builds from dotted class names. The
// effective level of a name is the level assigned to its longest configured
// whole-segment prefix, falling back to the root level, which is Logback's
// effective-level rule. The pseudo-name "root" addresses the root logger, as
// it does in Spring.
//
// Levels are assigned at three moments that mirror the precedence of Spring:
// construction options declare the levels a program compiles in, Apply
// overlays an externally provided Configuration document over them, and
// SetLevel and ResetLevel adjust a running program the way the loggers
// endpoint of Spring Boot Actuator does. A later assignment overrides an
// earlier one, which is exactly how the externalized configuration of Spring
// overrides the levels its logging file declares.
//
// A Manager is safe for concurrent use. The level tree is an immutable
// snapshot behind an atomic pointer, following the standard library's
// slog.LevelVar, so the level check on the logging hot path takes no lock.
//
// The package depends only on the standard library.
package logging

import (
	"log"
	"log/slog"
	"maps"
	"os"
	"slices"
	"sync"
	"sync/atomic"
)

// Manager owns a hierarchical tree of log levels and hands out slog.Logger
// instances bound to names inside it. It plays the role of Spring's
// LoggingSystem: the single place where levels are assigned, inherited,
// overridden, and inspected. A Manager is safe for concurrent use.
type Manager struct {
	// delegate and nameKey are immutable after NewManager returns, so
	// Handler reads them without locking.
	delegate slog.Handler
	nameKey  string

	// initialRootLevel is the root level the Manager was constructed
	// with, the value ResetLevel restores on the root logger.
	initialRootLevel slog.Level

	// state holds the current immutable levelTree. Readers load it with
	// one atomic operation; writers rebuild a copy under mutex and swap
	// the pointer, so the logging hot path never takes a lock.
	state atomic.Pointer[levelTree]

	// mutex serializes writers of state and guards groups and known.
	mutex  sync.Mutex
	groups map[string][]string
	known  map[string]struct{}
}

// levelTree is one immutable snapshot of the level assignments. The root
// always has a level, which guarantees the effective-level walk terminates,
// the same invariant Logback maintains for its root logger.
type levelTree struct {
	root     slog.Level
	assigned map[string]slog.Level
}

// effective resolves the first assigned level along the chain of a name and
// its ancestors, falling back to the root level: Logback's effective-level
// rule.
func (tree *levelTree) effective(chain []string) slog.Level {
	for _, name := range chain {
		if level, ok := tree.assigned[name]; ok {
			return level
		}
	}
	return tree.root
}

// Option customizes a Manager at construction time.
type Option func(*Manager)

// WithHandler sets the slog.Handler every logger of the Manager emits
// through. The level tree is the single filtering authority: the delegate's
// own level, if it has one, is never consulted, so construct the delegate
// with the lowest level it should ever emit. The default delegate writes
// text to standard error, where the Go standard library logs by default: the
// analog of the console appender a Spring Boot application starts with. A
// nil handler is ignored.
func WithHandler(handler slog.Handler) Option {
	return func(manager *Manager) {
		if handler != nil {
			manager.delegate = handler
		}
	}
}

// WithRootLevel assigns the root level, the floor every name falls back to
// when no ancestor is configured. The default is slog.LevelInfo, the root
// level of a Spring Boot application. The value the Manager holds when
// construction finishes also becomes the level ResetLevel restores on the
// root logger.
func WithRootLevel(level slog.Level) Option {
	return func(manager *Manager) {
		manager.SetLevel("root", level)
	}
}

// WithLevel assigns a level to one name at construction time, the analog of
// a logger element compiled into a Logback file. A later Apply, SetLevel, or
// ResetLevel overrides it, which is exactly the precedence Spring gives
// externalized configuration over the levels its logging file declares.
func WithLevel(name string, level slog.Level) Option {
	return func(manager *Manager) {
		manager.SetLevel(name, level)
	}
}

// WithGroup declares a named group of loggers at construction time, the
// analog of the logging.group properties of Spring Boot; it is unrelated to
// the attribute grouping of slog.Handler.WithGroup. Assigning a level to the
// group name fans it out to every member, and a group with members shadows a
// logger of the same name, as it does in Spring. Members that designate no
// logger are ignored, and a group cannot be named after the root logger.
func WithGroup(name string, members ...string) Option {
	return func(manager *Manager) {
		canonical := canonicalName(name)
		if canonical == "" {
			return
		}
		canonicalMembers := make([]string, 0, len(members))
		for _, member := range members {
			if member := canonicalName(member); member != "" {
				canonicalMembers = append(canonicalMembers, member)
			}
		}
		manager.groups[canonical] = canonicalMembers
	}
}

// WithNameKey sets the attribute key under which every logger records its
// own name, making the emitting logger visible in the output the way the
// logger name column of a Spring log line is. The default key is "logger";
// the empty string disables the attribute.
func WithNameKey(key string) Option {
	return func(manager *Manager) {
		manager.nameKey = key
	}
}

// NewManager constructs a Manager with the analog of the defaults a fresh
// Spring Boot application boots with: a root level of slog.LevelInfo and a
// console text delegate, writing to standard error as the Go standard
// library does by default. Options are applied in the order given, so a
// group is declared before a level is assigned to its name.
func NewManager(options ...Option) *Manager {
	manager := &Manager{
		// The delegate is constructed at LevelTrace because the level
		// tree, not the delegate, decides what is logged.
		delegate: slog.NewTextHandler(os.Stderr, &slog.HandlerOptions{Level: LevelTrace}),
		nameKey:  "logger",
		groups:   map[string][]string{},
		known:    map[string]struct{}{},
	}
	manager.state.Store(&levelTree{root: slog.LevelInfo, assigned: map[string]slog.Level{}})
	for _, option := range options {
		option(manager)
	}
	manager.initialRootLevel = manager.state.Load().root
	return manager
}

// Logger returns a slog.Logger bound to the hierarchical name, typically a
// package import path. The logger observes level changes made after it was
// created, so a long-lived logger stored in a struct is retuned by a later
// Apply or SetLevel the way a running Spring application is retuned through
// the actuator.
func (manager *Manager) Logger(name string) *slog.Logger {
	return slog.New(manager.Handler(name))
}

// Handler returns a slog.Handler bound to the hierarchical name. The handler
// consults the level tree on every call, so a later Apply or SetLevel is
// visible to handlers already handed out. When the name attribute is enabled
// the delegate carries it preformatted, adding no cost per record.
func (manager *Manager) Handler(name string) slog.Handler {
	canonical := canonicalName(name)
	manager.remember(canonical)
	delegate := manager.delegate
	if manager.nameKey != "" {
		delegate = delegate.WithAttrs([]slog.Attr{slog.String(manager.nameKey, displayName(canonical))})
	}
	return &levelHandler{
		manager:  manager,
		delegate: delegate,
		chain:    ancestry(canonical),
	}
}

// StandardLogger returns a standard library log.Logger that forwards every
// message to the named logger at the given level, the bridge for code that
// still speaks the classic log API. The bridge honors the level tree: when
// the effective level of the name silences the given level, messages are
// discarded before formatting. The process-global standard logger is routed
// through the Manager by passing the bridge writer on:
//
//	bridge := manager.StandardLogger("legacy", slog.LevelInfo)
//	log.SetOutput(bridge.Writer())
//	log.SetFlags(0)
//
// Clearing the flags matters: the delegate stamps records itself, so the
// standard logger's own date and time prefix would otherwise be duplicated
// inside every message.
func (manager *Manager) StandardLogger(name string, level slog.Level) *log.Logger {
	return slog.NewLogLogger(manager.Handler(name), level)
}

// SetLevel assigns the level of the name for every logger already created
// and every logger created afterwards, the runtime mutation the loggers
// endpoint of Spring Boot Actuator performs. When the name designates a
// group with members the level fans out to the members and the group name
// itself stays unconfigured, matching Spring. The pseudo-name "root" assigns
// the root level.
func (manager *Manager) SetLevel(name string, level slog.Level) {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	manager.rebuild(func(tree *levelTree) {
		for _, target := range manager.expand(name) {
			if target == "" {
				tree.root = level
			} else {
				tree.assigned[target] = level
			}
		}
	})
}

// ResetLevel clears the assigned level of the name so it inherits from its
// nearest configured ancestor again, the effect of writing a null
// configuredLevel through Spring Boot Actuator. Resetting a group clears
// every member. Resetting the root logger restores the level the Manager was
// constructed with; Logback instead rejects clearing the root level, and the
// divergence keeps the operation total while preserving the invariant that
// the root always has a level.
func (manager *Manager) ResetLevel(name string) {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	manager.rebuild(func(tree *levelTree) {
		for _, target := range manager.expand(name) {
			if target == "" {
				tree.root = manager.initialRootLevel
			} else {
				delete(tree.assigned, target)
			}
		}
	})
}

// EffectiveLevel resolves the level the hierarchy yields for the name: the
// level of its nearest configured ancestor, or the root level when no
// ancestor is configured. The pseudo-name "root" resolves the root level.
func (manager *Manager) EffectiveLevel(name string) slog.Level {
	return manager.state.Load().effective(ancestry(name))
}

// ConfiguredLevel returns the level assigned directly to the name and
// reports whether one is assigned at all; the root logger always has one.
func (manager *Manager) ConfiguredLevel(name string) (slog.Level, bool) {
	tree := manager.state.Load()
	canonical := canonicalName(name)
	if canonical == "" {
		return tree.root, true
	}
	level, ok := tree.assigned[canonical]
	return level, ok
}

// LoggerLevels describes one logger the way one entry of the loggers
// endpoint of Spring Boot Actuator does: the level explicitly assigned to
// the name, if any, and the effective level the hierarchy resolves for it.
type LoggerLevels struct {
	// Name is the canonical logger name; the root logger reads "root".
	Name string

	// Configured is the level assigned directly to the name. It is
	// meaningful only when IsConfigured is true.
	Configured slog.Level

	// IsConfigured reports whether a level is assigned directly to the
	// name rather than inherited.
	IsConfigured bool

	// Effective is the level the hierarchy resolves for the name.
	Effective slog.Level
}

// Loggers lists every logger the Manager knows: each name a Logger or
// Handler call bound plus each name a level is assigned to, sorted by name
// with the root logger first, the inventory the loggers endpoint of Spring
// Boot Actuator exposes.
func (manager *Manager) Loggers() []LoggerLevels {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	tree := manager.state.Load()

	names := make(map[string]struct{}, len(manager.known)+len(tree.assigned))
	for name := range manager.known {
		names[name] = struct{}{}
	}
	for name := range tree.assigned {
		names[name] = struct{}{}
	}

	loggers := make([]LoggerLevels, 0, len(names)+1)
	loggers = append(loggers, LoggerLevels{
		Name:         "root",
		Configured:   tree.root,
		IsConfigured: true,
		Effective:    tree.root,
	})
	for _, name := range slices.Sorted(maps.Keys(names)) {
		configured, isConfigured := tree.assigned[name]
		loggers = append(loggers, LoggerLevels{
			Name:         name,
			Configured:   configured,
			IsConfigured: isConfigured,
			Effective:    tree.effective(ancestry(name)),
		})
	}
	return loggers
}

// Groups returns the declared groups and their members, the way the loggers
// endpoint of Spring Boot Actuator lists them. The result is a copy;
// mutating it does not affect the Manager.
func (manager *Manager) Groups() map[string][]string {
	manager.mutex.Lock()
	defer manager.mutex.Unlock()
	groups := make(map[string][]string, len(manager.groups))
	for name, members := range manager.groups {
		groups[name] = slices.Clone(members)
	}
	return groups
}

// rebuild clones the current snapshot, applies change to the clone, and
// publishes it with one atomic store, so a concurrent reader observes either
// the old tree or the new one, never a partial mutation. The caller must
// hold mutex.
func (manager *Manager) rebuild(change func(tree *levelTree)) {
	current := manager.state.Load()
	next := &levelTree{root: current.root, assigned: maps.Clone(current.assigned)}
	change(next)
	manager.state.Store(next)
}

// expand resolves the canonical targets of an assignment: the members of the
// group the name designates, or the canonical name itself. A group with
// members shadows a logger of the same name, matching Spring. The caller
// must hold mutex.
func (manager *Manager) expand(name string) []string {
	canonical := canonicalName(name)
	if members, ok := manager.groups[canonical]; ok && len(members) > 0 {
		return members
	}
	return []string{canonical}
}

// remember records a name handed out through Handler or Logger so Loggers
// can list it, the way the actuator inventory lists every logger the context
// created.
func (manager *Manager) remember(canonical string) {
	if canonical == "" {
		return
	}
	manager.mutex.Lock()
	manager.known[canonical] = struct{}{}
	manager.mutex.Unlock()
}
