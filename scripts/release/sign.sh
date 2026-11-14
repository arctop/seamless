#!/bin/sh
# Phase 2 of a release, in an attended session on the owner's machine, once
# release.yml's goreleaser job has filled the tag's draft: sign the draft's
# checksums.txt with the owner's hardware key and upload the signature,
# checksums.txt.sshsig, for the publish job to verify (plan
# agent-accounts-and-releases, step D). checksums.txt lists every archive,
# the macOS assets, and both installer scripts, so this one signature covers
# every byte a user runs.
#
#   scripts/release/sign.sh vX.Y.Z             sign and upload
#   scripts/release/sign.sh vX.Y.Z --dry-run   sign and verify, upload nothing
#
# Before anything is signed it:
#   - verifies the owner's own signed tag, with /usr/bin/ssh-keygen against
#     scripts/release/tag_signers as the tag's own tree holds it;
#   - holds the draft's checksums.txt to its Sigstore bundle by this tag's
#     release workflow, and its macOS lines to the manifest in the tag
#     (releasekit check-draft);
#   - shows the keys that may sign this release: those release_signers lists
#     at both the previous release's tag and this one, and any key this tag
#     adds or removes (releasekit owner-keys);
#   - asks the owner to type the tag.
# After signing it checks the signature the way the publish job will
# (releasekit verify-owner), uploads it, and reads it back.
#
# Environment:
#   SEAMLESS_RELEASE_SIGNER  the release signing helper that signs with the
#                          owner's hardware key, run under /usr/bin/env -i: its
#                          absolute path; `checksums checksums.txt` writes
#                          checksums.txt.sig in namespace seamless-release
#   SEAMLESS_RELEASE_SIGNER_KEY  the helper's --key (owner-yk1 or owner-yk2);
#                          unset, the helper's own default
#   SEAMLESS_RELEASE_KEY   sign with ssh-keygen instead of the helper: the path
#                          ssh-keygen -f takes, for a hardware key the key handle
#                          file (a fork, or a machine without the helper)
#   SEAMLESS_SSH_KEYGEN    that ssh-keygen (default: the one on PATH); it must
#                          be able to use the hardware key
#
# Either way the signature is the one release_signers names:
#   ssh-keygen -Y sign -n seamless-release -f <key> checksums.txt
set -eu

die() {
	echo "sign: $*" >&2
	exit 1
}
say() {
	echo "sign: $*" >&2
}

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
cd "$ROOT"

[ $# -ge 1 ] || die "usage: scripts/release/sign.sh vX.Y.Z [--dry-run]"
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
*[!A-Za-z0-9.-]* | '') die "$TAG is not a release tag (vX.Y.Z or vX.Y.Z-pre)" ;;
esac
printf '%s\n' "$TAG" | grep -Eq '^v[0-9]+\.[0-9]+\.[0-9]+(-[0-9A-Za-z.-]+)?$' ||
	die "$TAG is not a release tag (vX.Y.Z or vX.Y.Z-pre)"
NAMESPACE=seamless-release
KEYGEN=${SEAMLESS_SSH_KEYGEN:-ssh-keygen}
KEY=${SEAMLESS_RELEASE_KEY:-}
SIGNER=${SEAMLESS_RELEASE_SIGNER:-}
SIGNER_KEY=${SEAMLESS_RELEASE_SIGNER_KEY:-}
# An explicit key file signs with ssh-keygen; otherwise the helper does.
[ -z "$KEY" ] || SIGNER=none

# --- preflight -------------------------------------------------------------
for tool in git gh go; do
	command -v "$tool" >/dev/null 2>&1 || die "needs $tool"
done
case $SIGNER in
'') die "set SEAMLESS_RELEASE_SIGNER to the release signing helper's absolute path, or SEAMLESS_RELEASE_KEY to sign with ssh-keygen" ;;
none)
	command -v "$KEYGEN" >/dev/null 2>&1 || die "needs $KEYGEN"
	[ -n "$KEY" ] || die "set SEAMLESS_RELEASE_KEY to the key to sign with (for a hardware key, its key handle file), or leave SEAMLESS_RELEASE_SIGNER to the helper"
	[ -f "$KEY" ] || die "SEAMLESS_RELEASE_KEY=$KEY is not a file"
	SIGNED_WITH=$KEY
	;;
