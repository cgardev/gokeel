# gokeel

[![License: MIT](https://img.shields.io/badge/License-MIT-yellow.svg)](LICENSE)
[![Go Version](https://img.shields.io/github/go-mod/go-version/cgardev/gokeel?filename=transaction%2Fgo.mod)](transaction/go.mod)
[![Status: Alpha](https://img.shields.io/badge/status-alpha-orange.svg)](#status-and-roadmap)

The keel of a ship is the first structural piece laid down and the spine the
rest of the hull is built on. **gokeel** is the same idea for a Go application:
a small family of building blocks for a modular monolith — declarative
transactions, an in-process event bus, and a transactional outbox — drawn from
the patterns Spring and Spring Modulith made familiar, with a standard-library
core.

> **Alpha.** The API may still change and there are no tagged releases yet. Pin a
> specific commit when depending on a module.

## The modules

The repository is a single project published as several independent Go modules,
so you take only the pieces you need and your dependency graph stays as small as
the part you import.

| Module | Import path | What it does | Third-party dependencies |
| --- | --- | --- | --- |
| **transaction** | `github.com/cgardev/gokeel/transaction` | A context-bound, declarative transaction manager over `database/sql` with Spring-style propagation and commit synchronizations. | **None** — standard library only. |
| **eventbus** | `github.com/cgardev/gokeel/eventbus` | A small, synchronous, in-process event bus with per-listener delivery. | **None** — standard library only. |
| **outbox** | `github.com/cgardev/gokeel/outbox` | The transactional outbox pattern as an in-process event publication registry: events are written in the producing transaction and delivered after commit. | `transaction`, `eventbus`, `google/uuid`. |

`transaction` and `eventbus` are leaves: each is its own module with a `go.mod`
that has **no `require` directive at all**, a property enforced in CI. `outbox`
sits on top and composes the other two.

### Schema migrations

`outbox` needs two tables. By default it creates them with the standard library
alone — `Store.Initialize` runs a built-in `NativeMigrator`, so the core pulls in
no migration engine. Clients who prefer Flyway-style versioned migrations can opt
in to a [`goway`](https://github.com/cgardev/goway)-backed `Migrator` that lives
in a **separate module**, so goway only enters builds that ask for it:

```go
import (
	"github.com/cgardev/gokeel/outbox"
	"github.com/cgardev/gokeel/outbox/gowaymigrator"
)

store := outbox.NewPostgresStore(database, outbox.CompletionModeUpdate,
	outbox.WithMigrator(gowaymigrator.New()))
```

Any `outbox.Migrator` (`Migrate(ctx, db, dialect, schema) error`) can be supplied
the same way — the schema scripts are exposed through `outbox.Schema()`, so an
alternative engine reuses the exact SQL.

## Install

```sh
go get github.com/cgardev/gokeel/transaction
go get github.com/cgardev/gokeel/eventbus
go get github.com/cgardev/gokeel/outbox
```

## Quick start

A unit of work that commits on success and rolls back on error or panic:

```go
package main

import (
	"context"
	"database/sql"

	"github.com/cgardev/gokeel/transaction"
	_ "modernc.org/sqlite" // your own driver, blank-imported
)

func main() {
	database, _ := sql.Open("sqlite", ":memory:")
	manager := transaction.NewManager(database)

	_ = manager.Run(context.Background(), func(ctx context.Context) error {
		querier := manager.Querier(ctx) // the active transaction, bound to ctx
		_, err := querier.ExecContext(ctx, `INSERT INTO widgets (id) VALUES (?)`, "w1")
		return err // returning an error rolls the whole unit back
	})
}
```

See [`transaction/README.md`](transaction/README.md) for propagation modes,
savepoints, rollback rules, and the synchronization phases.

## Versioning

The modules are released **in lockstep**: every release tags all of them at the
same version, so a single number identifies the whole family and any
`gokeel/transaction@vX.Y.Z` is always compatible with `gokeel/outbox@vX.Y.Z`.
Because Go requires one tag per module subdirectory, a release of `v0.3.0`
creates the tags `transaction/v0.3.0`, `eventbus/v0.3.0`, `outbox/v0.3.0`, and
`outbox/gowaymigrator/v0.3.0`. See [`scripts/release.sh`](scripts/release.sh).

## Status and roadmap

**Implemented (and covered by tests):**

- `transaction`: declarative `Run`, propagation, savepoints/nested transactions,
  isolation and read-only options, timeouts, rollback rules, and before-commit /
  after-commit / after-completion synchronizations.
- `eventbus`: typed registration, ordered synchronous delivery, panic isolation.
- `outbox`: outbox store over PostgreSQL and SQLite, after-commit publication,
  and a resubmitter for unfinished entries.

**Not yet implemented:**

- Tagged releases (the modules are consumed by commit pseudo-version for now).
- A documentation site and runnable `example/` modules per library.

## Acknowledgements

`transaction` follows the behavior of declarative transaction management in the
Spring Framework, and `outbox` follows the event-publication model of Spring
Modulith. These are independent reimplementations in Go; no source code was
copied. "Spring" and "Spring Modulith" are trademarks of Broadcom Inc. and are
used here only nominatively to describe the inspiration.

## Contributing

See [CONTRIBUTING.md](CONTRIBUTING.md) and the [Code of Conduct](CODE_OF_CONDUCT.md).

## License

Released under the [MIT License](LICENSE). Copyright (c) 2026 the gokeel authors.
