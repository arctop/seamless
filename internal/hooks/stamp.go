package hooks

// Provenance stamps on what the hooks record for a conversation
// (isolation.requests on, plan fenced-sessions P6; core.Stamp). What a hook
// records is a write on the conversation's behalf, so it carries the stamps
// its tool calls' writes carry: for each fenced project in the conversation's
// label other than the destination, the record that cleared that project's
// content into it. A plan or subagent capture carries them in its note's
// frontmatter (the files layer mirrors them into item_provenance); the
// harvest's findings -- the SessionEnd harvest, Codex Stop and pushed
// findings -- and the plan's tracking task in item_provenance alone.
//
// Only the conversation's own text is stamped: a pointer, a withheld or
// not-recorded line, a token count or a status flip carries nothing it holds.
// The stamps are read where the record lands, after P0's routing and P5's
// home decided where that is and the clearance cleared it; a home the
// clearance refuses records nothing, so nothing is stamped there. A stamp that
// cannot be read keeps the write from landing unstamped: the capture is
// dropped, the findings stay as they were. With the switch off nothing here
// runs and every hook is P0's.

import (
	"context"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// conversationStamps returns the stamps a write into project carries for the
// conversation a hook names (store.StampsOfGrants over clearanceOf): nil with
// isolation.requests off and for a hook that names no session.
func (h *Handler) conversationStamps(ctx context.Context, client Client, externalSessionID, project string) ([]core.Stamp, error) {
	if !h.readGrants || h.db == nil || externalSessionID == "" {
		return nil, nil
	}
	g, err := h.clearanceOf(ctx, client, externalSessionID)
	if err != nil {
		return nil, err
	}
	return store.StampsOfGrants(g, project), nil
}

// stampNote adds to note, about to be written in its project, the stamps the
// conversation's write carries, keeping every stamp it has. false: they could
// not be read (recorded), and the caller drops the capture.
func (h *Handler) stampNote(ctx context.Context, client Client, externalSessionID string, note *core.Note) bool {
	stamps, err := h.conversationStamps(ctx, client, externalSessionID, note.Project)
	if err != nil {
		h.recordHookError(ctx, "capture-provenance", client, err, "note", note.ID)
		return false
	}
	note.MayContain, _ = core.MergeStamps(note.MayContain, stamps, note.Project)
	return true
}

// stampWorkItem records on an item of the work record the hook is about to
// write for the conversation -- the session whose findings take its harvest,
// a plan's tracking task -- the stamps that write carries into project.
// false: they could not be recorded (recorded as a hook error), and a caller
// that has not written yet leaves the write out.
func (h *Handler) stampWorkItem(ctx context.Context, client Client, externalSessionID, kind, id, project string) bool {
	stamps, err := h.conversationStamps(ctx, client, externalSessionID, project)
	if err == nil {
		err = store.StampWorkItem(ctx, h.db, kind, id, project, stamps, h.now())
	}
	if err != nil {
		h.recordHookError(ctx, "record-provenance", client, err, kind, id)
		return false
	}
	return true
}

// stampFindings is stampWorkItem for the session whose findings are about to
// take the conversation's harvest.
func (h *Handler) stampFindings(ctx context.Context, client Client, externalSessionID string, sess core.Session) bool {
	return h.stampWorkItem(ctx, client, externalSessionID, core.ItemKindSession, sess.ID, sess.ProjectSlug)
}
