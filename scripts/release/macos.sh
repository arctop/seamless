#!/bin/sh
# Phase 1 of a release, on the owner's Mac: build the darwin binaries, sign
# them with the project's Developer ID Application certificate, have Apple
# notarize them, pack them, build/sign/notarize the Touch ID approver, put the
# three assets on the tag's draft release, and write the signed annotated tag
# whose message carries their sha256 manifest. The owner then pushes the tag.
#
#   scripts/release/macos.sh vX.Y.Z             the release
#   scripts/release/macos.sh vX.Y.Z --dry-run   everything but the draft and the tag
#   scripts/release/macos.sh vX.Y.Z --adhoc     ad-hoc signed, not notarized, no approver:
#                                               a fork's release, or a local test (on
#                                               arctop/seamless only with --dry-run)
#
# The release, in order (scripts/release/README.md has the steps):
#   1. this script: build, sign, notarize, package, draft, tag
#   2. the owner pushes the tag: git push origin vX.Y.Z
#   3. release.yml, once the owner approves its `release` environment: holds
#      the draft's macOS assets to the tag's manifest, builds linux and
#      windows, and signs checksums.txt (Sigstore) into the draft
#   4. scripts/release/sign.sh vX.Y.Z: the owner signs checksums.txt with a
#      hardware key and uploads checksums.txt.sshsig; then approves the
#      publish job, which verifies it against release_signers and publishes
#   5. scripts/release/cask.sh vX.Y.Z: the Homebrew cask
#   6. make changelog, then a site-* tag publishes the refreshed site
#
# One-time setup of the Mac that releases is in scripts/release/README.md.
#
# Environment:
#   SEAMLESS_SIGN_IDENTITY     the codesign identity, a SHA-1 hash or name; by
#                              default the login keychain's one valid
#                              "Developer ID Application" identity
#   SEAMLESS_NOTARY_PROFILE    notarytool keychain profile (default seamless-notary)
#   SEAMLESS_APPROVER_PROFILE  the approver's Developer ID provisioning profile
#                              (default ~/.config/seamless-release/approver.provisionprofile)
#
# Its output goes to dist/macos-<version>/ (dist/ is ignored). Nothing it
# prints names the signing identity or its team.
set -eu

die() {
	echo "release: $*" >&2
	exit 1
}
say() {
	echo "release: $*" >&2
}

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
cd "$ROOT"

[ $# -ge 1 ] || die "usage: scripts/release/macos.sh vX.Y.Z [--dry-run] [--adhoc]"
TAG=$1
shift
DRY=
ADHOC=
for arg in "$@"; do
	case $arg in
	--dry-run) DRY=1 ;;
	--adhoc) ADHOC=1 ;;
	*) die "unknown option $arg" ;;
	esac
done
case $TAG in
*[!A-Za-z0-9.-]* | '') die "$TAG is not a release tag (vX.Y.Z or vX.Y.Z-pre)" ;;
esac
printf '%s\n' "$TAG" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$' ||
	die "$TAG is not a release tag (vX.Y.Z or vX.Y.Z-pre)"
