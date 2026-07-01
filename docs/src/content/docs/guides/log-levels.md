---
title: Log Levels
description: Name loggers hierarchically, let a package inherit its level from the nearest configured ancestor, and retune a running program from code, a JSON document, or the environment.
---

The `logging` module brings Spring Boot's `logging.level` model to the
standard library logger. Every `slog.Logger` is bound to a hierarchical name,
one level assignment on a package path governs every logger beneath it, and
the levels a program compiles in can be overridden from outside and changed
while the program runs — down to a single type, the way
`logging.level.com.acme.shop.OrderService=DEBUG` reaches one class in Spring.

A `Manager` owns the level tree. It depends only on the Go standard library
and is safe for concurrent use: handlers read an immutable snapshot of the
tree through an atomic pointer, the same discipline as `slog.LevelVar`, so
the level check on the logging hot path takes no lock.

## Constructing a manager

`NewManager` returns a manager with the analog of the defaults a fresh Spring
Boot application boots with — the root level is `slog.LevelInfo`, and records
are written as text to standard error:

```go
import "github.com/cgardev/gokeel/logging"

levels := logging.NewManager()
logger := levels.Logger("github.com/acme/shop/orders")

logger.Info("order placed", "order", "A-100")
// time=... level=INFO msg="order placed" logger=github.com/acme/shop/orders order=A-100
```

Every record carries its logger's name under the `logger` attribute, the
counterpart of the logger-name column in a Spring log line. `WithNameKey`
renames the attribute, and the empty key disables it.

The output format is a delegate `slog.Handler`, so any handler works —
`slog.NewJSONHandler`, or your own. The tree is the single filtering
authority: the delegate's own level is never consulted, so construct the
delegate with the lowest level it should ever emit:

```go
levels := logging.NewManager(
	logging.WithHandler(slog.NewJSONHandler(os.Stderr, &slog.HandlerOptions{
		Level: logging.LevelTrace,
	})),
)
```

## The hierarchy

Logger names are hierarchical: segments are separated by `/` and `.`, so a
package import path and a package-qualified type name form the same kind of
tree Spring builds from dotted class names. The effective level of a name is
the level assigned to its nearest configured ancestor, falling back to the
root — Logback's effective-level rule:

```go
levels := logging.NewManager(
	logging.WithLevel("github.com/acme/shop", slog.LevelWarn),
	logging.WithLevel("github.com/acme/shop/orders", slog.LevelDebug),
)

levels.EffectiveLevel("github.com/acme/shop/orders/postgres.Store") // DEBUG
levels.EffectiveLevel("github.com/acme/shop/billing")               // WARN
levels.EffectiveLevel("github.com/acme/warehouse")                  // INFO, the root
```

Inheritance cuts whole segments, so a name never leaks its level onto a
sibling that merely shares a prefix: a level on `github.com/acme/event` does
not reach `github.com/acme/eventbus`. The pseudo-name `root` addresses the
root logger, as it does in Spring.

## External overrides

Levels declared with `WithLevel` are the analog of the levels a Logback file
compiles into a Spring application. External configuration overrides them:
`Apply` overlays a `Configuration` document over the current tree, exactly as
Spring's `logging.level` properties are applied over the logging file at
startup. Every name the document configures is overwritten; every other name
keeps its assignment:

```go
document := []byte(`{
	"levels": {
		"root": "warn",
		"github.com/acme/shop/orders": "debug"
	}
}`)

configuration, err := logging.ParseConfiguration(document)
if err != nil {
	return err
}
err = levels.Apply(configuration)
```

`ParseConfiguration` rejects unknown fields and validates every level token,
and `Apply` validates again before touching the tree, so a misspelled level
fails with `logging.ErrInvalidLevel` and changes nothing.

For overrides that arrive through a flag or an environment variable,
`ParseLevels` reads the compact `name=level` list form:

```go
overrides, err := logging.ParseLevels(os.Getenv("LOG_LEVELS"))
if err != nil {
	return err
}
err = levels.Apply(logging.Configuration{Levels: overrides})
```

An unset variable parses to an empty map and applies nothing, so the code
path is the same whether or not the operator provided overrides.

## Changing levels at runtime

