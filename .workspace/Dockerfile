# Development workspace image for the gokeel repository.
#
# It extends the shared base image (Go, Node.js via nvm, pnpm, git, Claude Code
# and Gemini CLI are already provisioned there) and only prepares persistent
# cache locations so tooling state survives container rebuilds.
FROM cgarciamtg/coder-workspace-base:20260323-125620

USER root

# Cache and configuration directories backed by named volumes in the compose
# file. They are made world-writable so they stay usable regardless of the
# effective user the base image runs as.
RUN mkdir -p /cache/go/bin /cache/go-build /cache/pnpm /cache/claude \
    && chmod -R 777 /cache

# Point every toolchain at the persisted cache directories.
ENV GOPATH=/cache/go \
    GOMODCACHE=/cache/go/pkg/mod \
    GOCACHE=/cache/go-build \
    PNPM_HOME=/cache/pnpm \
    CLAUDE_CONFIG_DIR=/cache/claude \
    PATH=/cache/pnpm:/cache/go/bin:$PATH

# Startup script: applies the git identity and upgrades Claude Code on boot.
COPY entrypoint.sh /usr/local/bin/entrypoint.sh
RUN chmod +x /usr/local/bin/entrypoint.sh

# The base image follows the Coder convention (unprivileged user "coder").
# Adjust this line and the compose "user:" mapping if your base differs.
USER coder
WORKDIR /workspace

ENTRYPOINT ["/usr/local/bin/entrypoint.sh"]
