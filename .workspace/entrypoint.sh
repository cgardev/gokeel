#!/usr/bin/env bash
# Container startup: configure the developer git identity and keep Claude Code
# up to date, then hand control to the container command.
set -euo pipefail

# Apply the git identity provided through the .env file, when present.
if [ -n "${GIT_USER_NAME:-}" ]; then
    git config --global user.name "${GIT_USER_NAME}"
fi
if [ -n "${GIT_USER_EMAIL:-}" ]; then
    git config --global user.email "${GIT_USER_EMAIL}"
fi

# Upgrade Claude Code on every start. Kept non-fatal so the workspace still
# comes up when there is no network or the update fails.
if command -v claude >/dev/null 2>&1; then
    claude update || echo "workspace: 'claude update' failed, continuing" >&2
fi

exec "$@"
