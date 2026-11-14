#!/bin/sh
# Build the macOS Touch ID approver (macos/SeamApprove) into an app bundle:
#
#   $APPROVER_OUT/Seamless Approve.app     (default bin/approver/)
#
# `make approver` runs this. It is opt-in and macOS-only: nothing else in the
# build, install or release of seamlessd and seam depends on it, and a default
# install neither contains nor launches the helper (`seam touchid enroll`
# installs it).
#
# Two modes, chosen by the environment:
#
#   unsigned (none of the three below set): an ad-hoc signed bundle for
#     compile checks and CI. It carries no entitlements, so it has no key
#     store and says so when run.
#
#   signed (all three set):
#     APPROVER_SIGN_IDENTITY  the codesign identity (a name or SHA-1 hash in a
#                             keychain codesign can reach)
#     APPROVER_TEAM_ID        the 10-character team id the identity belongs to
#     APPROVER_PROFILE        path to the provisioning profile for the App ID
#                             com.arctop.seamless.approve (Developer ID
#                             for a release, development for a local test)
#     The team id is substituted into the entitlements, the profile is checked
#     against them and embedded at Contents/embedded.provisionprofile, and the
#     bundle is signed with the hardened runtime and a secure timestamp, then
#     verified. None of these values is printed.
#
# Optional:
#   APPROVER_VERSION    the version stamped into Info.plist (default 0.0.0-dev)
#   APPROVER_OUT        output directory (default <repo>/bin/approver)
#   APPROVER_TIMESTAMP  "none" signs without Apple's timestamp server (offline
#                       local tests only; notarization needs the timestamp)
#
# Notarization is a separate step: scripts/approver/notarize.sh.
set -eu

die() {
	echo "approver: $*" >&2
	exit 1
}

[ "$(uname -s)" = Darwin ] || die "the Touch ID approver builds on macOS only"

ROOT=$(cd "$(dirname "$0")/../.." && pwd)
PKG="$ROOT/macos/SeamApprove"
OUT=${APPROVER_OUT:-$ROOT/bin/approver}
APP="$OUT/Seamless Approve.app"
EXE=SeamApprove
BUNDLE_ID=com.arctop.seamless.approve
VERSION=${APPROVER_VERSION:-0.0.0-dev}

# CFBundleShortVersionString takes up to three dot-separated integers; the
# full version (a prerelease suffix included) rides in SeamlessVersion.
SHORT_VERSION=$(printf '%s\n' "$VERSION" | sed -n 's/^\([0-9][0-9]*\(\.[0-9][0-9]*\)\{0,2\}\).*/\1/p')
[ -n "$SHORT_VERSION" ] || die "APPROVER_VERSION=$VERSION does not start with a numeric version"
case "$VERSION" in
*[!A-Za-z0-9.+-]*) die "APPROVER_VERSION=$VERSION holds characters a version cannot" ;;
esac

# Signed or unsigned: all three or none.
set_count=0
for name in APPROVER_SIGN_IDENTITY APPROVER_TEAM_ID APPROVER_PROFILE; do
	if [ -n "$(printenv "$name" || true)" ]; then
		set_count=$((set_count + 1))
	fi
done
case $set_count in
0) MODE=unsigned ;;
3) MODE=signed ;;
*)
	missing=
	for name in APPROVER_SIGN_IDENTITY APPROVER_TEAM_ID APPROVER_PROFILE; do
		[ -n "$(printenv "$name" || true)" ] || missing="$missing $name"
	done
	die "a signed build needs APPROVER_SIGN_IDENTITY, APPROVER_TEAM_ID and APPROVER_PROFILE; missing:$missing"
	;;
esac

if [ "$MODE" = signed ]; then
	case "$APPROVER_TEAM_ID" in
	[A-Z0-9][A-Z0-9][A-Z0-9][A-Z0-9][A-Z0-9][A-Z0-9][A-Z0-9][A-Z0-9][A-Z0-9][A-Z0-9]) ;;
	*) die "APPROVER_TEAM_ID is not a 10-character team id" ;;
	esac
	[ -f "$APPROVER_PROFILE" ] || die "APPROVER_PROFILE does not name a file"
fi

TMP=$(mktemp -d "${TMPDIR:-/tmp}/seam-approver.XXXXXX")
trap 'rm -rf "$TMP"' EXIT INT TERM

# --- compile: arm64 only (Apple silicon), macOS 12 and later (Package.swift
# says why 12). No Intel Mac runs the approver, and Xcode deprecates x86_64
# for the current macOS deployment targets.
mkdir -p "$OUT"
SCRATCH="$OUT/.build"
swift build --package-path "$PKG" --scratch-path "$SCRATCH" -c release --arch arm64
BIN_DIR=$(swift build --package-path "$PKG" --scratch-path "$SCRATCH" -c release --arch arm64 --show-bin-path)
archs=$(lipo -archs "$BIN_DIR/$EXE")
[ "$archs" = arm64 ] || die "the binary is not arm64 only (has: $archs)"

