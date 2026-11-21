package core

import (
	"slices"
	"strings"
	"unicode"
)

// MishapType is the kind of a mishap an agent self-reported at session_end,
// so a recurrence review can count reports by kind. An entry names its type
// as a prefix inside its own text ("shell-trap: a bare == in zsh died"), so
// the mishaps argument stays an array of strings and a report written before
// types existed reads exactly as it did: untyped.
type MishapType string

// The mishap types. The slugs are frozen: the event log keeps every report's
// type for good, and a listing filters by these words.
const (
	MishapShellTrap      MishapType = "shell-trap"
	MishapWrongTarget    MishapType = "wrong-target"
	MishapLiveState      MishapType = "live-state"
	MishapIgnoredWarning MishapType = "ignored-warning"
	MishapFabricated     MishapType = "fabricated"
	MishapOther          MishapType = "other"
)

// MishapTypes lists every type, in the order each surface lists them.
var MishapTypes = []MishapType{
	MishapShellTrap, MishapWrongTarget, MishapLiveState, MishapIgnoredWarning, MishapFabricated, MishapOther,
}

// MishapUntyped is the listing filter for the reports that name no type: every
// report written before types existed, and any whose text opens with no type.
// It is a filter word, never a type: no report is stored with it.
const MishapUntyped = "untyped"

// mishapPrefixMax bounds where a type prefix's colon may sit: within the
// entry's first bytes, so a colon deep in a sentence (a path, a URL, a
// "LOCK name ref:" line) never makes its opening words a type candidate.
const mishapPrefixMax = 32

// Valid reports whether t is one of MishapTypes.
func (t MishapType) Valid() bool { return slices.Contains(MishapTypes, t) }

// Meaning is the type's one-line meaning, as the owner reads it; "" for a
// value that is not a type.
func (t MishapType) Meaning() string {
	switch t {
	case MishapShellTrap:
		return "A shell or tool quirk broke a command: zsh = expansion, an unquoted variable, a glob, a pipe that hid a failure, a BSD/GNU difference."
	case MishapWrongTarget:
		return "A command or edit hit the wrong file, repository, branch, worktree, project, host or account."
	case MishapLiveState:
		return "Live state touched by mistake: a running daemon, its data or config, a service, the owner's machine or screen."
	case MishapIgnoredWarning:
		return "An action a warning, memory, convention or gate said not to take."
	case MishapFabricated:
		return "A made-up id, path, flag, command or fact used as if it were real."
	case MishapOther:
		return "None of these."
	}
	return ""
}

// ParseMishap reads a mishap entry's type prefix: the words before the
// entry's first colon, when that colon sits within its first bytes, compared
// after trimming, lowercasing and joining the words with '-' ("Shell trap:",
// "shell_trap:" and "shell-trap:" all name shell-trap). It returns the type
// and the text after the prefix, trimmed. An entry with no colon there, or
// whose opening words are no type ("note:", "violated some-memory:"), is an
// untyped report: typ is "" and rest is text itself. It never refuses an
// entry.
func ParseMishap(text string) (typ MishapType, rest string) {
	body := strings.TrimLeftFunc(text, unicode.IsSpace)
	i := strings.IndexByte(body, ':')
	if i <= 0 || i >= mishapPrefixMax {
		return "", text
	}
	candidate := MishapType(strings.Join(strings.Fields(strings.ToLower(strings.ReplaceAll(body[:i], "_", " "))), "-"))
	if !candidate.Valid() {
		return "", text
	}
	return candidate, strings.TrimSpace(body[i+1:])
}

// MishapText is a report's text for a reader that shows its type beside it:
// the description with its type prefix taken off, when the report's recorded
// type is t and the description opens with it. Any other description comes
// back as it is, so an untyped report reads exactly as it was written.
func MishapText(description string, t MishapType) string {
	if !t.Valid() {
		return description
	}
	if typ, rest := ParseMishap(description); typ == t {
		return rest
	}
	return description
}
