#!/bin/sh
# Phase 2 of a release, on the owner's Mac once release.yml has published it:
# render the Homebrew cask (`brew install arctop/tap/seamless`) from the
# release's checksums.txt, checked against its Sigstore bundle for this exact
# tag, and push it to arctop/homebrew-tap with the owner's own GitHub login.
# CI holds no token for the tap.
#
#   scripts/release/cask.sh vX.Y.Z            push the cask
#   scripts/release/cask.sh vX.Y.Z --dry-run  show the change, push nothing
#
# A prerelease tag, or a release marked prerelease (a candidate, or a yanked
# release), gets no cask, as goreleaser's skip_upload: auto did. Neither does
# a release older than the cask's: brew would move everyone back. A bad cask
# is reverted in the tap.
set -eu

die() {
	echo "cask: $*" >&2
	exit 1
}
say() {
	echo "cask: $*" >&2
}

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
cd "$ROOT"

[ $# -ge 1 ] || die "usage: scripts/release/cask.sh vX.Y.Z [--dry-run]"
TAG=$1
shift
DRY=
for arg in "$@"; do
	case $arg in
	--dry-run) DRY=1 ;;
	*) die "unknown option $arg" ;;
	esac
done
case $TAG in
*[!A-Za-z0-9.-]* | '') die "$TAG is not a release tag" ;;
*-*)
	say "$TAG is a prerelease: no cask, so brew never moves anyone onto a release candidate"
	exit 0
	;;
esac
printf '%s\n' "$TAG" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+$' || die "$TAG is not a release tag (vX.Y.Z)"
VERSION=${TAG#v}
# The cask's URLs are this repository's releases (cmd/releasekit/cask.go).
REPO=arctop/seamless
TAP=arctop/homebrew-tap
for tool in gh git go curl; do
	command -v "$tool" >/dev/null 2>&1 || die "needs $tool"
done

state=$(gh release view "$TAG" --repo "$REPO" --json isDraft,isPrerelease --jq '[.isDraft, .isPrerelease] | @tsv')
[ "$(printf '%s\n' "$state" | cut -f1)" = false ] || die "the release $TAG is still a draft; run this once release.yml has published it"
if [ "$(printf '%s\n' "$state" | cut -f2)" != false ]; then
	say "$TAG is marked prerelease (a candidate, or yanked): no cask"
	exit 0
fi

WORK=$(mktemp -d "${TMPDIR:-/tmp}/seamless-cask.XXXXXX")
trap 'rm -rf "$WORK"' EXIT INT TERM
base=https://github.com/$REPO/releases/download/$TAG
for asset in checksums.txt checksums.txt.sigstore.json; do
	curl -fsSL --proto '=https' --retry 3 -o "$WORK/$asset" "$base/$asset"
done
go run ./cmd/releasekit cask -tag "$TAG" -checksums "$WORK/checksums.txt" \
	-bundle "$WORK/checksums.txt.sigstore.json" >"$WORK/seamless.rb"

gh repo clone "$TAP" "$WORK/tap" -- --quiet --depth 1
cask=$WORK/tap/Casks/seamless.rb
if [ -f "$cask" ]; then
	current=$(sed -n 's/^  version "\(.*\)"$/\1/p' "$cask")
	newest=$(printf '%s\n%s\n' "$current" "$VERSION" | sort -t. -k1,1n -k2,2n -k3,3n | tail -1)
	[ "$newest" = "$VERSION" ] || die "the tap's cask is $current, newer than $VERSION; brew would move everyone back"
fi
mkdir -p "$(dirname "$cask")"
cp "$WORK/seamless.rb" "$cask"
if [ -z "$(git -C "$WORK/tap" status --porcelain -- Casks/seamless.rb)" ]; then
	say "the tap's cask is already $TAG's"
	exit 0
fi
git -C "$WORK/tap" --no-pager diff -- Casks/seamless.rb >&2 || true
if [ -n "$DRY" ]; then
	say "dry run: nothing pushed"
	exit 0
fi
git -C "$WORK/tap" add -- Casks/seamless.rb
git -C "$WORK/tap" commit -q -m "seamless $TAG"
git -C "$WORK/tap" push -q origin HEAD
say "pushed the cask for $TAG to $TAP"
