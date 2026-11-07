#!/bin/sh
# vulncheck: the known-vulnerability gate behind `make vulncheck`.
#
# Fails on any vulnerability the code calls (what govulncheck's text mode
# fails on), except the advisories in `allowed` below, until `recheck_by`.
# An advisory belongs there only when the code is shown not to be exposed to
# it although govulncheck still sees the vulnerable calls, and the comment
# says how. From the re-check date the exception no longer applies and the
# gate fails until someone looks again: then move the date, or delete the
# entry.
#
# Usage: scripts/vulncheck.sh [package patterns]   (default ./...)
set -eu

# GO-2026-6911: mcp-go's client transport follows an MCP server's redirect
# with no policy, re-sending its headers to wherever Location points. No
# fixed mcp-go yet (checked 2026-11-06, v1.1.1). Every MCP client Seamless
# builds -- seam's dial, the mcp-proxy bridge, seamlessd doctor's probe,
# seambench -- sets CheckRedirect to config.RefuseRedirect, so none follows
# one (tests: *RefusesARedirect). Owner decision 2026-11-06.
allowed='GO-2026-6911'
recheck_by=2026-12-06

for tool in govulncheck jq; do
	command -v "$tool" >/dev/null || {
		echo "ERROR: $tool not found (govulncheck: go install golang.org/x/vuln/cmd/govulncheck@latest)" >&2
		exit 1
	}
done
[ "$#" -gt 0 ] || set -- ./...

if [ "$(date -u +%Y%m%d)" -ge "$(printf '%s' "$recheck_by" | tr -d -)" ]; then
	echo "vulncheck: the exception for $allowed lapsed on $recheck_by;" \
		"re-check the advisory, then move recheck_by in $0 or delete the entry" >&2
	allowed=''
fi

out=$(mktemp)
trap 'rm -f "$out" "$out.bad"' EXIT
# JSON mode exits 0 whatever it finds (but not on an error), so the findings
# are judged here: one with a function in its first frame is called code.
govulncheck -format json "$@" >"$out"
jq -r --arg ids "$allowed" '
	select(.finding and .finding.trace[0].function != null) | .finding
	| select(.osv as $id | ($ids | split(" ") | index($id)) == null)
	| .osv' "$out" >"$out.bad"
sort -u -o "$out.bad" "$out.bad"
if [ -s "$out.bad" ]; then
	echo "vulncheck: the code calls known vulnerabilities: $(tr '\n' ' ' <"$out.bad")" >&2
	# The readable report, traces and all; it fails, as it should.
	govulncheck "$@" >&2 || true
	exit 1
fi
for id in $allowed; do
	fix=$(jq -r --arg id "$id" 'select(.finding.osv == $id) | .finding.fixed_version // empty' "$out" | head -1)
	if [ -n "$fix" ]; then
		echo "vulncheck: $id now has a fix ($fix): upgrade, then delete its entry in $0" >&2
	fi
done
echo "vulncheck: no known vulnerabilities called${allowed:+ (allowed until $recheck_by: $allowed)}"
