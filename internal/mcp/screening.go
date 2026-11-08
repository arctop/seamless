package mcp

import (
	"context"
	"errors"

	"github.com/mark3labs/mcp-go/mcp"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/features"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/screening"
	"github.com/arctop/seamless/internal/store"
)

// Write screening at the MCP boundary (the screening optional feature).
//
// Every write tool that lands content in the corpus asks screeningOn first;
// off -- the default, or no screening service wired -- the tool runs exactly
// as it always has. On, the tool builds the item it would write as usual, and
// right before the files layer is called the write is judged: the screens
// (screening.Judge), the session's outside-text mark, and the constraint
// rules. A held write never reaches the files layer: it goes to the owner's
// queue instead (screening.Service.Hold), which keeps it out of every
// briefing, recall, injection and gardener pass by construction, because
// those only ever read the memory/ and notes/ trees.

// screeningOn reports whether writes on this request are screened.
func (s *Server) screeningOn(ctx context.Context) bool {
	return s.cfg.Screening != nil && features.Enabled(s.effectiveFeatures(ctx), features.Screening)
}

// sessionTainted reports whether the session a write runs in carries the
// outside-text mark. A failed lookup counts as marked: the write is then held,
// which loses nothing (the owner can release it), where guessing "clean" could
// land exactly the write the mark exists to stop.
func (s *Server) sessionTainted(ctx context.Context, sessionID string) bool {
	tainted, err := s.cfg.Screening.Tainted(ctx, sessionID)
	if err != nil {
		s.logger.Warn("mcp: session taint lookup failed; treating the session as marked", "session_id", sessionID, "error", err)
		return true
	}
	return tainted
}

// heldError carries a hold out of a files-layer mutation callback: returning
// it aborts the mutation before anything is written.
type heldError struct{ hold store.HeldWrite }

func (e *heldError) Error() string { return "held for the owner's review" }

// asHeld unwraps a heldError.
func asHeld(err error) (*heldError, bool) {
	var h *heldError
	if errors.As(err, &h) {
		return h, true
	}
	return nil, false
}

// judged is one write's screening outcome as the tool records it: the verdict
// and the size of the text the screens read.
type judged struct {
	verdict screening.Verdict
	size    int
}

// heldMessage is what a held write tells the agent. It says plainly that
// nothing landed and that the hold is the owner's to resolve, so an agent
// neither reports a write that did not happen nor rephrases its way past it.
const heldMessage = "Held for the owner's review: this write is stored but kept out of briefings, recall and the " +
	"gardener until the owner releases it on the console's Held screen. Nothing in the corpus changed. Do not " +
	"retry or rephrase it to get past the hold; tell the user it is waiting for their review."

// heldResult is the tool answer for a held write: a success result (the
// write was accepted into the queue), never an error an agent would retry.
func heldResult(h store.HeldWrite, fields map[string]any) (*mcp.CallToolResult, error) {
	out := map[string]any{
		"status":  "held",
		"hold":    map[string]any{"id": h.ID, "reasons": h.Reasons},
		"message": heldMessage,
	}
	for k, v := range fields {
		out[k] = v
	}
	return jsonResult(out)
}

// recordScreened records one screened write's memory.screened event.
func (s *Server) recordScreened(ctx context.Context, tool, itemKind, itemID, project, sessionID string,
	kind core.MemoryKind, jd judged) {
	s.cfg.Screening.RecordScreened(ctx, screening.Screened{
		ItemKind: itemKind, ItemID: itemID, Project: project, SessionID: sessionID, Kind: kind,
		Tool: tool, Size: jd.size, Verdicts: jd.verdict.Introduced, Held: jd.verdict.Held(),
	})
}