VERSION=${TAG#v}
NOTARY_PROFILE=${SEAMLESS_NOTARY_PROFILE:-seamless-notary}
APPROVER_PROFILE_FILE=${SEAMLESS_APPROVER_PROFILE:-$HOME/.config/seamless-release/approver.provisionprofile}
APPROVER_ASSET=seamless-approve_${VERSION}_darwin.zip

# --- preflight -------------------------------------------------------------
[ "$(uname -s)" = Darwin ] || die "the macOS assets build on macOS only"
for tool in git go gh codesign xcrun ditto security tar; do
	command -v "$tool" >/dev/null 2>&1 || die "needs $tool"
done
REPO=$(gh repo view --json nameWithOwner --jq .nameWithOwner)
if [ "$REPO" = arctop/seamless ] && [ -n "$ADHOC" ] && [ -z "$DRY" ]; then
	die "a release of arctop/seamless is Developer ID-signed and notarized; --adhoc only with --dry-run"
fi

# check: an error for the release, a warning for a dry run.
check() {
	if [ -n "$DRY" ]; then
		say "warning (dry run): $*"
	else
		die "$*"
	fi
}

[ -z "$(git status --porcelain --untracked-files=normal)" ] ||
	die "the working tree is not clean; the build stamps HEAD's commit"
HEAD=$(git rev-parse HEAD)
git fetch --quiet origin main
git merge-base --is-ancestor "$HEAD" origin/main || check "HEAD ($HEAD) is not on origin/main"
ci=$(gh run list --repo "$REPO" --workflow CI --branch main --limit 100 \
	--json headSha,status,conclusion --jq ".[] | select(.headSha == \"$HEAD\") | .status + \" \" + .conclusion" | head -1)
[ "$ci" = "completed success" ] || check "CI on HEAD ($HEAD) is ${ci:-not run}, not green"
on_origin=$(git ls-remote --tags origin "refs/tags/$TAG")
[ -z "$on_origin" ] || die "$TAG is already on origin"
if git rev-parse -q --verify "refs/tags/$TAG" >/dev/null; then
	check "a local tag $TAG exists; it is not on origin, so delete it (git tag -d $TAG) and run again"
fi

# The release for the tag: none yet, or this script's draft from an earlier
# run, which is reused. goreleaser finds the draft by its title, the tag.
DRAFT_ID=
existing=$(gh api "repos/$REPO/releases" --paginate --jq '.[] | [.id, .tag_name, .draft, .name] | @tsv' |
	awk -F '\t' -v t="$TAG" '$2 == t')
case $(printf '%s' "$existing" | grep -c . || true) in
0) ;;
1)
	[ "$(printf '%s\n' "$existing" | cut -f3)" = true ] || die "the release $TAG is already published"
	[ "$(printf '%s\n' "$existing" | cut -f4)" = "$TAG" ] || die "the draft for $TAG is not titled $TAG, so goreleaser would not find it"
	DRAFT_ID=$(printf '%s\n' "$existing" | cut -f1)
	;;
*) die "several releases carry the tag $TAG" ;;
esac

OUT=$ROOT/dist/macos-$VERSION
rm -rf "$OUT"
mkdir -p "$OUT/assets"

IDENTITY=-
TEAM=
if [ -z "$ADHOC" ]; then
	IDENTITY=${SEAMLESS_SIGN_IDENTITY:-}
	if [ -z "$IDENTITY" ]; then
		ids=$(security find-identity -v -p codesigning | awk '/"Developer ID Application: / { print $2 }' | sort -u)
		n=$(printf '%s' "$ids" | grep -c . || true)
		[ "$n" -eq 1 ] ||
			die "the keychain holds $n valid Developer ID Application identities; set SEAMLESS_SIGN_IDENTITY to the one to sign with (its SHA-1 hash)"
		IDENTITY=$ids
	fi
	xcrun notarytool history --keychain-profile "$NOTARY_PROFILE" >/dev/null 2>&1 ||
		die "notarytool has no working keychain profile $NOTARY_PROFILE (one-time setup: scripts/release/README.md)"
	[ -f "$APPROVER_PROFILE_FILE" ] ||
		die "no approver provisioning profile at $APPROVER_PROFILE_FILE (set SEAMLESS_APPROVER_PROFILE; scripts/release/README.md)"
	security cms -D -i "$APPROVER_PROFILE_FILE" >"$OUT/approver-profile.plist" 2>/dev/null ||
		die "$APPROVER_PROFILE_FILE is not a provisioning profile"
	TEAM=$(/usr/libexec/PlistBuddy -c 'Print :TeamIdentifier:0' "$OUT/approver-profile.plist")
	rm -f "$OUT/approver-profile.plist"
	[ -n "$TEAM" ] || die "the approver profile names no team"
fi

say "$TAG from $HEAD ($([ -n "$ADHOC" ] && echo 'ad-hoc signed' || echo 'Developer ID, notarized'))$([ -n "$DRY" ] && echo ', dry run')"
go build -o "$OUT/releasekit" ./cmd/releasekit
RK=$OUT/releasekit