`SetLevel` and `ResetLevel` are the runtime mutations Spring Boot Actuator
performs through its loggers endpoint. A change is visible to every logger
already created — handlers consult the tree on every record, so a long-lived
logger stored in a struct is retuned in place:

```go
levels.SetLevel("github.com/acme/shop/orders", slog.LevelDebug)
// ... diagnose ...
levels.ResetLevel("github.com/acme/shop/orders") // inherit again
```

`ResetLevel` clears the assignment so the name inherits from its nearest
configured ancestor again — the effect of writing a null `configuredLevel`
through the actuator. Resetting the root restores the level the manager was
constructed with; Logback instead rejects clearing the root level, and the
divergence keeps the operation total while the root always keeps a level.

`Loggers` is the inventory the actuator exposes: every known name with its
directly configured level, if any, and the effective level the hierarchy
resolves for it. The root still carries the level the document assigned above:

```go
entries := levels.Loggers()
// entries[0] == logging.LoggerLevels{Name: "root", Configured: slog.LevelWarn,
//	IsConfigured: true, Effective: slog.LevelWarn}
```

## Logger groups

A group names several loggers at once, the analog of Spring Boot's
`logging.group` properties. Assigning a level to the group name fans it out
to every member, and a group with members shadows a logger of the same name,
as it does in Spring:

```go
levels := logging.NewManager(
	logging.WithGroup("persistence",
		"github.com/acme/shop/orders/postgres",
		"github.com/acme/shop/billing/postgres",
	),
)

levels.SetLevel("persistence", slog.LevelDebug) // both members, one call
```

Groups can also arrive in a `Configuration` document under `"groups"`, and
they stay registered for later `SetLevel` and `ResetLevel` calls. Where
Spring leaves a conflict between a group level and a member's direct level to
property order, the module resolves it deterministically: within one
document, a level assigned directly to a name wins over a level the name
receives through a group.

## Trace, off, and the tokens

slog predeclares four levels; Spring's ladder has two more. `LevelTrace`
sits one spacing step below `slog.LevelDebug`, and `LevelOff` silences a
logger entirely — no record carries a level that high:

```go
levels.SetLevel("github.com/noisy/dependency", logging.LevelOff) // silence it
levels.SetLevel("github.com/acme/suspect", logging.LevelTrace)   // everything
```

`ParseLevel` reads the Spring tokens in any case — `trace`, `debug`, `info`,
`warn`, `error`, `fatal`, `off` — plus the offset notation slog itself
parses, such as `DEBUG-2`. Two mappings mirror Spring Boot exactly: `fatal`
parses as `slog.LevelError`, the way Logback maps FATAL to ERROR, and `false`
parses as `LevelOff`, the alias Spring keeps because YAML reads a bare `off`
as the boolean false.

## A logger per type

`NameFor` derives a hierarchical name from a type — its package import path,
a dot, and the type name — the Go analog of Spring's
`LoggerFactory.getLogger(MyClass.class)`. `LoggerFor` binds a logger to that
name in one call:

```go
type OrderStore struct{ /* ... */ }

logger := logging.LoggerFor[OrderStore](levels)
// named github.com/acme/shop/orders.OrderStore

levels.SetLevel(logging.NameFor[OrderStore](), slog.LevelDebug)
```

Because the type name is one more segment under the package, a package-level
assignment still covers it, and a type-level assignment overrides the package
for that one type alone.

## The classic log package

Code that still speaks the standard `log` API joins the tree through
`StandardLogger`, which returns a `*log.Logger` forwarding every message at a
fixed level. The bridge honors the tree, so a silenced name discards messages
before they are formatted:

```go
bridge := levels.StandardLogger("github.com/acme/legacy", slog.LevelInfo)
bridge.Print("still on the old API") // filtered like any other record
```

The process-global standard logger is routed the same way:

```go
log.SetOutput(bridge.Writer())
log.SetFlags(0) // the delegate stamps records; avoid a duplicated timestamp
```

## Where to go next

The [Logging reference](/gokeel/reference/logging/) covers every exported
symbol — the options, the `Configuration` document, and the parsing helpers —
and the [cookbook](/gokeel/cookbook/logging/) collects copy-ready recipes for
the common tasks.
