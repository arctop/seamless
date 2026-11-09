package retrieve

import (
	"context"
	"fmt"
	"slices"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/features"
	"github.com/arctop/seamless/internal/store"
)

// Provenance on injected memories (the screening optional feature): every
// memory line a briefing or a prompt-recall block carries says whether the
// owner confirmed its current content or an agent wrote it, and from which
// session. Off, nothing here runs and every line renders exactly as before.
//
// "Owner-confirmed" has one meaning: the owner released a held write of this
// content on the console's Held screen, and the memory still says exactly
// what it said then (store.OwnerConfirmations holds the core.MemoryDigest of
// the confirmed content; any later change -- an agent's edit, a hand edit of
// the file -- no longer matches, so the mark lapses on its own). Everything
// else is "agent-written": the memory reached the corpus through an agent's
// write, the gardener, or a path that recorded no owner decision.

// Provenance labels, as agents read them.
const (
	ProvenanceOwnerConfirmed = "owner-confirmed"
	ProvenanceAgentWritten   = "agent-written"
)

// Provenance is who stands behind a memory's current content.
type Provenance struct {
	// OwnerConfirmed: the owner confirmed exactly this content.
	OwnerConfirmed bool
	// Session is the display name of the session that last wrote the content
	// through Seamless: "" when none is on record, or when that session ran
	// outside the memory's own scope (a global memory written from inside a
	// project), whose name the reader has no business learning.
	Session string
}

// Label is the provenance word: owner-confirmed or agent-written.
func (p Provenance) Label() string {
	if p.OwnerConfirmed {
		return ProvenanceOwnerConfirmed
	}
	return ProvenanceAgentWritten
}

// mark is the bracketed suffix a briefing or recall line carries.
func (p Provenance) mark() string {
	if p.OwnerConfirmed {
		return " [" + ProvenanceOwnerConfirmed + "]"
	}
	if p.Session != "" {
		return " [" + ProvenanceAgentWritten + ", session " + sanitizeField(p.Session, 40) + "]"
	}
	return " [" + ProvenanceAgentWritten + "]"
}

// provenanceOn reports whether injected memories carry provenance marks.
func (s *Service) provenanceOn(ctx context.Context) bool {
	return features.Enabled(s.effectiveFeatures(ctx), features.Screening)
}

// MemoryProvenance resolves the provenance of each memory. mems may be index
// rows (no body) or file-read memories; a confirmation is verified against
// the memory's current content, read through the body reader when the row
// carries none. It is failure-soft: a store or file error degrades that
// memory to agent-written (the conservative answer) and is logged.
func (s *Service) MemoryProvenance(ctx context.Context, mems []core.Memory) map[string]Provenance {
	out := make(map[string]Provenance, len(mems))
	if len(mems) == 0 {
		return out
	}
	ids := make([]string, 0, len(mems))
	for _, m := range mems {
		ids = append(ids, m.ID)
	}
	confirmed, err := store.OwnerConfirmations(ctx, s.db, ids)
	if err != nil {
		s.logger.Warn("retrieve: owner confirmations unavailable; marking memories agent-written", "error", err)
		confirmed = nil
	}
	writers, err := store.ContentWriters(ctx, s.db, ids)
	if err != nil {
		s.logger.Warn("retrieve: content writers unavailable; provenance names no session", "error", err)
		writers = nil
	}
	sessionIDs := make([]string, 0, len(mems))
	writerOf := make(map[string]string, len(mems))
	for _, m := range mems {
		w, ok := writers[m.ID]
		if !ok {
			w = m.SourceSession // no write on record: the creator, if known
		}
		writerOf[m.ID] = w
		if w != "" && !slices.Contains(sessionIDs, w) {
			sessionIDs = append(sessionIDs, w)
		}
	}
	refs, err := store.SessionRefs(ctx, s.db, sessionIDs)
	if err != nil {
		s.logger.Warn("retrieve: session names unavailable; provenance names no session", "error", err)
		refs = nil
	}
	for _, m := range mems {
		var p Provenance
		// A session is named only when it ran in the memory's own scope: anyone
		// reading the memory reaches that scope already. A global memory written
		// from inside a project would otherwise carry that project's session
		// name into every other project's briefing -- across an isolation fence
		// or a lock as easily as anywhere.
		if ref, ok := refs[writerOf[m.ID]]; ok && ref.Project == m.Project {
			p.Session = ref.Name
		}
		if digest, ok := confirmed[m.ID]; ok {
			p.OwnerConfirmed = s.stillConfirmed(m, digest)
		}
		out[m.ID] = p
	}
	return out
}

