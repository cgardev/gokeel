# Development Workspace

A containerized development environment for the gokeel repository. The whole
repository is mounted inside the container, so edits sync live in both
directions, and the container shares the host network so any service you start
inside is reachable from your local machine.

## What it provides

- **Full repository inside the container** — mounted at `/workspace`.
- **Shared host networking** — a process listening on a port inside the
  container is available on the same port on the host (and the reverse).
- **AI sandbox** — Claude Code is preinstalled in the base image; run it with
  `--dangerously-skip-permissions` to let it execute any command safely, isolated
  from your host.
- **Persistent caches** — Go modules, the pnpm store, and Claude credentials
  live on named volumes and survive rebuilds.
- **Automatic setup on startup** — the container applies your git identity from
  a local `.env` file and runs `claude update` every time it boots.

## Requirements

- Docker with Compose v2 (`docker compose`).
- On Docker Desktop (Windows/macOS): enable **Settings > Resources > Network >
  Host networking** so `network_mode: host` works.

## Usage

Run these commands from the `.workspace` directory.

```sh
# One-time: create your local settings and set your git identity.
cp .env.example .env
# then edit .env and fill in GIT_USER_NAME and GIT_USER_EMAIL

# Build the image and start the container in the background.
docker compose up -d --build

# Open an interactive shell inside the workspace.
docker compose exec workspace bash

# Run Claude Code as an unrestricted sandboxed agent.
docker compose exec workspace claude --dangerously-skip-permissions

# Stop the container (named volumes are kept).
docker compose down

# Stop and also remove the caches and stored credentials.
docker compose down -v
```

The first time you run Claude Code inside the container it will ask you to log
in; the credentials are stored in the `claude-config` volume and reused
afterwards.

## Networking example

Because the container uses host networking, starting a server inside it exposes
it directly on the host:

```sh
docker compose exec workspace bash -lc 'cd docs && pnpm dev'
# then open the reported port on http://localhost:<port> from the host
```

## Notes

- The base image follows the Coder convention (unprivileged user `coder`). If
  your base image uses a different user, adjust the `USER` line in the
  `Dockerfile`.
- Host networking is a hard requirement for the port-sharing behaviour; without
  it, ports would have to be published individually and dynamic ports would not
  be reachable.