# --- build: .goreleaser.yaml's builds, for darwin ---------------------------
"$RK" darwin-build -tag "$TAG" -out "$OUT/bin" >"$OUT/binaries.txt"

# --- sign: hardened runtime, Apple's secure timestamp, the binary's name as
# its identifier (what goreleaser's signing gave v0.8.2's binaries).
sign() {
	if [ -n "$ADHOC" ]; then
		codesign --force --sign - --options runtime --identifier "$(basename "$1")" "$1"
		return
	fi
	codesign --force --sign "$IDENTITY" --options runtime --timestamp --identifier "$(basename "$1")" "$1" \
		>/dev/null 2>"$OUT/codesign.err" || {
		sed 's/^/release: codesign: /' "$OUT/codesign.err" >&2
		die "codesign failed for $1"
	}
}
# verify_binary: the seal, the hardened runtime, and (signed) the team.
verify_binary() {
	codesign --verify --strict "$1" || die "$1 does not verify"
	codesign -dv "$1" 2>&1 | grep -q '^CodeDirectory .*flags=.*runtime' || die "$1 lacks the hardened runtime"
	if [ -z "$ADHOC" ]; then
		[ "$(codesign -dv "$1" 2>&1 | sed -n 's/^TeamIdentifier=//p')" = "$TEAM" ] ||
			die "$1 is not signed by the approver profile's team: SEAMLESS_SIGN_IDENTITY and SEAMLESS_APPROVER_PROFILE disagree"
	fi
}
while IFS= read -r bin; do
	sign "$bin"
	verify_binary "$bin"
done <"$OUT/binaries.txt"
say "signed $(grep -c . "$OUT/binaries.txt") binaries"

# --- notarize: one submission for every binary. A bare Mach-O cannot carry
# a stapled ticket; Gatekeeper finds the ticket online.
if [ -z "$ADHOC" ]; then
	ditto -c -k --keepParent "$OUT/bin" "$OUT/notarize.zip"
	if ! xcrun notarytool submit "$OUT/notarize.zip" --keychain-profile "$NOTARY_PROFILE" \
		--wait --timeout 30m --output-format json >"$OUT/notary.json"; then
		cat "$OUT/notary.json" >&2 || true
		die "notarytool submit failed"
	fi
	status=$(plutil -extract status raw -o - "$OUT/notary.json" 2>/dev/null || true)
	id=$(plutil -extract id raw -o - "$OUT/notary.json" 2>/dev/null || true)
	[ -n "$id" ] && xcrun notarytool log "$id" --keychain-profile "$NOTARY_PROFILE" "$OUT/notary-log.json" >/dev/null 2>&1 || true
	if [ "$status" != Accepted ]; then
		[ -f "$OUT/notary-log.json" ] && cat "$OUT/notary-log.json" >&2
		die "Apple did not accept the binaries: ${status:-unknown} (submission ${id:-unknown})"
	fi
	rm -f "$OUT/notarize.zip"
	say "notarized the binaries (submission $id: Accepted)"
fi

# --- package: .goreleaser.yaml's archive, for darwin -----------------------
"$RK" darwin-archive -tag "$TAG" -bin "$OUT/bin" -out "$OUT/assets" >"$OUT/archives.txt"

# --- the Touch ID approver: built with the same identity, notarized and
# stapled (scripts/approver/).
if [ -z "$ADHOC" ]; then
	APPROVER_SIGN_IDENTITY=$IDENTITY APPROVER_TEAM_ID=$TEAM APPROVER_PROFILE=$APPROVER_PROFILE_FILE \
		APPROVER_VERSION=$VERSION APPROVER_OUT="$OUT/approver" \
		scripts/approver/build.sh
	env -u APPROVER_NOTARY_KEY -u APPROVER_NOTARY_KEY_ID -u APPROVER_NOTARY_ISSUER_ID \
		APPROVER_NOTARY_PROFILE="$NOTARY_PROFILE" APPROVER_ZIP="$OUT/assets/$APPROVER_ASSET" \
		scripts/approver/notarize.sh "$OUT/approver/Seamless Approve.app"
fi