# --- assemble the bundle.
rm -rf "$APP"
mkdir -p "$APP/Contents/MacOS" "$APP/Contents/Resources"
cp "$BIN_DIR/$EXE" "$APP/Contents/MacOS/$EXE"
sed -e "s/@SHORT_VERSION@/$SHORT_VERSION/g" -e "s/@VERSION@/$VERSION/g" "$PKG/Bundle/Info.plist" >"$APP/Contents/Info.plist"
printf 'APPL????' >"$APP/Contents/PkgInfo"
plutil -lint -s "$APP/Contents/Info.plist" || die "Info.plist does not lint"
[ "$(plutil -extract CFBundleIdentifier raw -o - "$APP/Contents/Info.plist")" = "$BUNDLE_ID" ] ||
	die "Info.plist does not carry the bundle id $BUNDLE_ID"
# The app icon is committed already rendered, so the build needs no renderer.
# After a change to the mark, regenerate it from the repository root with
#   swift scripts/approver/render-icon.swift macos/SeamApprove/Bundle/AppIcon.icns
cp "$PKG/Bundle/AppIcon.icns" "$APP/Contents/Resources/AppIcon.icns"
[ "$(plutil -extract CFBundleIconFile raw -o - "$APP/Contents/Info.plist")" = AppIcon ] ||
	die "Info.plist does not name the icon Contents/Resources/AppIcon.icns (CFBundleIconFile AppIcon)"

if [ "$MODE" = unsigned ]; then
	# Ad-hoc: seals the bundle so it runs (arm64 code must be signed) and so
	# codesign can verify it. No entitlements: macOS would refuse to launch an
	# app claiming a keychain group no profile authorizes.
	codesign --force --sign - --options runtime "$APP"
	codesign --verify --strict --deep "$APP"
	echo "approver: built $APP (unsigned: no key store; set APPROVER_SIGN_IDENTITY, APPROVER_TEAM_ID and APPROVER_PROFILE to sign)"
	exit 0
fi

# --- signed: check the profile authorizes exactly what will be signed.
TEAM=$APPROVER_TEAM_ID
APP_ID="$TEAM.$BUNDLE_ID"
PROFILE_PLIST="$TMP/profile.plist"
security cms -D -i "$APPROVER_PROFILE" >"$PROFILE_PLIST" 2>/dev/null ||
	die "APPROVER_PROFILE is not a provisioning profile (security cms could not decode it)"
pb() {
	/usr/libexec/PlistBuddy -c "Print :$1" "$2" 2>/dev/null
}
# allows PATTERN VALUE: a profile entitlement value is an exact value or a
# prefix ending in '*' (TEAM.*).
allows() {
	case "$2" in
	$1) return 0 ;;
	esac
	return 1
}

[ "$(pb TeamIdentifier:0 "$PROFILE_PLIST")" = "$TEAM" ] ||
	die "the provisioning profile belongs to another team than APPROVER_TEAM_ID"
p_appid=$(pb Entitlements:com.apple.application-identifier "$PROFILE_PLIST") ||
	die "the provisioning profile authorizes no com.apple.application-identifier"
allows "$p_appid" "$APP_ID" ||
	die "the provisioning profile is for another App ID than <team>.$BUNDLE_ID"
[ "$(pb Entitlements:com.apple.developer.team-identifier "$PROFILE_PLIST")" = "$TEAM" ] ||
	die "the provisioning profile does not authorize com.apple.developer.team-identifier for the team"
group_ok=
i=0
while g=$(pb "Entitlements:keychain-access-groups:$i" "$PROFILE_PLIST"); do
	if allows "$g" "$APP_ID"; then
		group_ok=1
	fi
	i=$((i + 1))
done
[ -n "$group_ok" ] ||
	die "the provisioning profile does not authorize the keychain access group <team>.$BUNDLE_ID (enable Keychain Sharing on the App ID and regenerate it)"
expires=$(plutil -extract ExpirationDate raw -o - "$PROFILE_PLIST" 2>/dev/null || true)
now=$(date -u +%Y-%m-%dT%H:%M:%SZ)
if [ -n "$expires" ] && [ "$(printf '%s\n%s\n' "$expires" "$now" | sort | head -1)" = "$expires" ]; then
	die "the provisioning profile expired ($expires)"
