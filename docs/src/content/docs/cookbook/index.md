---
title: Cookbook Overview
description: Task-oriented gokeel recipes for transactions, in-process events, the outbox, schema migrations, log levels, and externalized configuration.
---

The cookbook collects short, copy-ready recipes. Each one states the task,
provides a single complete Go snippet, shows the resulting SQL or log output
where it helps, and ends with a one-line gotcha. For the systematic API, see the
[Reference](/gokeel/reference/).

Every recipe assumes a `context.Context` named `ctx` and a `*transaction.Manager`
named `manager`, constructed once over an open `*sql.DB` with
`transaction.NewManager(database)`. Stores resolve the executor to run against
with `manager.Querier(ctx)`. The import paths are
`github.com/cgardev/gokeel/transaction`, `/eventbus`, `/outbox`, `/logging`,
`/conf`, and the optional `/outbox/gowaymigrator`. SQLite is the primary
backend; PostgreSQL is supported the same way.

## Recipes

### [Transactional use cases](/gokeel/cookbook/transactions/)

- Run a unit of work that commits or rolls back
- Return a value from a unit of work with RunResult
- Span several stores in one transaction with nested Run calls
- Mark a transaction rollback-only without returning an error
- Trigger a side effect after the commit lands
- Bound a transaction with a timeout
- Choose an isolation level for a new transaction

### [In-process events](/gokeel/cookbook/events/)

- Register a typed listener with SubscribeTo
- Publish an event to every matching listener
- Fan an event out to multiple listeners
- Isolate a failing or panicking listener
- Queue a consumer with retries on the broker
- Keep events in order under failure
- Revive a dead letter

### [The outbox](/gokeel/cookbook/outbox/)

- Write an event inside the producing transaction
- Deliver an event only after the commit lands
- Resubmit unfinished entries with a Resubmitter
- Choose a completion mode for settled publications
- Plug in a custom serializer

### [Schema migrations](/gokeel/cookbook/migrations/)

- Create the outbox tables with the native default
- Opt into versioned migrations with goway
- Drive an alternative engine with a custom Migrator

### [Log levels](/gokeel/cookbook/logging/)

- Give each package its own logger
- Raise one package to debug at runtime
- Override the compiled-in levels from the environment
- Load levels from a JSON document
- Retune several packages at once with a group
- Bind a logger to a type
- Route the classic log package through the tree
- Silence a noisy dependency

### [Configuration](/gokeel/cookbook/conf/)

- Load a configuration file onto a struct
- Ship defaults inside the binary and override them outside
- Read secrets from the environment
- Generate the schema for editor completion
- Configure the log levels from the configuration document
- Catch a misspelled key before it hides a setting
