package console

import "github.com/arctop/seamless/internal/core"

// kindGlossary says, in one line each, what a memory of each kind holds. The
// Memories screen shows the line as a kind chip's and a row's kind tooltip,
// and under each kind group of a project's list, so a bare "stage" or
// "protocol" explains itself. The wording follows the kinds table in
// docs-src/concepts/memory.md; the test beside this file fails when a kind
// core accepts has no line, or a line names a kind core does not have.
var kindGlossary = map[core.MemoryKind]string{
	core.KindConstraint: "A rule any agent must follow, whatever the task. Briefings never drop it.",
	core.KindConvention: "A project's own choice or layout fact: naming, branding, where things live.",
	core.KindRunbook:    "A procedure that works, step by step.",
	core.KindProtocol:   "An agreed way of doing something, and why it was agreed.",
	core.KindGotcha:     "A trap and how to avoid it, led by its symptom.",
	core.KindDecision:   "A choice and its reasoning, with the alternatives it turned down.",
	core.KindRefuted:    "Something believed that turned out false, kept so nobody believes it again.",
	core.KindReference:  "A pointer to something outside Seamless, and what is there.",
	core.KindStage:      "Where a piece of work across sessions stands: its status and what it waits on.",
}

// kindGloss is the glossary line for a memory kind ("" for an unknown kind).
func kindGloss(kind string) string { return kindGlossary[core.MemoryKind(kind)] }

// KindGloss is the list row's kind tooltip.
func (m memoryRow) KindGloss() string { return kindGloss(m.Kind) }

// KindGloss is the reader's and the peek's kind chip tooltip.
func (d memoryDetail) KindGloss() string { return kindGloss(d.Kind) }

// KindGloss is a supersession neighbor's kind chip tooltip.
func (r memoryRef) KindGloss() string { return kindGloss(r.Kind) }