# --- what ships: unpack each archive and check its binaries again.
while IFS= read -r archive; do
	dir=$OUT/check/$(basename "$archive" .tar.gz)
	mkdir -p "$dir"
	tar -xzf "$archive" -C "$dir"
	for b in seamlessd seam; do
		[ -f "$dir/$b" ] || die "$(basename "$archive") holds no $b"
		verify_binary "$dir/$b"
	done
done <"$OUT/archives.txt"
rm -rf "$OUT/check"

# --- the manifest, as the tag's message carries it --------------------------
"$RK" manifest "$OUT"/assets/* >"$OUT/manifest.txt"
{
	printf '%s\n\n' "$TAG"
	if [ -n "$ADHOC" ]; then
		printf 'macOS assets, built and ad-hoc signed by scripts/release/macos.sh --adhoc (sha256):\n'
	else
		printf 'macOS assets, built, signed and notarized by scripts/release/macos.sh (sha256):\n'
	fi
	cat "$OUT/manifest.txt"
} >"$OUT/tag-message.txt"
"$RK" check-darwin -tag "$TAG" -tag-object "$OUT/tag-message.txt" -assets "$OUT/assets" >/dev/null

if [ -n "$DRY" ]; then
	say "dry run done; nothing was uploaded and no tag was made. $OUT/assets:"
	cat "$OUT/manifest.txt" >&2
	exit 0
fi

# --- the draft --------------------------------------------------------------
if [ -z "$DRAFT_ID" ]; then
	DRAFT_ID=$(gh api -X POST "repos/$REPO/releases" -f tag_name="$TAG" -f target_commitish="$HEAD" \
		-f name="$TAG" -F draft=true \
		-f body="The macOS assets of $TAG. release.yml replaces these notes with the changelog when it publishes." \
		--jq .id)
	say "created the draft release $TAG ($DRAFT_ID)"
fi
case $DRAFT_ID in
*[!0-9]* | '') die "draft release id $DRAFT_ID is not a number" ;;
esac
# The draft holds exactly these assets: an earlier run's copies are
# replaced (--clobber; gh finds a draft by its pending tag), and anything
# else is the owner's to look at first.
gh api "repos/$REPO/releases/$DRAFT_ID/assets" --paginate --jq '.[].name' >"$OUT/draft-assets.txt"
while IFS= read -r name; do
	[ -f "$OUT/assets/$name" ] || die "the draft $TAG holds $name, which this run did not make; remove it, then run again"
done <"$OUT/draft-assets.txt"
gh release upload "$TAG" --repo "$REPO" --clobber "$OUT"/assets/*
say "uploaded $(cd "$OUT/assets" && echo *)"
# Read the draft back the way release.yml will, and hold it to the manifest.
scripts/release/fetch-draft.sh "$TAG" "$OUT/draft" "$REPO"
"$RK" check-darwin -tag "$TAG" -tag-object "$OUT/tag-message.txt" -assets "$OUT/draft" >/dev/null

# --- the tag: SSH-signed and annotated, with the manifest -------------------
git tag -s "$TAG" -F "$OUT/tag-message.txt" "$HEAD"
git tag -v "$TAG" >/dev/null 2>&1 ||
	die "git tag -v does not verify $TAG (gpg.format ssh, gpg.ssh.allowedSignersFile); fix it, git tag -d $TAG, and run again"
git cat-file tag "$TAG" >"$OUT/tag-object.txt"
"$RK" check-darwin -tag "$TAG" -tag-object "$OUT/tag-object.txt" -assets "$OUT/draft" >/dev/null

say "ready: the draft $TAG holds the macOS assets, and the signed tag $TAG names them:"
cat "$OUT/manifest.txt" >&2
cat >&2 <<EOF
release: next, push the tag on its own (never together with main):
  git push origin $TAG
then approve the release environment in the run: https://github.com/$REPO/actions/workflows/release.yml
once its goreleaser job has filled the draft, sign it with your hardware key:
  SEAMLESS_RELEASE_KEY=<key handle> scripts/release/sign.sh $TAG
then approve the run's publish job, and once it has published:
  scripts/release/cask.sh $TAG
EOF
