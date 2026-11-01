package screening

import (
	"slices"
	"strings"

	"github.com/arctop/seamless/internal/core"
)

// Hold reasons that are not screen verdicts: who wrote, and what the write
// retires. They sit in the same reasons list as the verdict codes.
const (
	// ReasonTaintedSession: a session marked as having read outside text wrote
	// (or deleted) a briefing-injected kind.
	ReasonTaintedSession Code = "session.tainted"
	// ReasonConstraintSupersede: the write supersedes a constraint.
	ReasonConstraintSupersede Code = "constraint.supersede"
	// ReasonConstraintDelete: the write deletes or archives a constraint.
	ReasonConstraintDelete Code = "constraint.delete"
	// ReasonConstraintDemote: the write turns a constraint into another kind,
	// which drops it from the binding section of every briefing.
	ReasonConstraintDemote Code = "constraint.demote"
	// ReasonLockdown: lockdown is on, and the write touches a
	// briefing-injected kind (writes it, replaces it, supersedes or deletes
	// it). Every such write waits for the owner until lockdown lifts.
	ReasonLockdown Code = "lockdown"
)

// Reasons lists the non-verdict hold reasons.
var Reasons = []Code{ReasonTaintedSession, ReasonConstraintSupersede, ReasonConstraintDelete, ReasonConstraintDemote, ReasonLockdown}

// BriefingKinds are the memory kinds that bind or steer every session:
// constraints and conventions (each its own briefing section), stages (pinned
// while a gate is live), and protocols -- the coordination contracts agents
// follow, which today ride the briefing's memory index like any other kind but
// carry the same weight. A session marked as having read outside text cannot
// write them without the owner's release.
var BriefingKinds = []core.MemoryKind{core.KindConstraint, core.KindConvention, core.KindProtocol, core.KindStage}

// IsBriefingKind reports whether k is a briefing-injected kind.
func IsBriefingKind(k core.MemoryKind) bool { return slices.Contains(BriefingKinds, k) }

// MemoryText is what the screens read of a memory: everything an agent is
// shown -- the description every index line carries, the body, and the tags.
// The name is a validated slug and cannot carry any of it.
func MemoryText(m core.Memory) string {
	return m.Description + "\n" + m.Body + "\n" + strings.Join(m.Tags, " ")
}

// NoteText is what the screens read of a note.
func NoteText(n core.Note) string {
	return n.Title + "\n" + n.Description + "\n" + n.SourceURL + "\n" + n.Body + "\n" + strings.Join(n.Tags, " ")
}

// Verdict is one write's screening outcome.
type Verdict struct {
	// Introduced are the verdict codes the write adds over what it replaces.
	Introduced []Code
	// Reasons are the codes that hold it: the introduced verdicts that hold,
	// plus any non-verdict reason. Empty means the write lands.
	Reasons []Code
}

// Held reports whether the write is held.
func (v Verdict) Held() bool { return len(v.Reasons) > 0 }

// Strings renders codes for storage and events.
func Strings(codes []Code) []string {
	out := make([]string, len(codes))
	for i, c := range codes {
		out[i] = string(c)
	}
	return out
}

// Judge screens a write's new text against the text it replaces (base is ""
// for a create) and folds in the non-verdict reasons the caller determined.
func Judge(newText, baseText string, rules Rules, extra ...Code) Verdict {
	found := Scan(newText, rules)
	var base Findings
	if baseText != "" {
		base = Scan(baseText, rules)
	}
	intro := found.Introduced(base)
	reasons := HoldCodes(intro)
	for _, r := range extra {
		if r != "" && !slices.Contains(reasons, r) {
			reasons = append(reasons, r)
		}
	}
	return Verdict{Introduced: intro, Reasons: reasons}
}

// MemoryReasons are the non-verdict reasons a memory write holds for: a
// tainted session writing a briefing-injected kind (the kind it writes, or
// the kind it replaces), and a constraint turned into another kind.
func MemoryReasons(tainted bool, newKind, oldKind core.MemoryKind) []Code {
	var out []Code
	if tainted && (IsBriefingKind(newKind) || IsBriefingKind(oldKind)) {
		out = append(out, ReasonTaintedSession)
	}
	if oldKind == core.KindConstraint && newKind != core.KindConstraint {
		out = append(out, ReasonConstraintDemote)
	}
	return out
}
