package retrieve

import (
	"strings"

	"github.com/arctop/seamless/internal/core"
)

// Account labels (peer_auth.restricted). A restricted local account is a
// second OS account whose agents work beside the owner's; every item it wrote
// -- a memory or note's current content, a session and its findings, a task it
// created, a trial it recorded -- carries its account name (core.*.Account),
// and every line this package renders for such an item says so with the
// label "written by account <name>".
//
// The label is not part of write screening's provenance marks
// (provenance.go): those render only while the screening feature is on, while
// the label renders whenever the item carries an account, feature or not. On a
// line it is its own bracketed suffix after the provenance mark, if any:
//
//	- name: description [agent-written, session cc/x] [written by account _example_agent]
//
// The owner's items carry no account, so every line renders byte-identically
// to an install without restricted accounts: no suffix, no space, no new text.
// Recall and search hits carry the sanitized account as Hit.Account (JSON
// "account", omitted when empty); a surface that prints a hit as text adds
// AccountLabel.

// accountLabelPrefix is the label's fixed text, lowercase, exactly as every
// surface renders it.
const accountLabelPrefix = "written by account "

// accountMaxRunes caps an account name in a label. OS account names are short
// (macOS and Linux both stop well below it); the cap is defense in depth for a
// value read back from a hand-edited frontmatter.
const accountMaxRunes = 64

// AccountLabel is the provenance label for an item a restricted account wrote,
// "written by account <name>", or "" for the owner's items (account ""). The
// name is sanitized (sanitizeAccount), so the label is safe to print anywhere
// an agent or the owner reads it.
func AccountLabel(account string) string {
	if account == "" {
		return ""
	}
	return accountLabelPrefix + sanitizeAccount(account)
}

// accountMark is the label as a line suffix: " [written by account <name>]",
// or "" for the owner's items, which keeps their lines byte-identical.
func accountMark(account string) string {
	if account == "" {
		return ""
	}
	return " [" + AccountLabel(account) + "]"
}

// sanitizeAccount renders an account name for a label. An OS account name is
// letters, digits and _ . - (what the kernel-named socket peer resolves to),
// so anything else -- whitespace, brackets, punctuation a hand edit put in the
// frontmatter -- becomes "_": the label can neither close its bracket early,
// nor run onto a second line, nor carry a sentence. It is not passed through
// the injection scrub (sanitizeField): with no spaces left there is no phrase
// to strip, and scrubbing would delete a name like "override" outright. A
// non-empty account always renders a non-empty name, capped at
// accountMaxRunes.
func sanitizeAccount(account string) string {
	var b strings.Builder
	for _, r := range account {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '_', r == '.', r == '-':
			b.WriteRune(r)
		default:
			b.WriteByte('_')
		}
	}
	return core.TruncateWords(b.String(), accountMaxRunes)
}