// stillConfirmed reports whether m still digests as the owner confirmed it.
func (s *Service) stillConfirmed(m core.Memory, digest string) bool {
	full := m
	if full.Body == "" {
		if s.bodyReader == nil || m.FilePath == "" {
			return false // cannot verify, so not confirmed
		}
		read, err := s.bodyReader.ReadMemory(m.FilePath)
		if err != nil {
			s.logger.Warn("retrieve: confirmed memory unreadable; marking it agent-written", "id", m.ID, "error", err)
			return false
		}
		full = read
	}
	return core.MemoryDigest(full) == digest
}

// provenanceMarks maps each memory to its line suffix. Callers ask
// provenanceOn first and pass a nil map while it is off -- the nil map is what
// keeps every line byte-identical to the feature-off rendering.
func (s *Service) provenanceMarks(ctx context.Context, groups ...[]core.Memory) map[string]string {
	var all []core.Memory
	seen := map[string]bool{}
	for _, g := range groups {
		for _, m := range g {
			if !seen[m.ID] {
				seen[m.ID] = true
				all = append(all, m)
			}
		}
	}
	marks := make(map[string]string, len(all))
	for id, p := range s.MemoryProvenance(ctx, all) {
		marks[id] = p.mark()
	}
	return marks
}

// hitMemories loads the index rows behind prompt-matched hits, for their
// provenance. Only asked while provenance is on; a row that vanished since the
// match (or a failed read) simply carries no mark.
func (s *Service) hitMemories(ctx context.Context, hits []promptHit) []core.Memory {
	if len(hits) == 0 {
		return nil
	}
	out := make([]core.Memory, 0, len(hits))
	for _, h := range hits {
		m, ok, err := store.MemoryByID(ctx, s.db, h.id)
		if err != nil {
			s.logger.Warn("retrieve: provenance row unavailable", "id", h.id, "error", err)
			continue
		}
		if ok {
			out = append(out, m)
		}
	}
	return out
}

// ---------------------------------------------------------------------------
// Withholding (write screening, part 2)
// ---------------------------------------------------------------------------

// While the screening feature is on, two things keep a memory out of the
// briefing and the prompt-matched recall block -- the surfaces that INJECT
// knowledge into a session without the agent asking:
//
//   - drift: its file changed (or appeared) outside Seamless, so its text
//     never passed the write screens. It is withheld until the owner accepts
//     it on the console, lockdown or not.
//   - lockdown: only trusted memories are injected. Trusted means the owner
//     confirmed this exact content (a release, a mark-safe, an accepted
//     drift: owner_confirmations), or the review marked it safe and it still
//     digests as the review read it. An agent's later change lapses both, so
//     an agent-written memory created or changed since the review waits for
//     the owner or the next review.
//
// Off, nothing here runs. Recall and memory_read, which an agent asks for by
// name or query, are not withheld.

// lockdownOn reports whether lockdown is on.
func (s *Service) lockdownOn(ctx context.Context) bool {
	return s.lockdown != nil && s.lockdown(ctx)
}

// withholding is one assembly's answer to "may this memory be injected": the
// drifted set and, under lockdown, the trusted set. Built once per briefing
// or prompt-recall block.
type withholding struct {
	on       bool
	lockdown bool
	drifted  map[string]bool
	trusted  map[string]bool
	count    int
}

// newWithholding resolves the drifted set (and the lockdown state) for one
// assembly. Off, it withholds nothing.
func (s *Service) newWithholding(ctx context.Context) *withholding {
	w := &withholding{}
	if !s.provenanceOn(ctx) {
		return w
	}
	w.on = true
	w.lockdown = s.lockdownOn(ctx)
	drifted, err := store.DriftedItemIDs(ctx, s.db)
	if err != nil {
		s.logger.Warn("retrieve: drift quarantine unreadable; nothing is withheld for drift", "error", err)
	}
	w.drifted = drifted
	return w
}