// judgeMemoryWrite judges a memory write about to land (mem) against the file
// it replaces (replaced; zero for a create) and, when the call also
// supersedes, the memory it retires. Two things can hold it: write screening
// (when on), and -- whether or not screening is on -- a restricted account
// touching a briefing-injected kind, which makes the write a proposal (see
// proposals.go). Both fold into ONE hold. A held write is stored in the owner's
// queue and returned as a heldError; nil means write it.
func (s *Server) judgeMemoryWrite(ctx context.Context, j *memoryJudge, mem, replaced, supersedes core.Memory) error {
	retires := supersedes.ID != "" && supersedes.ID != mem.ID
	var retiredKind core.MemoryKind
	if retires {
		retiredKind = supersedes.Kind
	}
	propose := j.proposes(mem.Kind, replaced.Kind, retiredKind)
	if !j.screen && !propose {
		return nil
	}
	if propose && s.cfg.Screening == nil {
		return errNoProposalQueue
	}
	detail := map[string]string{"path": files.MemoryRelPath(mem.Project, mem.Name)}
	if retires {
		detail["supersedes_id"] = supersedes.ID
		detail["supersedes_name"] = supersedes.Name
		detail["supersedes_project"] = supersedes.Project
	}
	var verdict screening.Verdict
	if j.screen {
		extra := screening.MemoryReasons(j.tainted, mem.Kind, replaced.Kind)
		lockdown := s.cfg.Screening.LockdownOn(ctx)
		if lockdown && (screening.IsBriefingKind(mem.Kind) || screening.IsBriefingKind(replaced.Kind)) {
			extra = append(extra, screening.ReasonLockdown)
		}
		if retires {
			if supersedes.Kind == core.KindConstraint {
				extra = append(extra, screening.ReasonConstraintSupersede)
			}
			if j.tainted && screening.IsBriefingKind(supersedes.Kind) {
				extra = append(extra, screening.ReasonTaintedSession)
			}
			if lockdown && screening.IsBriefingKind(supersedes.Kind) {
				extra = append(extra, screening.ReasonLockdown)
			}
		}
		text := screening.MemoryText(mem)
		base := ""
		if replaced.ID != "" && !s.drifted(ctx, replaced.FilePath) {
			// A file in drift quarantine changed outside Seamless and was never
			// screened, so it is no baseline: judged against it, an edit would carry
			// the drifted text into the corpus as if it were already there.
			base = screening.MemoryText(replaced)
		}
		verdict = screening.Judge(text, base, s.cfg.Screening.Rules(), extra...)
		j.jd.size = len(text)
	}
	if propose {
		verdict.Reasons = proposalReasons(verdict.Reasons)
		detail[screening.DetailAccount] = j.account
	}
	j.jd.verdict = verdict
	if !verdict.Held() {
		return nil
	}
	// Rendered from the struct about to land, so a proposal's content carries
	// the account label and keeps it when the owner promotes it.
	content, err := files.RenderMemory(mem)
	if err != nil {
		return err
	}
	h, err := s.cfg.Screening.Hold(ctx, store.HeldWrite{
		Op: store.HeldOpWrite, Tool: j.tool, ItemKind: core.ItemKindMemory, ItemID: mem.ID,
		Project: mem.Project, Name: mem.Name, MemoryKind: mem.Kind, SessionID: j.session,
		Reasons: screening.Strings(verdict.Reasons), BaseHash: replaced.ContentHash,
		Content: content, Detail: detail,
	})
	if err != nil {
		return err
	}
	return &heldError{hold: h}
}

// judgeNoteWrite is judgeMemoryWrite for a note: the screens only (a note is
// never a briefing-injected kind). fromPath is the note's current path when
// the write moves it to another project, else "".
func (s *Server) judgeNoteWrite(ctx context.Context, tool string, note, replaced core.Note, fromPath,
	sessionID string, jd *judged) error {
	text := screening.NoteText(note)
	base := ""
	if replaced.ID != "" && !s.drifted(ctx, replaced.FilePath) {
		base = screening.NoteText(replaced)
	}
	jd.verdict = screening.Judge(text, base, s.cfg.Screening.Rules())
	jd.size = len(text)
	if !jd.verdict.Held() {
		return nil
	}
	content, err := files.RenderNote(note)
	if err != nil {
		return err
	}
	path := files.NoteRelPath(note.Project, note.Slug)
	detail := map[string]string{"path": path}
	if fromPath != "" && fromPath != path {
		detail["from_path"] = fromPath
	}
	h, err := s.cfg.Screening.Hold(ctx, store.HeldWrite{
		Op: store.HeldOpWrite, Tool: tool, ItemKind: core.ItemKindNote, ItemID: note.ID,
		Project: note.Project, Name: note.Slug, SessionID: sessionID,
		Reasons: screening.Strings(jd.verdict.Reasons), BaseHash: replaced.ContentHash,
		Content: content, Detail: detail,
	})
	if err != nil {
		return err
	}
	return &heldError{hold: h}
}

