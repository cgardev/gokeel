---
title: Logging
description: Constructing a level Manager, binding hierarchical loggers, overriding levels from a Configuration document, and adjusting a running program.
---

The `logging` package provides hierarchical, dynamically adjustable log
levels for the standard library `log/slog` and `log` packages: the Go analog
of the `logging.level` configuration tree of Spring Boot. A `Manager` owns a
tree of named levels in which every logger inherits the level of its nearest
configured ancestor, so one assignment on a package path governs every logger
beneath it. A `Manager` is safe for concurrent use, and the level check on
the logging hot path reads an atomic snapshot, so it takes no lock.

```go
import "github.com/cgardev/gokeel/logging"
```

## NewManager

`NewManager` constructs a `Manager` with the analog of the defaults a fresh
Spring Boot application boots with: a root level of `slog.LevelInfo` and a
console text delegate, writing to standard error as the Go standard library
does by default.

```go
func NewManager(options ...Option) *Manager
```

```go
levels := logging.NewManager(
    logging.WithLevel("github.com/acme/shop", slog.LevelWarn),
)
```

Options are applied in the order given, so a group is declared before a level
is assigned to its name. Throughout this page, `levels` is the `*Manager`
constructed here.

## The options

An `Option` customizes a `Manager` at construction time.

### WithHandler

```go
func WithHandler(handler slog.Handler) Option
```

`WithHandler` sets the `slog.Handler` every logger emits through. The level
tree is the single filtering authority — the delegate's own level, if it has
one, is never consulted — so construct the delegate with the lowest level it
should ever emit. A nil handler is ignored.

```go
logging.WithHandler(slog.NewJSONHandler(os.Stderr,
    &slog.HandlerOptions{Level: logging.LevelTrace}))
```

### WithRootLevel

```go
func WithRootLevel(level slog.Level) Option
```

`WithRootLevel` assigns the root level, the floor every name falls back to
when no ancestor is configured. The default is `slog.LevelInfo`, the root
level of a Spring Boot application; the value also becomes the level
`ResetLevel` restores on the root logger.

```go
logging.WithRootLevel(slog.LevelWarn)
```

### WithLevel

```go
func WithLevel(name string, level slog.Level) Option
```

`WithLevel` assigns a level to one name at construction time, the analog of a
logger element compiled into a Logback file. A later `Apply`, `SetLevel`, or
`ResetLevel` overrides it — exactly the precedence Spring gives externalized
configuration over the levels its logging file declares.

```go
logging.WithLevel("github.com/acme/shop", slog.LevelWarn)
```

### WithGroup

```go
func WithGroup(name string, members ...string) Option
```

`WithGroup` declares a named group of loggers, the analog of Spring Boot's
`logging.group` properties; it is unrelated to the attribute grouping of
`slog.Handler.WithGroup`. Assigning a level to the group name fans it out to
every member, and a group with members shadows a logger of the same name, as
it does in Spring.

```go
logging.WithGroup("persistence",
    "github.com/acme/shop/orders/postgres",
    "github.com/acme/shop/billing/postgres")
```

### WithNameKey

```go
func WithNameKey(key string) Option
```

`WithNameKey` sets the attribute key under which every logger records its own
name. The default key is `logger`; the empty string disables the attribute.

```go
logging.WithNameKey("component")
```

## Manager.Logger and Manager.Handler

`Logger` returns a `*slog.Logger` bound to a hierarchical name, typically a
package import path; `Handler` returns the underlying `slog.Handler` for
callers that compose handlers themselves. Both consult the level tree on
every record, so a later `Apply` or `SetLevel` is visible to loggers already
handed out.

```go
func (manager *Manager) Logger(name string) *slog.Logger
func (manager *Manager) Handler(name string) slog.Handler
```

```go
logger := levels.Logger("github.com/acme/shop/orders")
logger.Info("order placed", "order", "A-100")
```

Names segment on `/` and `.`, so `github.com/acme/shop/orders.Store` sits
under `github.com/acme/shop/orders`. Inheritance cuts whole segments — a
level on `github.com/acme/event` does not reach `github.com/acme/eventbus` —
and the pseudo-name `root` addresses the root logger. When the name attribute
is enabled, the delegate carries it preformatted, adding no cost per record.