// filter drops the memories this assembly may not inject and counts them.
func (s *Service) filter(ctx context.Context, w *withholding, mems []core.Memory) []core.Memory {
	if !w.on || len(mems) == 0 {
		return mems
	}
	if w.lockdown {
		s.addTrusted(ctx, w, mems)
	}
	kept := make([]core.Memory, 0, len(mems))
	for _, m := range mems {
		if w.withheld(m.ID) {
			w.count++
			continue
		}
		kept = append(kept, m)
	}
	return kept
}

// filterHits is filter for prompt-matched hits (loaded by id for the trust
// check).
func (s *Service) filterHits(ctx context.Context, w *withholding, hits []promptHit) []promptHit {
	if !w.on || len(hits) == 0 {
		return hits
	}
	if w.lockdown {
		s.addTrusted(ctx, w, s.hitMemories(ctx, hits))
	}
	kept := make([]promptHit, 0, len(hits))
	for _, h := range hits {
		if w.withheld(h.id) {
			w.count++
			continue
		}
		kept = append(kept, h)
	}
	return kept
}

func (w *withholding) withheld(id string) bool {
	if w.drifted[id] {
		return true
	}
	return w.lockdown && !w.trusted[id]
}

// addTrusted adds to w.trusted the memories of mems whose current content the
// owner confirmed or the review marked safe. Failure-soft toward withholding:
// a store or file error leaves a memory untrusted.
func (s *Service) addTrusted(ctx context.Context, w *withholding, mems []core.Memory) {
	if w.trusted == nil {
		w.trusted = make(map[string]bool, len(mems))
	}
	ids := make([]string, 0, len(mems))
	for _, m := range mems {
		ids = append(ids, m.ID)
	}
	confirmed, err := store.OwnerConfirmations(ctx, s.db, ids)
	if err != nil {
		s.logger.Warn("retrieve: owner confirmations unavailable; lockdown withholds unconfirmed memories", "error", err)
	}
	records, err := store.CorpusRecordsByItem(ctx, s.db, ids)
	if err != nil {
		s.logger.Warn("retrieve: corpus record unavailable; lockdown withholds unreviewed memories", "error", err)
	}
	for _, m := range mems {
		conf, hasConf := confirmed[m.ID]
		rec, hasRec := records[m.ID]
		reviewed := hasRec && rec.ReviewStatus == store.ReviewSafe && rec.ReviewDigest != ""
		if !hasConf && !reviewed {
			continue
		}
		full, ok := s.fullMemory(m)
		if !ok {
			continue
		}
		digest := core.MemoryDigest(full)
		if (hasConf && conf == digest) || (reviewed && rec.ReviewDigest == digest) {
			w.trusted[m.ID] = true
		}
	}
}

// fullMemory returns m with its body, read through the body reader when the
// index row carries none.
func (s *Service) fullMemory(m core.Memory) (core.Memory, bool) {
	if m.Body != "" {
		return m, true
	}
	if s.bodyReader == nil || m.FilePath == "" {
		return core.Memory{}, false
	}
	read, err := s.bodyReader.ReadMemory(m.FilePath)
	if err != nil {
		s.logger.Warn("retrieve: memory unreadable for the trust check; withholding it under lockdown", "id", m.ID, "error", err)
		return core.Memory{}, false
	}
	return read, true
}

// line is the pinned briefing line that says what was withheld: under
// lockdown always (an agent must know its binding-rule writes will wait),
// otherwise only when a drifted memory was left out.
func (w *withholding) line() string {
	switch {
	case !w.on:
		return ""
	case w.lockdown:
		return fmt.Sprintf("Lockdown: only memories the owner confirmed or reviewed are shown (%s withheld); "+
			"changes to constraints, conventions, protocols and stages wait for the owner.\n",
			countNoun(w.count, "memory", "memories"))
	case w.count > 0:
		return fmt.Sprintf("Withheld: %s changed outside Seamless, waiting for the owner's review.\n",
			countNoun(w.count, "memory", "memories"))
	}
	return ""
}
