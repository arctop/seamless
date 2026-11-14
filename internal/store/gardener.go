package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// Proposal statuses. A rejection has two strengths: ProposalDismissed is the
// regular one, suppressing the pattern only until new evidence for it arrives,
// while ProposalHidden is the forever block the owner asks for explicitly.
const (
	ProposalPending   = "pending"
	ProposalApplied   = "applied"
	ProposalDismissed = "dismissed"
	ProposalHidden    = "hidden"
)

// Proposal kinds (mirrors the gardener_proposals.kind CHECK constraint).
const (
	ProposalMerge        = "merge"
	ProposalArchive      = "archive"
	ProposalDigest       = "digest"
	ProposalConsolidate  = "consolidate"
	ProposalReproject    = "reproject"     // move one memory to another project
	ProposalSplit        = "split"         // set up child/shared projects + family for a project split
	ProposalAbandonPlan  = "abandon_plan"  // retag a never-approved captured plan plan-status:abandoned
	ProposalMemoryWanted = "memory_wanted" // agents repeatedly searched for knowledge that does not exist
	ProposalToolError    = "tool_error"    // agents keep hitting the same tool-call or hook-stage error
	ProposalRekind       = "rekind"        // change one memory's kind classification in place
	ProposalShipPlan     = "ship_plan"     // retag an implemented-but-unapproved captured plan plan-status:shipped
	// ProposalRelocate moves a GLOBAL memory back inside the project whose
	// sessions wrote it, after that project tightened its isolation. Its effect
	// is a reproject's, but the decision is not: the evidence is provenance (a
	// leak that predates the fence), not scope tidying, so the inbox groups and
	// explains it on its own terms. The gardener's copy-relocate pass files it
	// too (payload basis "cited_sources"), for a memory carrying provenance
	// stamps whose sources all lie in one other project: the same move, and
	// provenance evidence again -- the memory's stamps and the references its
	// body makes.
	ProposalRelocate = "relocate"
	// ProposalMergePlans folds a never-approved captured plan into an existing
	// composition that already carries the steps for the same work. It is the
	// third answer the stale-plan pass can give (beside abandon and ship) and
	// the only one that moves anything: the capture's notes are retagged onto
	// the surviving slug instead of being settled where they sit.
	ProposalMergePlans = "merge_plans"
)

// ProposalKinds lists every valid proposal kind. This is the canonical set:
// derive the MCP schema's enum from it rather than transcribing, so a new kind
// cannot reach the store while staying invisible at the boundary.
var ProposalKinds = []string{
	ProposalMerge, ProposalArchive, ProposalDigest, ProposalConsolidate,
	ProposalReproject, ProposalSplit, ProposalAbandonPlan, ProposalMemoryWanted,
	ProposalToolError, ProposalRekind, ProposalShipPlan, ProposalRelocate,
	ProposalMergePlans,
}

// Proposal is one gardener suggestion awaiting owner review. Payload carries the
// kind-specific detail; every payload includes a stable "key" string the
// gardener uses to avoid re-proposing the same thing on a later pass. Result is
// what applying it produced (the note it wrote, the task it opened, the memory
// it moved) -- nil until applied, and the handle undo uses to find the artifact
// again.
type Proposal struct {
	ID         string         `json:"id"`
	Kind       string         `json:"kind"`
	Payload    map[string]any `json:"payload"`
	Status     string         `json:"status"`
	CreatedAt  time.Time      `json:"createdAt"`
	ResolvedAt *time.Time     `json:"resolvedAt,omitempty"`
	Result     map[string]any `json:"result,omitempty"`
}

// proposalCols is the column list every proposal query selects, in the order
// scanProposals reads them.
const proposalCols = `id, kind, payload, status, created_at, resolved_at, result`

// CreateProposal inserts a pending gardener proposal and returns it. The caller
// supplies the kind and payload; the id and created_at are stamped here.
func CreateProposal(ctx context.Context, db *sql.DB, kind string, payload map[string]any) (Proposal, error) {
	id, err := core.NewID()
	if err != nil {
		return Proposal{}, fmt.Errorf("store.CreateProposal: %w", err)
	}
	raw, err := json.Marshal(payload)
	if err != nil {
		return Proposal{}, fmt.Errorf("store.CreateProposal: marshal payload: %w", err)
	}
	now := time.Now().UTC()
	_, err = db.ExecContext(ctx, `
		INSERT INTO gardener_proposals (id, kind, payload, status, created_at)
		VALUES (?, ?, ?, ?, ?)`,
		id, kind, string(raw), ProposalPending, core.FormatTime(now))
	if err != nil {
		return Proposal{}, fmt.Errorf("store.CreateProposal: %w", err)
	}
	return Proposal{ID: id, Kind: kind, Payload: payload, Status: ProposalPending, CreatedAt: now}, nil
}

