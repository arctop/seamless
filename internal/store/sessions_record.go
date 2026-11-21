package store

// A conversation's own record with isolation.requests on (plan
// fenced-sessions): what the store keeps about the sessions its record did not
// go to, and what a move's child reports as its own.
//
//   - Where a record does not go, the session is left a metadata-only line in
//     its findings: a move's pointer on the session the move ended
//     (MovePointer), or the not-recorded line where the clearance rule refuses
//     a conversation's findings in its home (UnrecordedFindings). Each names
//     projects and sessions, never content. Neither is a finding: the
//     briefing's Recent findings and Sibling findings skip a session whose
//     findings are only such a line, as they skip the no-summary sentinel. With
//     isolation.requests off no session holds either, so both sections read
//     exactly as before.
//   - A move's child continues a conversation whose earlier sessions already
//     hold the token totals they harvested, while a transcript reports the
//     whole conversation's: the usage ledger splits it among them, so the
//     conversation's usage is counted once (ledger.go, usageOwner and
//     CreditConversationUsage).

import (
	"regexp"
	"strings"
)

// UnrecordedFindings is the line a session's findings become when the
// clearance rule refuses its conversation's own record where it works: who
// holds what -- holder is "session", or "agent" under peer_auth, where the
// agent's process tree is the holder -- and where (where reads "project
// <slug>" or "the global scope") it may not be written. It names projects
// only.
func UnrecordedFindings(holder string, label []string, where string) string {
	return "Not recorded: this " + holder + " holds content from " + strings.Join(label, ", ") +
		", and its findings may not be written into " + where + "."
}

// recordLineBuilders build the metadata-only lines (see the file comment), each
// from two placeholder arguments: recordLineShapes and IsRecordLine derive
// their patterns from the builders themselves, so a reworded line keeps
// matching.
var recordLineBuilders = []func(a, b string) string{
	func(a, b string) string { return MovePointer(a, b) },
	func(a, b string) string { return UnrecordedFindings(a, []string{b}, a) },
}

const (
	recordArgA = "\x00"
	recordArgB = "\x01"
)

// recordLineShapes are the LIKE patterns (ESCAPE '\') of the lines in
// recordLineBuilders: each one's fixed text escaped, each argument a
// wildcard. A pattern matches a session's whole findings, so an agent's own
// findings with a pointer after them still count as findings.
var recordLineShapes = func() []any {
	out := make([]any, 0, len(recordLineBuilders))
	wild := strings.NewReplacer(recordArgA, "%", recordArgB, "%")
	for _, build := range recordLineBuilders {
		out = append(out, wild.Replace(escapeLikePrefix(build(recordArgA, recordArgB))))
	}
	return out
}()

// recordLinePatterns are recordLineShapes as anchored regular expressions, for
// IsRecordLine.
var recordLinePatterns = func() []*regexp.Regexp {
	out := make([]*regexp.Regexp, 0, len(recordLineBuilders))
	for _, build := range recordLineBuilders {
		quoted := regexp.QuoteMeta(build(recordArgA, recordArgB))
		quoted = strings.NewReplacer(recordArgA, "(?s:.*)", recordArgB, "(?s:.*)").Replace(quoted)
		out = append(out, regexp.MustCompile(`\A`+quoted+`\z`))
	}
	return out
}()

// IsRecordLine reports whether findings are only one of the metadata-only
// lines a conversation's record routing leaves (see the file comment).
func IsRecordLine(findings string) bool {
	for _, re := range recordLinePatterns {
		if re.MatchString(findings) {
			return true
		}
	}
	return false
}

// recordLineFilter is the SQL that keeps a session whose findings are only a
// metadata-only line out of a findings section; it takes recordLineShapes as
// its arguments.
var recordLineFilter = strings.Repeat(` AND findings NOT LIKE ? ESCAPE '\'`, len(recordLineBuilders))