// judgeGardenerApply judges an agent's gardener_apply of proposal p: whether
// applying it retires a constraint (archive, merge, consolidate, rekind away
// from constraint), touches a briefing-injected kind from a session marked as
// having read outside text, or lands text that fails a screen (a digest, a
// consolidated body). A held apply leaves the proposal pending; releasing the
// hold applies it. screened reports whether the apply lands text the screens
// read, which is when a memory.screened event is due.
func (s *Server) judgeGardenerApply(ctx context.Context, p store.Proposal, sessionID string, tainted bool) (
	hold *store.HeldWrite, jd judged, screened bool, err error) {
	var reasons []screening.Code
	var target core.Memory
	itemKind := core.ItemKindMemory
	project, name := payloadText(p.Payload, "project"), payloadText(p.Payload, "name")
	var writesKind core.MemoryKind
	text := ""
	lookup := func(id string) (core.Memory, bool, error) {
		if id == "" {
			return core.Memory{}, false, nil
		}
		m, ok, lerr := store.MemoryByID(ctx, s.cfg.DB, id)
		if lerr != nil || !ok || !m.Active() {
			return core.Memory{}, false, lerr
		}
		return m, true, nil
	}
	lockdown := s.cfg.Screening.LockdownOn(ctx)
	retire := func(m core.Memory, reason screening.Code) {
		if m.Kind == core.KindConstraint {
			reasons = append(reasons, reason)
		}
		if tainted && screening.IsBriefingKind(m.Kind) {
			reasons = append(reasons, screening.ReasonTaintedSession)
		}
		if lockdown && screening.IsBriefingKind(m.Kind) {
			reasons = append(reasons, screening.ReasonLockdown)
		}
	}
	switch p.Kind {
	case store.ProposalArchive:
		m, ok, lerr := lookup(payloadText(p.Payload, "id"))
		if lerr != nil {
			return nil, judged{}, false, lerr
		}
		if ok {
			target = m
			retire(m, screening.ReasonConstraintDelete)
		}
	case store.ProposalMerge:
		drop, ok, lerr := lookup(payloadText(payloadObject(p.Payload, "drop"), "id"))
		if lerr != nil {
			return nil, judged{}, false, lerr
		}
		if ok {
			target = drop
			retire(drop, screening.ReasonConstraintSupersede)
		}
	case store.ProposalConsolidate:
		for _, src := range payloadObjects(p.Payload, "sources") {
			m, ok, lerr := lookup(payloadText(src, "id"))
			if lerr != nil {
				return nil, judged{}, false, lerr
			}
			if ok {
				retire(m, screening.ReasonConstraintSupersede)
			}
		}
		writesKind = core.MemoryKind(payloadText(p.Payload, "kind"))
		if tainted && screening.IsBriefingKind(writesKind) {
			reasons = append(reasons, screening.ReasonTaintedSession)
		}
		if lockdown && screening.IsBriefingKind(writesKind) {
			reasons = append(reasons, screening.ReasonLockdown)
		}
		text = payloadText(p.Payload, "description") + "\n" + payloadText(p.Payload, "body")
	case store.ProposalRekind:
		m, ok, lerr := lookup(payloadText(p.Payload, "id"))
		if lerr != nil {
			return nil, judged{}, false, lerr
		}
		if ok {
			target = m
			to := core.MemoryKind(payloadText(p.Payload, "to"))
			reasons = append(reasons, screening.MemoryReasons(tainted, to, m.Kind)...)
			if lockdown && (screening.IsBriefingKind(to) || screening.IsBriefingKind(m.Kind)) {
				reasons = append(reasons, screening.ReasonLockdown)
			}
		}
	case store.ProposalDigest:
		itemKind = core.ItemKindNote
		name = core.Slugify(payloadText(p.Payload, "title"))
		text = payloadText(p.Payload, "title") + "\n" + payloadText(p.Payload, "body")
	}
	if target.ID != "" {
		project, name, writesKind = target.Project, target.Name, target.Kind
	}
	screened = text != ""
	if screened {
		jd.verdict = screening.Judge(text, "", s.cfg.Screening.Rules(), reasons...)
		jd.size = len(text)
	} else {
		jd.verdict = screening.Judge("", "", s.cfg.Screening.Rules(), reasons...)
	}
	if !jd.verdict.Held() {
		return nil, jd, screened, nil
	}
	h, err := s.cfg.Screening.Hold(ctx, store.HeldWrite{
		Op: store.HeldOpGardener, Tool: "gardener_apply", ItemKind: itemKind, ItemID: target.ID,
		Project: project, Name: name, MemoryKind: writesKind, SessionID: sessionID,
		Reasons: screening.Strings(jd.verdict.Reasons),
		Detail:  map[string]string{"proposal": p.ID, "proposal_kind": p.Kind},
	})
	if err != nil {
		return nil, judged{}, false, err
	}
	return &h, jd, screened, nil
}

// drifted reports whether the file at relPath is in drift quarantine. A
// failed lookup counts as drifted: the write is then screened against no
// baseline, which can only find more.
func (s *Server) drifted(ctx context.Context, relPath string) bool {
	if relPath == "" {
		return false
	}
	rec, ok, err := store.CorpusRecordByPath(ctx, s.cfg.DB, relPath)
	if err != nil {
		s.logger.Warn("mcp: corpus record lookup failed; screening the write against no baseline", "file_path", relPath, "error", err)
		return true
	}
	return ok && rec.Drifted()
}

// payloadText, payloadObject and payloadObjects read a gardener proposal's
// untyped JSON payload ("" / nil when absent or of another type).
func payloadText(m map[string]any, key string) string {
	v, _ := m[key].(string)
	return v
}

func payloadObject(m map[string]any, key string) map[string]any {
	v, _ := m[key].(map[string]any)
	return v
}

func payloadObjects(m map[string]any, key string) []map[string]any {
	raw, _ := m[key].([]any)
	out := make([]map[string]any, 0, len(raw))
	for _, v := range raw {
		if obj, ok := v.(map[string]any); ok {
			out = append(out, obj)
		}
	}
	return out
}
