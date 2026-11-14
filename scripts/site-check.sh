#!/bin/sh
# site-check: assert the hand-written landing page still tells the truth.
#
# Why this exists: docs-check gates docs-src/ -> docs/docs/ and nothing else.
# The landing page is hand-written -- docsgen never emits it, no test reads it,
# no target diffs it -- so it can contradict the product indefinitely with the
# whole gate green. That is not hypothetical. The curl|sh installer landed in
# d1e926b with README.md, docs-src/, and the regenerated docs/docs/ all correct
# and `make check` green, while the hero pill went on telling every visitor to
# run `go install github.com/arctop/seamless/cmd/...@latest`. The front door
# advertised the old front door for a day.
#
# Fourteen assertions, each one a thing a machine can actually verify:
#
#   1. the hero pills run $INSTALL_CMD and $WIN_INSTALL_CMD, not other routes
#   2. every surface that teaches installing teaches the SAME two commands
#   3. every `seamlessd <sub>` the page names in a command context is real
#   4. each copy button copies the command it visibly shows
#   5. each static asset on the hand-written pages (the landing page, the
#      /compare/ hub, and the /claude-app/ page) carries a content-hash ?v=
#      cache-buster that matches
#   6. the head is complete and the canonicals match docs/CNAME
#   7. exactly one JSON-LD block, braces balanced, with the required types
#   8. the JSON-LD FAQPage mirrors the visible #faq section
#   9. every scene outcome in scenes.js appears verbatim in the SSR fallbacks
#  10. native service commands live only on their two reference homes
#  11. the setup configurator composes from the two canonical commands
#  12. the landing bootstrap folds the docs' localStorage("os") values back
#      to its coarse unix|windows pair
#  13. the landing OS switch stores the UA-refined canonical value (macos or
#      linux, never the coarse unix) so the docs honor the pick
#  14. the installers carry the repository's release_signers, line for line
#
# Deliberately not a prose linter: claims like "one binary, no ceremony" are the
# author's problem. Commands are checkable, so these are checked.

set -eu

PAGE=docs/index.html
COMPARE=docs/compare/index.html
# The Claude app page: Seamless for chat work that isn't code. It teaches the
# install and names seamlessd subcommands, so assertions 2-6 cover it too.
CLAUDEAPP=docs/claude-app/index.html
MAIN=cmd/seamlessd/main.go

# The canonical install commands, and the single place they are written down.
# They are marketing claims as much as commands, so changing one should be
# deliberate and should drag every surface along: edit it here, and this check
# names every file that still disagrees.
INSTALL_CMD='curl -fsSL https://thereisnospoon.org/install | sh'
WIN_INSTALL_CMD='irm https://thereisnospoon.org/install.ps1 | iex'

# Every doc whose job includes telling a new user how to install. webmcp.js is
# here because its list_agent_resources tool teaches both commands to agents.
SURFACES="$PAGE $CLAUDEAPP README.md docs-src/quickstart.md docs-src/install.md docs/static/webmcp.js"

fail=0
err() {
	printf 'site-check: %s\n' "$1" >&2
	fail=1
}

# 1. The hero pills are the first commands anyone sees. One per OS, and each
#    must be the real one -- no more, no less.
pills=$(grep 'class="install-pill"' "$PAGE" | grep -oE 'data-copy="[^"]*"' |
	sed 's/^data-copy="//; s/"$//')
if [ -z "$pills" ]; then
	err "no install-pill with a data-copy found in $PAGE (did the hero markup change?)"
else
	printf '%s\n' "$pills" | grep -qxF "$INSTALL_CMD" ||
		err "no hero install pill runs [$INSTALL_CMD]"
	printf '%s\n' "$pills" | grep -qxF "$WIN_INSTALL_CMD" ||
		err "no hero install pill runs [$WIN_INSTALL_CMD]"
	stray=$(printf '%s\n' "$pills" | grep -vxF "$INSTALL_CMD" | grep -vxF "$WIN_INSTALL_CMD" || true)
	[ -z "$stray" ] || err "hero install pill runs unexpected command [$stray]"
fi

# 2. Nobody should have to wonder which install command is the current one --
#    on either OS.
for f in $SURFACES; do
	grep -qF "$INSTALL_CMD" "$f" ||
		err "$f never teaches the install command [$INSTALL_CMD]"
	grep -qF "$WIN_INSTALL_CMD" "$f" ||
		err "$f never teaches the Windows install command [$WIN_INSTALL_CMD]"
done

