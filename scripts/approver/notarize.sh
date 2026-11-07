#!/bin/sh
# Notarize and staple a signed Touch ID approver, then zip it for release:
#
#   scripts/approver/notarize.sh "<dir>/Seamless Approve.app"
#
# The app must come from scripts/approver/build.sh in signed mode (Developer
# ID identity, Developer ID provisioning profile). Apple's notary service
# needs an App Store Connect API key:
#
#   APPROVER_NOTARY_KEY        path to the key's .p8 file
#   APPROVER_NOTARY_KEY_ID     the key id
#   APPROVER_NOTARY_ISSUER_ID  the issuer id
#
# Optional:
#   APPROVER_ZIP   write the stapled app here as the release asset, zipped
#                  with ditto -c -k --keepParent so the zip's one top-level
#                  entry is "Seamless Approve.app" and its signature and
#                  stapled ticket survive (seam touchid enroll unpacks it).
#
# It fails unless Apple accepts the submission, the ticket staples and
# validates, and Gatekeeper accepts the stapled app. Nothing printed here
# names the signing identity (spctl runs without -v, which would).
set -eu

die() {
	echo "approver: $*" >&2
	exit 1
}

[ "$(uname -s)" = Darwin ] || die "notarization runs on macOS only"
[ $# -eq 1 ] || die "usage: notarize.sh <path to Seamless Approve.app>"
APP=$1
[ -d "$APP/Contents" ] || die "$APP is not an app bundle"
for name in APPROVER_NOTARY_KEY APPROVER_NOTARY_KEY_ID APPROVER_NOTARY_ISSUER_ID; do
	[ -n "$(printenv "$name" || true)" ] || die "$name is not set"
done
[ -f "$APPROVER_NOTARY_KEY" ] || die "APPROVER_NOTARY_KEY does not name a file"
codesign --verify --strict --deep "$APP" || die "$APP is not validly signed"

TMP=$(mktemp -d "${TMPDIR:-/tmp}/seam-notary.XXXXXX")
trap 'rm -rf "$TMP"' EXIT INT TERM

# notarytool takes a zip, a dmg or a pkg; the ticket is stapled to the app.
ditto -c -k --keepParent "$APP" "$TMP/submit.zip"
if ! xcrun notarytool submit "$TMP/submit.zip" \
	--key "$APPROVER_NOTARY_KEY" --key-id "$APPROVER_NOTARY_KEY_ID" --issuer "$APPROVER_NOTARY_ISSUER_ID" \
	--wait --timeout 20m --output-format json >"$TMP/result.json"; then
	cat "$TMP/result.json" >&2 || true
	die "notarytool submit failed"
fi
status=$(plutil -extract status raw -o - "$TMP/result.json" 2>/dev/null || true)
id=$(plutil -extract id raw -o - "$TMP/result.json" 2>/dev/null || true)
if [ "$status" != Accepted ]; then
	echo "approver: notarization status: ${status:-unknown} (submission ${id:-unknown})" >&2
	if [ -n "$id" ]; then
		xcrun notarytool log "$id" \
			--key "$APPROVER_NOTARY_KEY" --key-id "$APPROVER_NOTARY_KEY_ID" --issuer "$APPROVER_NOTARY_ISSUER_ID" >&2 || true
	fi
	die "Apple did not accept the approver"
fi
echo "approver: notarized (submission $id)"

xcrun stapler staple "$APP" >/dev/null || die "stapling the ticket failed"
xcrun stapler validate "$APP" >/dev/null || die "the stapled ticket does not validate"
codesign --verify --strict --deep "$APP" || die "the stapled app no longer verifies"
spctl --assess --type execute "$APP" 2>/dev/null || die "Gatekeeper does not accept the stapled app"

if [ -n "${APPROVER_ZIP:-}" ]; then
	mkdir -p "$(dirname "$APPROVER_ZIP")"
	rm -f "$APPROVER_ZIP"
	# --norsrc --noextattr --noqtn: no AppleDouble "._" entries. ditto -x
	# would fold them back into extended attributes, but any other unzipper
	# (Go's archive/zip included) writes them as files inside the bundle,
	# and codesign --verify --strict then fails with "a sealed resource is
	# missing or invalid". The signature, the embedded profile and the
	# stapled ticket are all ordinary files, so nothing is lost.
	ditto -c -k --keepParent --norsrc --noextattr --noqtn "$APP" "$APPROVER_ZIP"
	top=$(zipinfo -1 "$APPROVER_ZIP" | sed 's|/.*||' | sort -u)
	[ "$top" = "$(basename "$APP")" ] || die "the zip's top-level entry is not $(basename "$APP")"
	if zipinfo -1 "$APPROVER_ZIP" | grep -q -e '/\._' -e '^__MACOSX'; then
		die "the zip holds AppleDouble entries"
	fi
	check="$TMP/check"
	mkdir -p "$check"
	ditto -x -k "$APPROVER_ZIP" "$check"
	codesign --verify --strict --deep "$check/$(basename "$APP")" || die "the zipped app does not verify once unpacked"
	xcrun stapler validate "$check/$(basename "$APP")" >/dev/null || die "the zipped app lost its stapled ticket"
	echo "approver: wrote $APPROVER_ZIP"
fi