// PendingProposals returns pending proposals, newest first. kind filters by
// proposal kind when non-empty.
func PendingProposals(ctx context.Context, db *sql.DB, kind string) ([]Proposal, error) {
	q := `SELECT ` + proposalCols + `
		FROM gardener_proposals WHERE status = ?`
	args := []any{ProposalPending}
	if kind != "" {
		q += ` AND kind = ?`
		args = append(args, kind)
	}
	q += ` ORDER BY created_at DESC, id DESC`
	rows, err := db.QueryContext(ctx, q, args...)
	if err != nil {
		return nil, fmt.Errorf("store.PendingProposals: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanProposals(rows)
}

// ProposalByID returns one proposal. found is false when absent.
func ProposalByID(ctx context.Context, db *sql.DB, id string) (Proposal, bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+proposalCols+`
		FROM gardener_proposals WHERE id = ? LIMIT 1`, id)
	if err != nil {
		return Proposal{}, false, fmt.Errorf("store.ProposalByID: %w", err)
	}
	defer func() { _ = rows.Close() }()
	ps, err := scanProposals(rows)
	if err != nil {
		return Proposal{}, false, err
	}
	if len(ps) == 0 {
		return Proposal{}, false, nil
	}
	return ps[0], true, nil
}

// ResolveProposal marks a proposal applied, dismissed or hidden and stamps
// resolved_at. It errors if the proposal is missing or already resolved (not
// pending).
func ResolveProposal(ctx context.Context, db *sql.DB, id, status string, at time.Time) error {
	if status != ProposalApplied && status != ProposalDismissed && status != ProposalHidden {
		return fmt.Errorf("store.ResolveProposal: invalid status %q", status)
	}
	res, err := db.ExecContext(ctx, `
		UPDATE gardener_proposals SET status = ?, resolved_at = ?
		WHERE id = ? AND status = ?`,
		status, core.FormatTime(at.UTC()), id, ProposalPending)
	if err != nil {
		return fmt.Errorf("store.ResolveProposal: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store.ResolveProposal: rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("store.ResolveProposal: no pending proposal with id %q", id)
	}
	return nil
}

// UpdateProposalPayload replaces a pending proposal's payload (e.g. retargeting a
// reproject to a different project before applying). It errors if the proposal is
// missing or not pending, so a resolved proposal is never silently rewritten.
func UpdateProposalPayload(ctx context.Context, db *sql.DB, id string, payload map[string]any) error {
	raw, err := json.Marshal(payload)
	if err != nil {
		return fmt.Errorf("store.UpdateProposalPayload: marshal: %w", err)
	}
	res, err := db.ExecContext(ctx, `
		UPDATE gardener_proposals SET payload = ?
		WHERE id = ? AND status = ?`, string(raw), id, ProposalPending)
	if err != nil {
		return fmt.Errorf("store.UpdateProposalPayload: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store.UpdateProposalPayload: rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("store.UpdateProposalPayload: no pending proposal with id %q", id)
	}
	return nil
}

// RecordProposalResult stores what applying a proposal produced. It is called
// after ResolveProposal, so it deliberately does not filter on status -- the
// proposal is already applied by then. A missing row is not an error: the
// result is a convenience for undo, never a correctness requirement of apply.
func RecordProposalResult(ctx context.Context, db *sql.DB, id string, result map[string]any) error {
	raw, err := json.Marshal(result)
	if err != nil {
		return fmt.Errorf("store.RecordProposalResult: marshal: %w", err)
	}
	if _, err := db.ExecContext(ctx, `
		UPDATE gardener_proposals SET result = ? WHERE id = ?`, string(raw), id); err != nil {
		return fmt.Errorf("store.RecordProposalResult: %w", err)
	}
	return nil
}

// ReopenProposal returns a resolved proposal to the pending queue, clearing the
// resolution stamp and the apply result. It is the store half of undo: the
// caller has already inverted the effect (or is undoing a rejection, which had
// none). It errors if the proposal is missing or still pending, so a double
// undo cannot resurrect an already-restored proposal.
//
// The payload key is deliberately left in place: ProposalKeyBlocks reads every
// status, so dedup behaves the same whether the proposal is pending, resolved,
// or reopened -- and a pending row blocks just as hard as an applied one.
func ReopenProposal(ctx context.Context, db *sql.DB, id string) error {
	res, err := db.ExecContext(ctx, `
		UPDATE gardener_proposals SET status = ?, resolved_at = NULL, result = NULL
		WHERE id = ? AND status IN (?, ?, ?)`,
		ProposalPending, id, ProposalApplied, ProposalDismissed, ProposalHidden)
	if err != nil {
		return fmt.Errorf("store.ReopenProposal: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store.ReopenProposal: rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("store.ReopenProposal: no resolved proposal with id %q", id)
	}
	return nil
}

// UnhideProposal demotes a forever-hidden proposal to a regular dismissal. It
// is NOT an undo: the proposal stays resolved and out of the queue, and nothing
// is re-raised on the spot. What changes is the suppression rule -- a hard
// block becomes one that lapses the moment evidence for the pattern recurs, so
// the next gardener pass MAY propose it again.
//
// resolved_at is deliberately left at the moment of the hide rather than
// restamped: it is when the owner decided, and it is the instant the recurrence
// comparison measures from, so a pattern that is still occurring re-raises on
// the next pass instead of waiting for evidence newer than the unhide.
func UnhideProposal(ctx context.Context, db *sql.DB, id string) error {
	res, err := db.ExecContext(ctx, `
		UPDATE gardener_proposals SET status = ? WHERE id = ? AND status = ?`,
		ProposalDismissed, id, ProposalHidden)
	if err != nil {
		return fmt.Errorf("store.UnhideProposal: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store.UnhideProposal: rows affected: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("store.UnhideProposal: no hidden proposal with id %q", id)
	}
	return nil
}

// HiddenProposals returns every forever-hidden proposal, newest decision first.
// It backs the console's hidden list, which is the only durable surface naming
// what the owner has blocked -- "Recently decided" scrolls a hide off within a
// handful of decisions, and a block nobody can see is a block nobody can lift.
func HiddenProposals(ctx context.Context, db *sql.DB) ([]Proposal, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+proposalCols+`
		FROM gardener_proposals WHERE status = ?
		ORDER BY resolved_at DESC, id DESC`, ProposalHidden)
	if err != nil {
		return nil, fmt.Errorf("store.HiddenProposals: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanProposals(rows)
}

// RecentResolvedProposals returns the most recently applied or dismissed
// proposals, newest resolution first, capped at limit. It backs the console's
// "Recently decided" section, where each row offers an undo.
func RecentResolvedProposals(ctx context.Context, db *sql.DB, limit int) ([]Proposal, error) {
	if limit <= 0 {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT `+proposalCols+`
		FROM gardener_proposals WHERE status <> ?
		ORDER BY resolved_at DESC, id DESC LIMIT ?`, ProposalPending, limit)
	if err != nil {
		return nil, fmt.Errorf("store.RecentResolvedProposals: %w", err)
	}
	defer func() { _ = rows.Close() }()
	return scanProposals(rows)
}

// ProposalBlock is why one payload key is suppressed. Hard is the permanent
// block: the key belongs to a proposal that is pending, was applied, or was
// hidden forever, and no amount of new evidence should raise it again. When
// Hard is false the key was only ever regularly dismissed, and DismissedAt is
// the newest such decision -- evidence that postdates it is a recurrence the
// owner has not yet answered, so the pattern may be proposed again.
type ProposalBlock struct {
	Hard        bool
	DismissedAt time.Time
}

// ProposalKeyBlocks returns the suppression rule for every payload "key" across
// proposals of EVERY status. The gardener consults it before proposing, so a
// suggestion the owner already saw is not raised again -- forever for an
// applied, pending or hidden one, and until the evidence recurs for a regular
// dismissal.
//
// A key can carry several rows (a dismissal, then the recurrence that re-raised
// it). The strongest rule wins: any hard row makes the key hard, otherwise the
// latest dismissal is the one to beat.
func ProposalKeyBlocks(ctx context.Context, db *sql.DB) (map[string]ProposalBlock, error) {
	rows, err := db.QueryContext(ctx, `SELECT payload, status, resolved_at FROM gardener_proposals`)
	if err != nil {
		return nil, fmt.Errorf("store.ProposalKeyBlocks: %w", err)
	}
	defer func() { _ = rows.Close() }()
	blocks := make(map[string]ProposalBlock)
	for rows.Next() {
		var (
			raw, status string
			resolved    sql.NullString
		)
		if err := rows.Scan(&raw, &status, &resolved); err != nil {
			return nil, fmt.Errorf("store.ProposalKeyBlocks: scan: %w", err)
		}
		var p struct {
			Key string `json:"key"`
		}
		if err := json.Unmarshal([]byte(raw), &p); err != nil || p.Key == "" {
			continue
		}
		prev := blocks[p.Key]
		if prev.Hard {
			continue
		}
		if status != ProposalDismissed {
			blocks[p.Key] = ProposalBlock{Hard: true}
			continue
		}
		at, err := nullTimePtr(resolved)
		if err != nil {
			return nil, fmt.Errorf("store.ProposalKeyBlocks: resolved_at: %w", err)
		}
		// A dismissal with no stamp cannot be compared against evidence, so it
		// keeps the zero time and any evidence at all counts as a recurrence.
		if at != nil && at.After(prev.DismissedAt) {
			prev.DismissedAt = *at
		}
		blocks[p.Key] = prev
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ProposalKeyBlocks: rows: %w", err)
	}
	return blocks, nil
}

func scanProposals(rows *sql.Rows) ([]Proposal, error) {
	var out []Proposal
	for rows.Next() {
		var (
			p             Proposal
			raw, created  string
			resolved, res sql.NullString
		)
		if err := rows.Scan(&p.ID, &p.Kind, &raw, &p.Status, &created, &resolved, &res); err != nil {
			return nil, fmt.Errorf("store: scan proposal: %w", err)
		}
		if err := json.Unmarshal([]byte(raw), &p.Payload); err != nil {
			return nil, fmt.Errorf("store: proposal payload: %w", err)
		}
		// A result written by an older binary (or hand-edited) must not make the
		// whole proposal unreadable: undo simply finds no handle and refuses.
		if res.Valid && res.String != "" {
			if err := json.Unmarshal([]byte(res.String), &p.Result); err != nil {
				p.Result = nil
			}
		}
		var err error
		if p.CreatedAt, err = core.ParseTime(created); err != nil {
			return nil, fmt.Errorf("store: proposal created_at: %w", err)
		}
		if p.ResolvedAt, err = nullTimePtr(resolved); err != nil {
			return nil, fmt.Errorf("store: proposal resolved_at: %w", err)
		}
		out = append(out, p)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.scanProposals: %w", err)
	}
	return out, nil
}

// ---------------------------------------------------------------------------
// Gardener support queries (dedup / staleness / digest inputs)
// ---------------------------------------------------------------------------

// MemoryVector pairs an active memory's index metadata with its stored embedding
// vector, for the gardener's pairwise dedup scan.
type MemoryVector struct {
	ID          string
	Project     string
	Name        string
	Description string
	Kind        string
	UpdatedAt   time.Time
	Vec         []float32
}

// ActiveMemoryVectors returns the vectors of all active memories embedded under
// model, oldest-created first (stable ordering for deterministic pairing). It
// joins the embeddings table, so a memory with no vector for that model is
// omitted (the gardener simply cannot dedup it semantically).
func ActiveMemoryVectors(ctx context.Context, db *sql.DB, model string) ([]MemoryVector, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT m.id, m.project, m.name, m.description, m.kind, m.updated_at, e.vec
		FROM memories_index m
		JOIN embeddings e ON e.item_id = m.id
		WHERE m.invalid_at IS NULL AND e.kind = 'memory' AND e.model = ?
		ORDER BY m.created_at ASC, m.id ASC`, model)
	if err != nil {
		return nil, fmt.Errorf("store.ActiveMemoryVectors: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []MemoryVector
	for rows.Next() {
		var (
			mv      MemoryVector
			updated string
			blob    []byte
		)
		if err := rows.Scan(&mv.ID, &mv.Project, &mv.Name, &mv.Description, &mv.Kind, &updated, &blob); err != nil {
			return nil, fmt.Errorf("store.ActiveMemoryVectors: scan: %w", err)
		}
		var err error
		if mv.UpdatedAt, err = core.ParseTime(updated); err != nil {
			return nil, fmt.Errorf("store.ActiveMemoryVectors: updated_at: %w", err)
		}
		mv.Vec = DecodeVector(blob)
		out = append(out, mv)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.ActiveMemoryVectors: rows: %w", err)
	}
	return out, nil
}

// AllActiveMemories returns every active memory across all projects (index rows,
// no body), newest-updated first. It backs the gardener's reference scan, which
// reads each file's body to find [[name]] links.
func AllActiveMemories(ctx context.Context, db *sql.DB) ([]core.Memory, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+memoryCols+`
		FROM memories_index WHERE invalid_at IS NULL
		ORDER BY updated_at DESC, id DESC`)
	if err != nil {
		return nil, fmt.Errorf("store.AllActiveMemories: %w", err)
	}
	defer func() { _ = rows.Close() }()
	mems, err := scanMemories(rows)
	if err != nil {
		return nil, fmt.Errorf("store.AllActiveMemories: %w", err)
	}
	return mems, nil
}

// CompletedSessionsSince returns completed sessions with non-empty findings
// updated on or after since, across all projects, newest first. It feeds the
// gardener's monthly digest pass.
func CompletedSessionsSince(ctx context.Context, db *sql.DB, since time.Time) ([]core.Session, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+sessionCols+`
		FROM sessions
		WHERE status = 'completed' AND findings <> '' AND updated_at >= ?
		ORDER BY updated_at DESC, id DESC`, core.FormatTime(since.UTC()))
	if err != nil {
		return nil, fmt.Errorf("store.CompletedSessionsSince: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []core.Session
	for rows.Next() {
		s, err := scanSession(rows)
		if err != nil {
			return nil, fmt.Errorf("store.CompletedSessionsSince: %w", err)
		}
		out = append(out, s)
	}
	return out, rows.Err()
}