/*)
	[ -f "$SIGNER" ] && [ -x "$SIGNER" ] ||
		die "no release signing helper at $SIGNER (set SEAMLESS_RELEASE_SIGNER, or SEAMLESS_RELEASE_KEY to sign with ssh-keygen)"
	SIGNED_WITH="the release signing helper${SIGNER_KEY:+ ($SIGNER_KEY)}"
	;;
*) die "SEAMLESS_RELEASE_SIGNER=$SIGNER is neither an absolute path nor none" ;;
esac
case $SIGNER_KEY in
'' | owner-yk1 | owner-yk2) ;;
*) die "SEAMLESS_RELEASE_SIGNER_KEY=$SIGNER_KEY is neither owner-yk1 nor owner-yk2" ;;
esac
REPO=$(gh repo view --json nameWithOwner --jq .nameWithOwner)

# Every release tag, for release_signers at the previous release's tag, and
# this one as origin has it: the owner's signed tag, nothing else.
git fetch --quiet --tags origin
git fetch --quiet --force origin "refs/tags/$TAG:refs/tags/$TAG"
[ "$(git cat-file -t "$TAG")" = tag ] || die "$TAG is not an annotated tag: scripts/release/macos.sh writes a signed one"

WORK=$(mktemp -d "${TMPDIR:-/tmp}/seamless-sign.XXXXXX")
trap 'rm -rf "$WORK"' EXIT INT TERM

# The tag's signature, by /usr/bin/ssh-keygen (never the one on PATH) against
# tag_signers as the tag's own tree holds it (never the owner's global list):
# the list macos.sh checked the tag against when it made it.
git show "$TAG:scripts/release/tag_signers" >"$WORK/tag_signers" 2>/dev/null ||
	die "$TAG holds no scripts/release/tag_signers: scripts/release/macos.sh tags only a tree that has one"
git -c gpg.format=ssh -c gpg.ssh.program=/usr/bin/ssh-keygen \
	-c gpg.ssh.allowedSignersFile="$WORK/tag_signers" tag -v "$TAG" >/dev/null 2>&1 ||
	die "$TAG does not verify against scripts/release/tag_signers (with /usr/bin/ssh-keygen): sign only a tag you made"
go build -o "$WORK/releasekit" ./cmd/releasekit
RK=$WORK/releasekit

# --- what is being signed ----------------------------------------------------
scripts/release/fetch-draft.sh "$TAG" "$WORK/draft" "$REPO" checksums.txt checksums.txt.sigstore.json
git cat-file tag "$TAG" >"$WORK/tag-object.txt"
"$RK" check-draft -tag "$TAG" -tag-object "$WORK/tag-object.txt" \
	-checksums "$WORK/draft/checksums.txt" -bundle "$WORK/draft/checksums.txt.sigstore.json" >"$WORK/names.txt"
"$RK" owner-keys -require -tag "$TAG" >"$WORK/keys.txt"

echo >&2
say "$TAG's checksums.txt, as the draft holds it:"
sed 's/^/  /' "$WORK/draft/checksums.txt" >&2
echo >&2
say "the keys that may sign it (release_signers):"
sed 's/^/  /' "$WORK/keys.txt" >&2
if grep -Eq '^(ADDED|REMOVED|BOOTSTRAP)' "$WORK/keys.txt"; then
	echo >&2
	say "release_signers changes with this release (above): make sure every change is yours before you sign"
fi
echo >&2
[ -r /dev/tty ] || die "no terminal to confirm on: signing a release is an attended step"
printf 'sign: type %s to sign it with %s: ' "$TAG" "$SIGNED_WITH" >/dev/tty
read -r answer </dev/tty
[ "$answer" = "$TAG" ] || die "not confirmed; nothing signed"

# --- sign ------------------------------------------------------------------
mkdir "$WORK/sign"
cp "$WORK/draft/checksums.txt" "$WORK/sign/checksums.txt"
if [ "$SIGNER" = none ]; then
	"$KEYGEN" -Y sign -n "$NAMESPACE" -f "$KEY" "$WORK/sign/checksums.txt"
elif [ -n "$SIGNER_KEY" ]; then
	/usr/bin/env -i "$SIGNER" checksums "$WORK/sign/checksums.txt" --key "$SIGNER_KEY" ||
		die "the release signing helper did not sign checksums.txt"
else
	/usr/bin/env -i "$SIGNER" checksums "$WORK/sign/checksums.txt" ||
		die "the release signing helper did not sign checksums.txt"
fi
[ -f "$WORK/sign/checksums.txt.sig" ] || die "no checksums.txt.sig was written"
mv "$WORK/sign/checksums.txt.sig" "$WORK/sign/checksums.txt.sshsig"
"$RK" verify-owner -require -tag "$TAG" -checksums "$WORK/sign/checksums.txt" -sig "$WORK/sign/checksums.txt.sshsig"

if [ -n "$DRY" ]; then
	say "dry run done: the signature verifies as the publish job will check it; nothing was uploaded"
	exit 0
fi

# --- upload and read back ------------------------------------------------------
gh release upload "$TAG" --repo "$REPO" --clobber "$WORK/sign/checksums.txt.sshsig"
scripts/release/fetch-draft.sh "$TAG" "$WORK/back" "$REPO" checksums.txt checksums.txt.sshsig
cmp -s "$WORK/back/checksums.txt" "$WORK/sign/checksums.txt" || die "the draft's checksums.txt changed while it was signed"
cmp -s "$WORK/back/checksums.txt.sshsig" "$WORK/sign/checksums.txt.sshsig" || die "the draft does not serve the signature just uploaded"
"$RK" verify-owner -require -tag "$TAG" -checksums "$WORK/back/checksums.txt" -sig "$WORK/back/checksums.txt.sshsig" >/dev/null

cat >&2 <<EOF
sign: uploaded checksums.txt.sshsig to the draft $TAG.
sign: next, approve the publish job of the release run (Review deployments, release):
  https://github.com/$REPO/actions/workflows/release.yml
and once it has published:
  scripts/release/cask.sh $TAG
EOF