## Manager.SetLevel and Manager.ResetLevel

`SetLevel` assigns the level of a name for every logger already created and
every logger created afterwards — the runtime mutation Spring Boot Actuator
performs through its loggers endpoint. `ResetLevel` clears the assignment so
the name inherits from its nearest configured ancestor again, the effect of
writing a null `configuredLevel` through the actuator.

```go
func (manager *Manager) SetLevel(name string, level slog.Level)
func (manager *Manager) ResetLevel(name string)
```

```go
levels.SetLevel("github.com/acme/shop/orders", slog.LevelDebug)
levels.ResetLevel("github.com/acme/shop/orders")
```

When the name designates a group with members, `SetLevel` fans the level out
to the members and the group name itself stays unconfigured; `ResetLevel`
clears every member. Resetting the root restores the level the `Manager` was
constructed with — Logback instead rejects clearing the root level, and the
divergence keeps the operation total while the root always keeps a level.

## Manager.EffectiveLevel and Manager.ConfiguredLevel

`EffectiveLevel` resolves the level the hierarchy yields for a name: the
level of its nearest configured ancestor, or the root level when no ancestor
is configured. `ConfiguredLevel` returns the level assigned directly to the
name and reports whether one is assigned at all; the root always has one. The
pair mirrors the `effectiveLevel` and `configuredLevel` fields of an actuator
loggers entry.

```go
func (manager *Manager) EffectiveLevel(name string) slog.Level
func (manager *Manager) ConfiguredLevel(name string) (slog.Level, bool)
```

```go
effective := levels.EffectiveLevel("github.com/acme/shop/orders/postgres.Store")
configured, ok := levels.ConfiguredLevel("github.com/acme/shop") // ok reports a direct assignment
```

## Configuration

A `Configuration` is the externally provided document that overrides the
levels a program compiled in — the analog of the `logging.level` and
`logging.group` property families. `Levels` assigns a level token to each
logger name, and `Groups` declares named groups so one entry can retune
several packages at once.

```go
type Configuration struct {
    Levels map[string]string   `json:"levels,omitempty"`
    Groups map[string][]string `json:"groups,omitempty"`
}
```

```go
configuration := logging.Configuration{
    Levels: map[string]string{
        "root":                 "warn",
        "github.com/acme/shop": "debug",
    },
    Groups: map[string][]string{
        "persistence": {"github.com/acme/shop/orders/postgres"},
    },
}
```

`Validate` reports the first problem that would prevent the document from
being applied: a level token that does not parse, a group that designates the
root logger, or a group member that names no logger.

## ParseConfiguration and ParseLevels

`ParseConfiguration` decodes a JSON `Configuration` document and validates
it; unknown fields are rejected, so a misspelled key fails loudly instead of
being silently ignored. `ParseLevels` reads the compact `name=level` list an
environment variable or a command-line flag carries; the result plugs
directly into the `Levels` field of a `Configuration`.

```go
func ParseConfiguration(document []byte) (Configuration, error)
func ParseLevels(list string) (map[string]string, error)
```

```go
overrides, err := logging.ParseLevels("root=warn,github.com/acme/shop=debug")
```

An empty list parses to an empty map, so an unset environment variable
applies nothing. Both functions fail with `ErrInvalidLevel` when a token
names no known level.

## Manager.Apply

`Apply` overlays a `Configuration` over the levels the manager currently
holds — the moment Spring Boot applies `logging.level` properties over the
levels its logging file declares. Every name the document configures is
overwritten; every other name keeps its current assignment. Groups the
document declares are registered before any level is applied, and they remain
available to later `SetLevel` and `ResetLevel` calls.

```go
func (manager *Manager) Apply(configuration Configuration) error
```

```go
if err := levels.Apply(configuration); err != nil {
    return err
}
```

Two rules make the outcome deterministic where Spring leaves it to property
iteration order: levels are applied in lexicographic name order, and a level
assigned directly to a name wins over a level the name receives through a
group. The document is validated up front, so a validation failure leaves the
manager untouched.

## Manager.Loggers and Manager.Groups

`Loggers` lists every logger the manager knows — each name a `Logger` or
`Handler` call bound plus each name a level is assigned to — sorted by name
with the root logger first: the inventory the actuator loggers endpoint
exposes. `Groups` returns the declared groups and their members; the result
is a copy, so mutating it does not affect the manager.