fi
# A development profile names the Macs it runs on; a Developer ID profile
# provisions all of them. Say so when this Mac is not covered (not fatal: the
# build may be for another Mac).
if [ "$(pb ProvisionsAllDevices "$PROFILE_PLIST" || true)" != true ]; then
	udid=$(system_profiler SPHardwareDataType 2>/dev/null | sed -n 's/.*Provisioning UDID: *//p' | head -1)
	if [ -z "$udid" ] || ! pb ProvisionedDevices "$PROFILE_PLIST" | grep -qx " *$udid"; then
		echo "approver: note: the provisioning profile does not list this Mac; the app will not launch here" >&2
	fi
fi

ENTS="$TMP/SeamApprove.entitlements"
sed -e "s/@TEAM_ID@/$TEAM/g" "$PKG/Bundle/SeamApprove.entitlements" >"$ENTS"
plutil -lint -s "$ENTS" || die "the entitlements do not lint"
cp "$APPROVER_PROFILE" "$APP/Contents/embedded.provisionprofile"

timestamp=--timestamp
if [ "${APPROVER_TIMESTAMP:-}" = none ]; then
	timestamp=--timestamp=none
fi
codesign --force --options runtime "$timestamp" --entitlements "$ENTS" --sign "$APPROVER_SIGN_IDENTITY" "$APP" >/dev/null 2>"$TMP/codesign.err" || {
	# codesign's error names the bundle and the problem, never the identity.
	sed 's/^/approver: codesign: /' "$TMP/codesign.err" >&2
	die "codesign failed"
}

# --- verify: the seal, the signer's team, the signed entitlements, and the
# embedded profile -- quietly, so no identity or team id reaches a log.
codesign --verify --strict --deep "$APP" || die "the signed bundle does not verify"
signed_team=$(codesign -dv "$APP" 2>&1 | sed -n 's/^TeamIdentifier=//p')
[ "$signed_team" = "$TEAM" ] || die "the signing identity's team is not APPROVER_TEAM_ID"
[ "$(codesign -dv "$APP" 2>&1 | sed -n 's/^Identifier=//p')" = "$BUNDLE_ID" ] ||
	die "the signature's identifier is not $BUNDLE_ID"
codesign -dv "$APP" 2>&1 | grep -q '^CodeDirectory .*flags=.*runtime' || die "the signature lacks the hardened runtime"
# The requirement `seam touchid enroll` pins (cmd/seam/touchid_install.go,
# approverRequirement): Apple's chain, this identifier, this team.
codesign --verify --strict --deep "-R=anchor apple generic and identifier \"$BUNDLE_ID\" and certificate leaf[subject.OU] = \"$TEAM\"" "$APP" 2>/dev/null ||
	die "the signature does not satisfy the requirement seam touchid enroll checks (an Apple-issued certificate of APPROVER_TEAM_ID)"
SIGNED="$TMP/signed.plist"
codesign -d --entitlements - --xml "$APP" >"$SIGNED" 2>/dev/null || die "cannot read the signed entitlements"
[ "$(pb com.apple.application-identifier "$SIGNED")" = "$APP_ID" ] || die "the signed application identifier is wrong"
[ "$(pb com.apple.developer.team-identifier "$SIGNED")" = "$TEAM" ] || die "the signed team identifier is wrong"
[ "$(pb keychain-access-groups:0 "$SIGNED")" = "$APP_ID" ] || die "the signed keychain access group is wrong"
if pb keychain-access-groups:1 "$SIGNED" >/dev/null; then
	die "the signature claims more than one keychain access group"
fi
if pb com.apple.security.get-task-allow "$SIGNED" >/dev/null; then
	die "the signature carries get-task-allow"
fi
cmp -s "$APPROVER_PROFILE" "$APP/Contents/embedded.provisionprofile" || die "the embedded profile is not APPROVER_PROFILE"
# The profile authorizes code signed by the certificates it lists
# (DeveloperCertificates, TN3125); an app signed by any other is refused at
# launch, after every check above has passed. That is what a renewed
# certificate with the old profile, or a Developer ID identity with a
# development profile, would ship.
codesign -d --extract-certificates="$TMP/signer" "$APP" 2>/dev/null || die "cannot read the signing certificate"
[ -f "$TMP/signer0" ] || die "the signature carries no certificate"
cert_ok=
i=0
while plutil -extract "DeveloperCertificates.$i" raw -o - "$PROFILE_PLIST" >"$TMP/listed.b64" 2>/dev/null; do
	if base64 -D -i "$TMP/listed.b64" -o "$TMP/listed.der" 2>/dev/null && cmp -s "$TMP/listed.der" "$TMP/signer0"; then
		cert_ok=1
		break
	fi
	i=$((i + 1))
done
[ -n "$cert_ok" ] ||
	die "the provisioning profile does not list the signing certificate (regenerate it after a certificate is renewed; pair a Developer ID identity with a Developer ID profile, a development identity with a development profile)"

echo "approver: built and signed $APP (version $VERSION); notarize it with scripts/approver/notarize.sh"
