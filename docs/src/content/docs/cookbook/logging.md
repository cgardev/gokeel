---
title: Log Levels
description: Recipes for hierarchical logger names, runtime level changes, environment overrides, logger groups, and silencing a noisy dependency.
---

## Give each package its own logger

You want every package to log under its own name and inherit its level from
the package above it.

```go
import "github.com/cgardev/gokeel/logging"

levels := logging.NewManager(
    logging.WithLevel("github.com/acme/shop", slog.LevelWarn),
)

orders := levels.Logger("github.com/acme/shop/orders")
orders.Warn("payment retried", "order", "A-100") // emitted
orders.Info("cache warmed")                      // dropped: the name inherits WARN
```

Gotcha: the root level defaults to `slog.LevelInfo`, like Spring Boot; a name
with no configured ancestor logs at INFO regardless of the delegate's level.

## Raise one package to debug at runtime

You want to diagnose one package in a running program and put it back
afterwards.

```go
levels.SetLevel("github.com/acme/shop/orders", slog.LevelDebug)
// ... reproduce the problem ...
levels.ResetLevel("github.com/acme/shop/orders")
```

Gotcha: loggers already handed out see the change immediately; `ResetLevel`
re-inherits from the nearest configured ancestor, and resetting `root`
restores the construction-time level rather than failing as Logback does.

## Override the compiled-in levels from the environment

You want operators to retune the levels without a rebuild, Spring-properties
style.

```go
overrides, err := logging.ParseLevels(os.Getenv("LOG_LEVELS"))
if err != nil {
    return err
}
if err := levels.Apply(logging.Configuration{Levels: overrides}); err != nil {
    return err
}
```

Gotcha: an unset variable parses to an empty map and applies nothing; an
unknown token fails with `logging.ErrInvalidLevel` before anything changes.

## Load levels from a JSON document

You want the level tree in a configuration file next to the rest of the
application settings.

```go
configuration, err := logging.ParseConfiguration([]byte(`{
    "levels": {
        "root": "warn",
        "github.com/acme/shop/orders": "debug"
    }
}`))
if err != nil {
    return err
}
err = levels.Apply(configuration)
```

Gotcha: unknown fields are rejected, so a misspelled `"level"` fails loudly;
within one document a direct assignment wins over a group fan-out.

## Retune several packages at once with a group

You want one switch for a concern that spans packages, like Spring Boot's
`logging.group.sql`.

```go
levels := logging.NewManager(
    logging.WithGroup("persistence",
        "github.com/acme/shop/orders/postgres",
        "github.com/acme/shop/billing/postgres",
    ),
)

levels.SetLevel("persistence", slog.LevelDebug) // both members, one call
```

Gotcha: a group with members shadows a logger of the same name, as in Spring;
the group name itself never receives a level.

## Bind a logger to a type

You want per-type loggers, the Go analog of
`LoggerFactory.getLogger(OrderStore.class)`.

```go
logger := logging.LoggerFor[OrderStore](levels)

levels.SetLevel(logging.NameFor[OrderStore](), logging.LevelTrace)
```

Gotcha: the type name is one more segment under its package, so a package
level still covers the type until a type-level assignment overrides it.

## Route the classic log package through the tree

You want code on the standard `log` API to obey the same levels as everything
else.

```go
bridge := levels.StandardLogger("github.com/acme/legacy", slog.LevelInfo)

log.SetOutput(bridge.Writer())
log.SetFlags(0)
```

Gotcha: clear the flags — the delegate stamps records itself, so the standard
logger's own prefix would duplicate the timestamp inside every message.

## Silence a noisy dependency

You want one library to stop logging entirely without touching anything else.

```go
levels.SetLevel("github.com/noisy/dependency", logging.LevelOff)
```

Gotcha: `LevelOff` silences even errors; prefer a temporary `slog.LevelError`
if failures should still surface.

For the full surface — the options, the `Configuration` document, and the
parsing helpers — see [Log Levels](/gokeel/guides/log-levels/) and the
[Logging reference](/gokeel/reference/logging/).