# 3. Commands the page names must exist. Scoped to command contexts -- copy
#    buttons and <code> spans -- because prose says things like "the seamlessd
#    binary", and the second word of a sentence is not a subcommand. A command
#    spelled with its install path (~/.local/bin/seamlessd console-open, which
#    the Claude app page uses because macOS does not put ~/.local/bin on PATH)
#    is folded back to the bare name before the lookup.
known=$(sed -n '/switch cmd {/,/^	default:/p' "$MAIN" |
	grep -oE '"[a-z][a-z-]*"' | tr -d '"' | sort -u)
for f in "$PAGE" "$CLAUDEAPP"; do
	named=$({
		grep -oE 'data-copy="[^"]*"' "$f" | sed 's/^data-copy="//; s/"$//'
		grep -oE '<code[^>]*>[^<]*</code>' "$f" | sed 's/<code[^>]*>//; s|</code>||'
	} | sed -E 's#^[^ ]*[/\\]seamlessd(\.exe)? #seamlessd #' |
		grep -E '^seamlessd ' | awk '{print $2}' | sort -u)
	for c in $named; do
		printf '%s\n' "$known" | grep -qx -- "$c" ||
			err "$f names [seamlessd $c], which $MAIN does not dispatch"
	done
done

# 4. A copy button reads its data-copy attribute, never the text beside it, so
#    the two drift silently and independently: the page shows one command and
#    the clipboard gets another. Nobody proof-reads an attribute.
for f in "$PAGE" "$CLAUDEAPP"; do
	drift=$(grep -oE 'data-copy="[^"]*">[^<]*<' "$f" |
		sed 's/^data-copy="//; s/<$//' |
		awk -F'">' '$1 != $2 { print "  shows [" $2 "] but copies [" $1 "]" }')
	if [ -n "$drift" ]; then
		err "$f copy buttons do not copy what they show:"
		printf '%s\n' "$drift" >&2
	fi
done

# 5. The page is served through a CDN (Cloudflare) that edge-caches static/ for
#    hours while passing the HTML through, so a deploy that changes site.css or
#    site.js keeps serving the stale asset behind the HTML until the TTL lapses.
#    That is exactly how the OS-switch shipped looking broken. A content-hash
#    ?v= makes a changed asset a new URL the edge never cached; enforce that the
#    stamped hash matches the file so a silent edit cannot un-bust it. `make
#    site-stamp` restamps.
sha8() {
	if command -v sha256sum >/dev/null 2>&1; then
		sha256sum "$1" | cut -c1-8
	else
		shasum -a 256 "$1" | cut -c1-8
	fi
}
stamped() {
	page=$1
	shift
	for asset in "$@"; do
		want=$(sha8 "docs/static/$asset")
		esc=$(printf '%s' "$asset" | sed 's/\./\\./g')
		got=$(grep -oE "static/$esc\?v=[0-9a-f]+" "$page" | head -1 | sed -E 's|.*\?v=||')
		if [ -z "$got" ]; then
			err "$page references static/$asset without a ?v= cache-buster (run 'make site-stamp')"
		elif [ "$got" != "$want" ]; then
			err "$page has static/$asset?v=$got but the file hashes to $want (run 'make site-stamp')"
		fi
	done
}
stamped "$PAGE" home.css home.js site.js scenes.js scenes-player.js webmcp.js
stamped "$COMPARE" site.css site.js webmcp.js
stamped "$CLAUDEAPP" home.css site.js webmcp.js

# 6. Head completeness. The docs pages get their head from docsgen and
#    cmd/docsgen/seo_test.go gates them; the landing page head is hand-written,
#    so only this check notices when the front door loses a tag or the canonical
#    drifts from the host GitHub Pages actually serves (docs/CNAME).
canon_host="https://$(tr -d '[:space:]' <docs/CNAME)/"
grep -qF "<link rel=\"canonical\" href=\"$canon_host\">" "$PAGE" ||
	err "$PAGE canonical does not match docs/CNAME [$canon_host]"
grep -qF "<link rel=\"canonical\" href=\"${canon_host}compare/\">" "$COMPARE" ||
	err "$COMPARE canonical does not match docs/CNAME [${canon_host}compare/]"
grep -qF "<link rel=\"canonical\" href=\"${canon_host}claude-app/\">" "$CLAUDEAPP" ||
	err "$CLAUDEAPP canonical does not match docs/CNAME [${canon_host}claude-app/]"
for tag in \
	'<meta name="description" content="' \
	'<meta name="robots" content="max-image-preview:large, max-snippet:-1">' \
	'<meta property="og:site_name" content="Seamless">' \
	'<meta property="og:type" content="' \
	'<meta property="og:url" content="' \
	'<meta property="og:title" content="' \
	'<meta property="og:description" content="' \
	'<meta property="og:image" content="' \
	'<meta property="og:image:width" content="' \
	'<meta property="og:image:height" content="' \
	'<meta property="og:image:alt" content="' \
	'<meta name="twitter:card" content="'; do
	grep -qF "$tag" "$PAGE" || err "$PAGE head is missing $tag"
	grep -qF "$tag" "$CLAUDEAPP" || err "$CLAUDEAPP head is missing $tag"
