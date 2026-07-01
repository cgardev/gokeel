#!/usr/bin/env bash
#
# Release the whole gokeel family at a single version (lockstep): every module is
# tagged at the same version, so one number identifies the entire family.
#
# Because Go requires one tag per module subdirectory, a release of v0.3.0 creates
# the tags transaction/v0.3.0, eventbus/v0.3.0, logging/v0.3.0,
# configuration/v0.3.0, outbox/v0.3.0, outbox/gowaymigrator/v0.3.0,
# sqlbus/v0.3.0 and sqlbus/gowaymigrator/v0.3.0.
#
# Usage: scripts/release.sh v0.3.0
set -euo pipefail

V="${1:?usage: scripts/release.sh vX.Y.Z}"
if [[ ! "$V" =~ ^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.]+)?$ ]]; then
	echo "error: version must look like v0.3.0 (got: $V)" >&2
	exit 1
fi

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"

if [[ -n "$(git status --porcelain)" ]]; then
	echo "error: working tree is not clean; commit or stash first" >&2
	exit 1
fi

remote="${REMOTE:-origin}"

# 1) Tag the zero-dependency leaves first. They have nothing to coordinate.
echo "==> tagging leaves transaction/$V, eventbus/$V, logging/$V and configuration/$V"
git tag "transaction/$V"
git tag "eventbus/$V"
git tag "logging/$V"
git tag "configuration/$V"
git push "$remote" "transaction/$V" "eventbus/$V" "logging/$V" "configuration/$V"

# 2) Point outbox at the freshly tagged leaves: drop the local replaces and pin
#    the published versions, then refresh go.sum. Commit and tag outbox.
echo "==> pinning outbox to gokeel $V"
(
	cd outbox
	go mod edit \
		-dropreplace=github.com/cgardev/gokeel/transaction \
		-dropreplace=github.com/cgardev/gokeel/eventbus \
		-require="github.com/cgardev/gokeel/transaction@$V" \
		-require="github.com/cgardev/gokeel/eventbus@$V"
	GOFLAGS=-mod=mod go mod tidy
)
git commit -am "release: pin outbox to gokeel $V"
git tag "outbox/$V"
git push "$remote" "outbox/$V"

# 2b) Point outbox/gowaymigrator at the freshly tagged outbox: drop all three
#     local replaces and pin the published outbox, then refresh go.sum. This must
#     run after outbox/$V is pushed, because tidy resolves outbox@$V (and, through
#     it, transaction@$V and eventbus@$V) from the published tags. Tag it last.
echo "==> pinning outbox/gowaymigrator to gokeel $V"
(
	cd outbox/gowaymigrator
	# Pin all three intra-repo edges to $V. transaction and eventbus are indirect
	# here, but their go.mod lines still carry the development sentinel version, so
	# they must be rewritten off it explicitly; otherwise, once the replaces are
	# dropped, `go mod tidy` tries to fetch the sentinel pseudo-version and fails.
	go mod edit \
		-dropreplace=github.com/cgardev/gokeel/outbox \
		-dropreplace=github.com/cgardev/gokeel/transaction \
		-dropreplace=github.com/cgardev/gokeel/eventbus \
		-require="github.com/cgardev/gokeel/outbox@$V" \
		-require="github.com/cgardev/gokeel/transaction@$V" \
		-require="github.com/cgardev/gokeel/eventbus@$V"
	GOFLAGS=-mod=mod go mod tidy
)
git commit -am "release: pin outbox/gowaymigrator to gokeel $V"
git tag "outbox/gowaymigrator/$V"
git push "$remote" "outbox/gowaymigrator/$V"

# 2c) Point sqlbus at the freshly tagged leaves, exactly like outbox.
echo "==> pinning sqlbus to gokeel $V"
(
	cd sqlbus
	go mod edit \
		-dropreplace=github.com/cgardev/gokeel/transaction \
		-dropreplace=github.com/cgardev/gokeel/eventbus \
		-require="github.com/cgardev/gokeel/transaction@$V" \
		-require="github.com/cgardev/gokeel/eventbus@$V"
	GOFLAGS=-mod=mod go mod tidy
)
git commit -am "release: pin sqlbus to gokeel $V"
git tag "sqlbus/$V"
git push "$remote" "sqlbus/$V"

# 2d) Point sqlbus/gowaymigrator at the freshly tagged sqlbus, exactly like
#     outbox/gowaymigrator: all three intra-repo edges are rewritten off the
#     development sentinel version before the replaces are dropped.
echo "==> pinning sqlbus/gowaymigrator to gokeel $V"
(
	cd sqlbus/gowaymigrator
	go mod edit \
		-dropreplace=github.com/cgardev/gokeel/sqlbus \
		-dropreplace=github.com/cgardev/gokeel/transaction \
		-dropreplace=github.com/cgardev/gokeel/eventbus \
		-require="github.com/cgardev/gokeel/sqlbus@$V" \
		-require="github.com/cgardev/gokeel/transaction@$V" \
		-require="github.com/cgardev/gokeel/eventbus@$V"
	GOFLAGS=-mod=mod go mod tidy
)
git commit -am "release: pin sqlbus/gowaymigrator to gokeel $V"
git tag "sqlbus/gowaymigrator/$V"
git push "$remote" "sqlbus/gowaymigrator/$V"

# 3) Restore the relative replaces on the branch so local development keeps
#    building from the working tree.
echo "==> restoring development replaces"
(
	cd outbox
	go mod edit \
		-replace=github.com/cgardev/gokeel/transaction=../transaction \
		-replace=github.com/cgardev/gokeel/eventbus=../eventbus
	go mod tidy
)
(
	cd outbox/gowaymigrator
	go mod edit \
		-replace=github.com/cgardev/gokeel/outbox=../ \
		-replace=github.com/cgardev/gokeel/transaction=../../transaction \
		-replace=github.com/cgardev/gokeel/eventbus=../../eventbus
	go mod tidy
)
(
	cd sqlbus
	go mod edit \
		-replace=github.com/cgardev/gokeel/transaction=../transaction \
		-replace=github.com/cgardev/gokeel/eventbus=../eventbus
	go mod tidy
)
(
	cd sqlbus/gowaymigrator
	go mod edit \
		-replace=github.com/cgardev/gokeel/sqlbus=../ \
		-replace=github.com/cgardev/gokeel/transaction=../../transaction \
		-replace=github.com/cgardev/gokeel/eventbus=../../eventbus
	go mod tidy
)
git commit -am "post-release: restore development replaces"
git push "$remote" HEAD

echo "==> released gokeel $V (transaction/$V, eventbus/$V, logging/$V, configuration/$V, outbox/$V, outbox/gowaymigrator/$V, sqlbus/$V, sqlbus/gowaymigrator/$V)"
