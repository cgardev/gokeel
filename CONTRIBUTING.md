# Contributing

Thank you for your interest in improving this project. This document describes
how the repository is organized and how to build and test it.

## Prerequisites

- Go 1.26 or newer.
- Docker, only for the PostgreSQL integration tests. The SQLite tests and all
  unit tests run without it.

## Repository layout

The repository is a single project published as several independent Go modules,
so that the core libraries stay free of external dependencies.

- `transaction` — the declarative transaction manager. Its `go.mod` has no
  `require` directive; database access goes through `database/sql` and the
  application supplies the driver.
- `eventbus` — the in-process event bus. Its `go.mod` also has no `require`
  directive.
- `logging` — hierarchical log-level management for `log/slog`. Its `go.mod`
  also has no `require` directive.
- `configuration` — externalized configuration from JSON documents with
  environment placeholders and JSON Schema generation. Its `go.mod` also has
  no `require` directive.
- `outbox` — the transactional outbox. It composes `transaction` and `eventbus`
  and additionally depends on `github.com/google/uuid`. By default it applies its
  schema with the standard library alone (a built-in `NativeMigrator`).
- `outbox/gowaymigrator` — an optional `outbox.Migrator` backed by
  `github.com/cgardev/goway`, kept in its own module so that `goway` only enters
  the builds of clients who opt in. A CI gate enforces that `goway` never reaches
  the `outbox` core.
- `transaction/integration` — a separate module that holds the database
  integration tests for `transaction`, keeping the test drivers (`modernc.org/sqlite`,
  `github.com/jackc/pgx/v5`) and the container library out of the core module.

While the family has no published tags, the intra-repository module edges are
wired with relative `replace` directives so that a fresh checkout builds without
any extra setup. The release script swaps them for pinned versions when cutting a
tagged release.

## Building and testing

Run the static checks and tests for each module:

```sh
for module in transaction eventbus logging configuration outbox outbox/gowaymigrator transaction/integration; do
  go -C "$module" build ./...
  go -C "$module" vet ./...
  go -C "$module" test ./... -count=1
done
gofmt -l .
```

The PostgreSQL integration tests are guarded by a build tag and start a
container through testcontainers:

```sh
go -C transaction/integration test -tags=integration ./... -count=1
go -C outbox test -tags=integration ./... -count=1
```

## Coding standards

- Format all code with `gofmt`.
- Document every exported identifier with a complete sentence that begins with
  the identifier name.
- Keep the `transaction`, `eventbus`, `logging`, and `configuration` modules
  free of external dependencies.
- Write clear, self-documenting code, and add comments only where the logic is
  not self-evident.
- Use technical, impersonal English in comments and documentation.