done

# 7. The JSON-LD block. Shell cannot validate JSON without a tool dependency
#    this script deliberately avoids, so balanced braces plus the required type
#    strings is the honest ceiling here; cmd/docsgen/seo_test.go does the real
#    json.Unmarshal round-trip for the generated pages. Date fields are banned
#    outright: there is no deterministic date source in this repo.
ld=$(sed -n '/<script type="application\/ld+json">/,/<\/script>/p' "$PAGE")
blocks=$(grep -c '<script type="application/ld+json">' "$PAGE" || true)
if [ "$blocks" != 1 ] || [ -z "$ld" ]; then
	err "$PAGE must carry exactly one JSON-LD block, found $blocks"
else
	open=$(printf '%s\n' "$ld" | grep -o '{' | wc -l | tr -d '[:space:]')
	close=$(printf '%s\n' "$ld" | grep -o '}' | wc -l | tr -d '[:space:]')
	[ "$open" = "$close" ] ||
		err "the JSON-LD block's braces do not balance ($open open vs $close close)"
	for typ in SoftwareApplication FAQPage; do
		printf '%s\n' "$ld" | grep -qF "\"@type\": \"$typ\"" ||
			err "the JSON-LD block declares no $typ"
	done
	if printf '%s\n' "$ld" | grep -qE 'datePublished|dateModified'; then
		err "the JSON-LD block carries a date field; there is no deterministic date source"
	fi
fi

# 8. The FAQPage mirrors the visible #faq section, or the JSON-LD quietly lies
#    the first time someone edits an answer. These assertions compare text
#    across an HTML-entity boundary: the page writes &hellip; while the JSON
#    carries literal UTF-8, so the shell normalizes the HTML side over the one
#    entity the summaries actually use, folding the section's shared "Why not
#    just use..." stem into the ellipsis-continuation summaries
#    (&hellip;Dosu? maps to the Question name "Why not just use Dosu?").
#
#    The extraction below reads 'grep -oE '"name": "[^"]*"'', which stops at
#    the FIRST double quote -- escaped or not. A JSON-escaped \" inside any
#    string would silently truncate the extracted value and let a broken check
#    pass, so any escaped quote in the block is a hard failure, not a style
#    preference.
faq_count=$(grep -c '<details class="rv">' "$PAGE" || true)
q_count=$(printf '%s\n' "$ld" | grep -c '"@type": "Question"' || true)
[ "$faq_count" = "$q_count" ] ||
	err "$PAGE shows $faq_count FAQ entries but the JSON-LD has $q_count Questions"
if printf '%s\n' "$ld" | grep -q '\\"'; then
	err "the JSON-LD block contains an escaped double quote, which this gate cannot parse; rephrase without quotes"
else
	names=$(printf '%s\n' "$ld" | grep -oE '"name": "[^"]*"' | sed 's/^"name": "//; s/"$//')
	while IFS= read -r s; do
		case "$s" in
		'&hellip;'*) want="Why not just use ${s#&hellip;}" ;;
		*) want="$s" ;;
		esac
		printf '%s\n' "$names" | grep -qxF "$want" ||
			err "FAQ summary [$s] has no JSON-LD Question named [$want]"
	done <<EOF
$(grep -oE '<summary>[^<]*</summary>' "$PAGE" | sed 's/<summary>//; s|</summary>||')
EOF
fi

# 9. The terminal scenes are rendered by JS, so the page carries a static
#    .scenes-fallback summary per scene -- that summary is all a crawler ever
#    reads of them. Its load-bearing lines are the pane outcomes, and they must
#    stay verbatim copies of scenes.js or the page quietly describes sessions
#    that never happened. Same entity caveat as check 8: the JS side is literal
#    UTF-8, so the HTML side is normalized over the entities an outcome could
#    plausibly be rewritten with before comparing.
SCENES=docs/static/scenes.js
if grep '"outcome"' "$SCENES" | grep -q '\\"'; then
	err "a scenes.js outcome contains an escaped double quote, which this gate cannot parse; rephrase without quotes"
