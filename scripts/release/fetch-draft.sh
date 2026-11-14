#!/bin/sh
# Download every asset of a tag's draft release, or only the ones named:
#
#   scripts/release/fetch-draft.sh vX.Y.Z DIR [OWNER/REPO [ASSET...]]
#
# release.yml runs it before goreleaser, and scripts/release/macos.sh right
# after it uploads, so both hold the very bytes the draft serves to the
# manifest in the tag (go run ./cmd/releasekit check-darwin). With assets
# named -- release.yml's publish job and scripts/release/sign.sh fetch
# checksums.txt and its signatures -- only those are downloaded, and each
# must be on the draft exactly once. It fails unless exactly one release
# carries the tag and it is still a draft, and DIR must not exist yet. The
# repository defaults to $GITHUB_REPOSITORY (CI), else the one gh resolves
# from this clone.
set -eu

die() {
	echo "fetch-draft: $*" >&2
	exit 1
}

[ $# -ge 2 ] || die "usage: fetch-draft.sh vX.Y.Z DIR [OWNER/REPO [ASSET...]]"
TAG=$1
DIR=$2
REPO=${3:-${GITHUB_REPOSITORY:-}}
shift 2
[ $# -eq 0 ] || shift
# What is left is the assets to fetch; none means every one.
for want in "$@"; do
	case $want in
	*[!A-Za-z0-9._+-]* | '' | . | ..) die "$want is not a plain asset name" ;;
	esac
done
if [ -z "$REPO" ]; then
	REPO=$(gh repo view --json nameWithOwner --jq .nameWithOwner)
fi
case $TAG in
*[!A-Za-z0-9.-]* | '') die "$TAG is not a release tag" ;;
esac
printf '%s\n' "$TAG" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$' || die "$TAG is not a release tag"
case $REPO in
*[!A-Za-z0-9._/-]* | '' | */*/*) die "$REPO is not OWNER/REPO" ;;
esac
[ ! -e "$DIR" ] || die "$DIR exists; it must not"

LIST=$(mktemp "${TMPDIR:-/tmp}/fetch-draft.XXXXXX")
trap 'rm -f "$LIST"' EXIT INT TERM

# Every release, drafts included (only an account that may push sees them),
# narrowed to the ones carrying the tag.
gh api "repos/$REPO/releases" --paginate --jq '.[] | [.id, .tag_name, .draft] | @tsv' >"$LIST"
matches=$(awk -F '\t' -v t="$TAG" '$2 == t' "$LIST")
n=$(printf '%s' "$matches" | grep -c . || true)
[ "$n" -eq 1 ] || die "$n releases of $REPO carry the tag $TAG; want exactly one draft"
id=$(printf '%s\n' "$matches" | cut -f1)
draft=$(printf '%s\n' "$matches" | cut -f3)
[ "$draft" = true ] || die "the release for $TAG is already published, not a draft"
case $id in
*[!0-9]* | '') die "release id $id is not a number" ;;
esac

gh api "repos/$REPO/releases/$id/assets" --paginate --jq '.[] | [.id, .name, .size] | @tsv' >"$LIST"
for want in "$@"; do
	n=$(awk -F '\t' -v w="$want" '$2 == w' "$LIST" | grep -c . || true)
	[ "$n" -eq 1 ] || die "the draft for $TAG holds $n assets named $want; want exactly one"
done
mkdir -p "$DIR"
while IFS="$(printf '\t')" read -r aid name size; do
	case $aid in
	*[!0-9]* | '') die "asset id $aid is not a number" ;;
	esac
	case $name in
	*[!A-Za-z0-9._+-]* | '' | . | ..) die "the draft holds an asset whose name is not a plain file name: $name" ;;
	esac
	if [ $# -gt 0 ]; then
		wanted=
		for want in "$@"; do
			[ "$want" = "$name" ] && wanted=1
		done
		[ -n "$wanted" ] || continue
	fi
	[ ! -e "$DIR/$name" ] || die "the draft holds $name twice"
	gh api -H 'Accept: application/octet-stream' "repos/$REPO/releases/assets/$aid" >"$DIR/$name"
	got=$(wc -c <"$DIR/$name" | tr -d ' ')
	[ "$got" = "$size" ] || die "$name: downloaded $got bytes, the release lists $size"
	echo "fetch-draft: $name ($size bytes)" >&2
done <"$LIST"
