package core

import (
	"encoding/json"
	"slices"
	"strings"
)

// Provenance stamps (isolation.requests, plan fenced-sessions P6; owner
// decision N2 (a)). An item a conversation writes while its label holds a
// fenced project other than the item's own records, for each such project,
// that it may contain content from it, and what cleared the write: the grant
// or move approval whose flow let that project's content into the item's
// project, or -- for a fenced home native to it -- the conversation's own
// session there. A stamp names projects and approvals, never content, and it
// only warns: nothing reads it to open or confine anything.
//
// Memories and notes carry their stamps in frontmatter (may_contain), mirrored
// into store's item_provenance index; tasks and session findings only in the
// index. A stamp is set from the writing conversation's state, never from
// agent input; no agent write removes one, and a supersession gives the
// replacement the union of both memories' stamps.

// What cleared a stamped write (Stamp.Via).
const (
	// StampViaGrant is a grant approval (read, or read and write).
	StampViaGrant = "grant"
	// StampViaMove is a move approval.
	StampViaMove = "move"
	// StampViaSession is the conversation's own session in the project: a
	// fenced home keeps its record whatever its own sessions reached, and no
	// approval was asked for (store.RecordDestinationOf's native-home rule).
	StampViaSession = "session"
)

// StampVias is the canonical Via set: also the frontmatter keys an entry names
// its reference under.
var StampVias = []string{StampViaGrant, StampViaMove, StampViaSession}

// Stamp is one provenance stamp: the item may contain content from Project,
// cleared by Via Ref -- the approval's id for a grant or a move, the session's
// for a fenced home's own session.
type Stamp struct {
	Project string
	// Via is one of StampVias, or "" for an entry this release does not know
	// (a hand-written one, or one a newer release wrote): it is kept and
	// rendered without its reference.
	Via string
	Ref string
	// Extra keeps a frontmatter entry's keys this release does not know, so
	// a rewrite never drops what a newer release added to an entry.
	Extra map[string]any
}

// MarshalJSON renders a stamp the way frontmatter carries it: the project,
// and the reference under its Via ({"project": "example-legal", "grant":
// "01..."}).
func (s Stamp) MarshalJSON() ([]byte, error) {
	out := map[string]string{"project": s.Project}
	if s.Via != "" && s.Ref != "" {
		out[s.Via] = s.Ref
	}
	return json.Marshal(out)
}

// UnmarshalJSON reads what MarshalJSON writes.
func (s *Stamp) UnmarshalJSON(b []byte) error {
	var raw map[string]string
	if err := json.Unmarshal(b, &raw); err != nil {
		return err
	}
	*s = Stamp{Project: raw["project"]}
	for _, via := range StampVias {
		if ref := raw[via]; ref != "" {
			s.Via, s.Ref = via, ref
			break
		}
	}
	return nil
}

// ShortRef is the stamp's reference as a person reads it: a grant's or a
// move's approval by its request's short form (R- and its last four symbols,
// as the Access requests page and seam approve name it), a session by the last
// eight symbols of its id (as the console names it).
func (s Stamp) ShortRef() string {
	switch {
	case s.Ref == "":
		return ""
	case s.Via == StampViaSession:
		if len(s.Ref) <= 8 {
			return s.Ref
		}
		return s.Ref[len(s.Ref)-8:]
	case len(s.Ref) < 4:
		return "R-" + s.Ref
	}
	return "R-" + s.Ref[len(s.Ref)-4:]
}

// Label is the stamp as one phrase: "example-legal (grant R-ABCD)". An entry
// with no reference is its project alone; one whose Via this release does
// not know names its reference as an approval.
func (s Stamp) Label() string {
	short := s.ShortRef()
	if short == "" {
		return s.Project
	}
	via := s.Via
	if via == "" {
		via = "approval"
	}
	return s.Project + " (" + via + " " + short + ")"
}

// MayContainLine is a text rendering's one line for stamps: "may contain
// content from example-legal (grant R-ABCD), example-finance (move R-1234)",
// or "" for none.
func MayContainLine(stamps []Stamp) string {
	if len(stamps) == 0 {
		return ""
	}
	labels := make([]string, len(stamps))
	for i, s := range stamps {
		labels[i] = s.Label()
	}
	return "may contain content from " + strings.Join(labels, ", ")
}

// MergeStamps returns have with each stamp of add whose project have does not
// already name appended, sorted by project among themselves, and whether it
// added any. have keeps its order and its entries, including one for project
// (an item moved into a project it was stamped for keeps the stamp, and shows
// it again once it moves out). A stamp for project itself, the global scope
// or no reference is never added. The first stamp for a project stays: a
// later write that names another approval for it changes nothing.
func MergeStamps(have, add []Stamp, project string) ([]Stamp, bool) {
	var fresh []Stamp
	for _, s := range add {
		if s.Project == "" || s.Project == project || s.Ref == "" {
			continue
		}
		named := func(o Stamp) bool { return o.Project == s.Project }
		if slices.ContainsFunc(have, named) || slices.ContainsFunc(fresh, named) {
			continue
		}
		fresh = append(fresh, Stamp{Project: s.Project, Via: s.Via, Ref: s.Ref})
	}
	if len(fresh) == 0 {
		return have, false
	}
	slices.SortFunc(fresh, func(a, b Stamp) int { return strings.Compare(a.Project, b.Project) })
	out := make([]Stamp, 0, len(have)+len(fresh))
	return append(append(out, have...), fresh...), true
}

// StampsShown is the stamps an item in project shows: every entry but one for
// project itself (an item moved into a project it may contain content from
// needs no warning about it there) or for no project at all.
func StampsShown(stamps []Stamp, project string) []Stamp {
	var out []Stamp
	for _, s := range stamps {
		if s.Project != "" && s.Project != project {
			out = append(out, s)
		}
	}
	return out
}