else
	outcomes=$(grep -oE '"outcome": "[^"]*"' "$SCENES" | sed 's/^"outcome": "//; s/"$//')
	if [ -z "$outcomes" ]; then
		err "no outcome strings found in $SCENES (did its formatting change?)"
	else
		page_norm=$(sed 's/&mdash;/—/g; s/&rarr;/→/g; s/&hellip;/…/g; s/&rsquo;/’/g; s/&amp;/\&/g' "$PAGE")
		while IFS= read -r o; do
			case "$o" in
			*'<'* | *'>'*) err "scenes.js outcome [$o] contains < or >, which this gate cannot compare across HTML escaping; rephrase" ;;
			*)
				printf '%s\n' "$page_norm" | grep -qF "$o" ||
					err "scenes.js outcome is missing from the $PAGE scene fallbacks: [$o]"
				;;
			esac
		done <<EOF
$outcomes
EOF
	fi
fi

# 10. OS-specific service commands have exactly one explanatory home
#     (reference/service.md), plus the CLI reference that documents what the
#     seamlessd verbs wrap. Phrase-level mentions ("launchd, systemd, or a
#     Scheduled Task") are fine anywhere; a third page carrying the actual
#     commands is a half-updated copy waiting to happen.
strays=$(grep -rlE 'launchctl |systemctl --user|ScheduledTask' docs-src |
	grep -v 'docs-src/reference/service\.md' |
	grep -v 'docs-src/reference/cli-seamlessd\.md' || true)
[ -z "$strays" ] || err "OS service commands leaked outside their reference homes: $strays"

# 11. The setup configurator on the setup page composes its output from the
#     two canonical commands carried in data attributes. Pin those attributes
#     to canon, or the widget teaches a drifted command with every knob.
CFGPAGE=docs-src/quickstart.md
if grep -q 'class="cfg"' "$CFGPAGE"; then
	grep -qF "data-cmd-unix=\"$INSTALL_CMD\"" "$CFGPAGE" ||
		err "$CFGPAGE configurator data-cmd-unix is not the canonical [$INSTALL_CMD]"
	grep -qF "data-cmd-win=\"$WIN_INSTALL_CMD\"" "$CFGPAGE" ||
		err "$CFGPAGE configurator data-cmd-win is not the canonical [$WIN_INSTALL_CMD]"
else
	err "$CFGPAGE carries no setup configurator (class=\"cfg\" markup missing)"
fi

# 12. The docs picker widens localStorage("os") to macos/linux/windows/all;
#     the landing bootstrap must keep folding those back to its coarse unix
#     value, or a pick made in the docs silently resets on the landing page.
grep -qF 'if(o==="macos"||o==="linux")o="unix";' "$PAGE" ||
	err "$PAGE bootstrap lost the macos/linux -> unix localStorage(\"os\") mapping"

# 13. The landing switch stays 2-way (it shows exactly two commands), but what
#     it STORES must be the docs' canonical vocabulary: a "macOS / Linux" pick
#     that stored the coarse unix would be rejected by the docs bootstrap and
#     re-UA-detected -- inverting the pick for a Windows-UA user who chose
#     unix. site.js refines on write; the coarse value stays display-only.
SITEJS=docs/static/site.js
grep -qF '/Mac/i.test(navigator.userAgent) ? "macos" : "linux"' "$SITEJS" ||
	err "$SITEJS lost the UA-refine-on-write of the coarse unix pick"
if grep -qF 'setItem("os", btn.dataset.osPick)' "$SITEJS"; then
	err "$SITEJS stores the coarse os pick verbatim; store the UA-refined canonical value instead"
fi

# 14. The installers carry the repository's release_signers: the keys whose
#     signature over checksums.txt (checksums.txt.sshsig) every release after
#     the first signed one must carry. The site publishes from a release tag,
#     so the copies must be the file's at that tag, line for line (comments
#     and blank lines aside), or a fresh install would check a release
#     against keys the release workflow does not hold it to.
signers_want=$(grep -v '^[[:space:]]*#' release_signers | sed '/^[[:space:]]*$/d')
[ -n "$signers_want" ] || err "release_signers lists no key"
signers_sh=$(sed -n '/^# BEGIN release_signers$/,/^# END release_signers$/p' docs/install |
	sed '1d;$d' | sed "s/^OWNER_SIGNERS='//; s/'\$//")
[ "$signers_sh" = "$signers_want" ] ||
	err "docs/install's OWNER_SIGNERS (between # BEGIN/END release_signers) is not release_signers' key lines"
signers_ps=$(sed -n '/^# BEGIN release_signers$/,/^# END release_signers$/p' docs/install.ps1 |
	sed '1d;$d' | sed "/^\$OwnerSigners = @'$/d; /^'@$/d")
[ "$signers_ps" = "$signers_want" ] ||
	err "docs/install.ps1's \$OwnerSigners (between # BEGIN/END release_signers) is not release_signers' key lines"

[ "$fail" -eq 0 ] || exit 1
echo "site-check: $PAGE agrees with the installer, the CLI, and its own structured data"