```go
func (manager *Manager) Loggers() []LoggerLevels
func (manager *Manager) Groups() map[string][]string
```

```go
type LoggerLevels struct {
    Name         string     // canonical name; the root logger reads "root"
    Configured   slog.Level // meaningful only when IsConfigured is true
    IsConfigured bool       // a level is assigned directly, not inherited
    Effective    slog.Level // the level the hierarchy resolves for the name
}
```

```go
entries := levels.Loggers()
// entries[0].Name == "root"; entries[0].IsConfigured == true
```

## LevelTrace and LevelOff

The package completes slog's ladder with the two levels Spring accepts beyond
it, declared as ordinary `slog.Level` values:

```go
const (
    LevelTrace slog.Level = slog.LevelDebug - 4 // one spacing step below DEBUG
    LevelOff   slog.Level = math.MaxInt32       // no record carries a level this high
)
```

`LevelTrace` occupies the position TRACE holds below DEBUG in Logback, and
`LevelOff` mirrors Logback's OFF, which is `Integer.MAX_VALUE`: assigning it
silences a logger entirely, even for errors.

## ParseLevel and LevelName

`ParseLevel` converts a Spring-style level token into a `slog.Level`. It
accepts the Spring tokens `trace`, `debug`, `info`, `warn`, `error`, `fatal`,
and `off` in any case, the extra convenience alias `warning`, plus the offset
notation slog itself parses, such as `DEBUG-2`. `LevelName` renders a level
with the Spring-style tokens `TRACE` and `OFF` for the two levels this
package adds, and with slog's own notation for every other value.

```go
func ParseLevel(token string) (slog.Level, error)
func LevelName(level slog.Level) string
```

```go
level, err := logging.ParseLevel("debug")     // slog.LevelDebug
name := logging.LevelName(logging.LevelTrace) // "TRACE"
```

Two mappings mirror Spring Boot exactly: `fatal` parses as `slog.LevelError`,
the way Logback maps FATAL to ERROR, and `false` parses as `LevelOff`, the
alias Spring keeps because YAML reads a bare `off` as the boolean false. An
unknown token fails with `ErrInvalidLevel`.

## NameFor and LoggerFor

`NameFor` returns the hierarchical logger name of the type `T`: its package
import path, a dot, and the type name — the Go analog of Spring's
`LoggerFactory.getLogger(MyClass.class)`. `LoggerFor` returns a logger of the
manager named after `T`. They are free functions rather than methods because
Go methods cannot be generic.

```go
func NameFor[T any]() string
func LoggerFor[T any](manager *Manager) *slog.Logger
```

```go
logger := logging.LoggerFor[OrderStore](levels)

levels.SetLevel(logging.NameFor[OrderStore](), slog.LevelDebug)
```

A pointer type yields the name of the type it points to, and a type without a
package, such as a predeclared type, yields its own notation.

## Manager.StandardLogger

`StandardLogger` returns a standard library `*log.Logger` that forwards every
message to the named logger at the given level — the bridge for code that
still speaks the classic `log` API. The bridge honors the level tree: when
the effective level of the name silences the given level, messages are
discarded before formatting.

```go
func (manager *Manager) StandardLogger(name string, level slog.Level) *log.Logger
```

```go
bridge := levels.StandardLogger("github.com/acme/legacy", slog.LevelInfo)
bridge.Print("delivered through the tree")

log.SetOutput(bridge.Writer()) // route the process-global logger the same way
log.SetFlags(0)                // the delegate stamps records itself
```

## Errors

The package exports one sentinel error, matchable with `errors.Is`:

- `ErrInvalidLevel` reports a level token that names no known level. The
  offending token is attached to the returned error, and `ParseLevel`,
  `ParseLevels`, `ParseConfiguration`, `Configuration.Validate`, and
  `Manager.Apply` all surface it.

```go
if _, err := logging.ParseLevel("loud"); errors.Is(err, logging.ErrInvalidLevel) {
    // the token names no known level
}
```

For the guided tour of the hierarchy and the override story, see
[Log Levels](/gokeel/guides/log-levels/); for copy-ready snippets, see the
[cookbook](/gokeel/cookbook/logging/).
