#!/usr/bin/env bash
#
# Publish the gokeel family in the tagless mode gooq uses: every intra-repository
# require directive is pinned to a pseudo-version of a commit on main, so
# `go get github.com/cgardev/gokeel/<module>@latest` resolves without any tags.
# The relative replace directives stay in place, so local development keeps
# building from the working tree; external consumers ignore them and follow the
# pinned pseudo-versions instead.
#
# Two commits are created because the pins are ordered: outbox and sqlbus pin the
# leaf modules first, and only once that commit exists on the remote can the
# gowaymigrator adapters pin outbox and sqlbus at a commit whose go.mod files are
# themselves resolvable.
#
# Rerun the script whenever a leaf module changes in a way its dependents need to
# see; consumers of @latest otherwise keep resolving the older pinned commit.
#
# This mode only applies while the family has no published tags. Once
# scripts/release.sh has cut a tagged release, pseudo-versions derive from that
# tag and the v0.0.0 form computed here becomes invalid, so the script refuses to
# run.
#
# Usage: scripts/pin.sh
set -euo pipefail

root="$(cd "$(dirname "$0")/.." && pwd)"
cd "$root"

if [[ -n "$(git status --porcelain)" ]]; then
	echo "error: working tree is not clean; commit or stash first" >&2
	exit 1
fi

if [[ -n "$(git tag --list)" ]]; then
	echo "error: the repository already has tags; release with scripts/release.sh instead" >&2
	exit 1
fi

branch="$(git rev-parse --abbrev-ref HEAD)"
if [[ "$branch" != "main" ]]; then
	echo "error: @latest resolves from main, so pins must land there (currently on: $branch)" >&2
	exit 1
fi

remote="${REMOTE:-origin}"

# pseudoversion prints the v0.0.0 pseudo-version of the given commit, the exact
# form the Go module proxy derives for a commit with no tagged ancestor: the
# committer timestamp in UTC followed by the twelve-character commit hash.
pseudoversion() {
	local stamp hash
	stamp="$(TZ=UTC git show -s --format=%cd --date=format-local:'%Y%m%d%H%M%S' "$1")"
	hash="$(git rev-parse --short=12 "$1")"
	echo "v0.0.0-$stamp-$hash"
}

# 1) Pin outbox and sqlbus to the leaves at the current HEAD. The replaces keep
#    resolution local, so tidy preserves the pinned versions verbatim.
leafpin="$(pseudoversion HEAD)"
echo "==> pinning outbox and sqlbus to the leaves at $leafpin"
for module in outbox sqlbus; do
	(
		cd "$module"
		go mod edit \
			-require="github.com/cgardev/gokeel/transaction@$leafpin" \
			-require="github.com/cgardev/gokeel/eventbus@$leafpin"
		GOFLAGS=-mod=mod go mod tidy
	)
done
git commit -am "release: pin outbox and sqlbus to main at $leafpin"
git push "$remote" HEAD

# 2) Pin the adapters to outbox and sqlbus at the commit created above. The
#    leaves are pinned at that same commit: their code is identical there, and a
#    single version per commit keeps the pins easy to audit.
adapterpin="$(pseudoversion HEAD)"
echo "==> pinning the gowaymigrator adapters to $adapterpin"
(
	cd outbox/gowaymigrator
	go mod edit \
		-require="github.com/cgardev/gokeel/outbox@$adapterpin" \
		-require="github.com/cgardev/gokeel/transaction@$adapterpin" \
		-require="github.com/cgardev/gokeel/eventbus@$adapterpin"
	GOFLAGS=-mod=mod go mod tidy
)
(
	cd sqlbus/gowaymigrator
	go mod edit \
		-require="github.com/cgardev/gokeel/sqlbus@$adapterpin" \
		-require="github.com/cgardev/gokeel/transaction@$adapterpin" \
		-require="github.com/cgardev/gokeel/eventbus@$adapterpin"
	GOFLAGS=-mod=mod go mod tidy
)
git commit -am "release: pin the gowaymigrator adapters to main at $adapterpin"
git push "$remote" HEAD

echo "==> pinned; every module now resolves with: go get github.com/cgardev/gokeel/<module>@latest"
