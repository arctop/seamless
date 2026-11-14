package store

// Provenance stamps (migration 039): an item written while its conversation
// holds content from a fenced project other than the item's own carries a
// stamp, "may contain content from <project> (<grant|move> <short id>)".
// item_provenance is the index behind the stamps and the console's came-from
// filters. Memories and notes also carry their stamp in frontmatter; tasks and
// session findings only here.
//
// A stamp is set from the writing conversation's state, never from agent
// input, and names projects and approvals, never content. It only ever warns:
// nothing here, and nothing that reads it, opens or confines anything. Edits
// keep a stamp, revocation and expiry never delete one, and stamping an item
// again keeps the approval it first named.

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// ErrInvalidProvenance is a provenance stamp this store refuses to record as
// given.
var ErrInvalidProvenance = errors.New("invalid provenance stamp")

// ProvenanceKinds are the item kinds a stamp may name (core.ItemKind*): the
// two knowledge kinds and the three kinds of the work record a conversation
// writes. A trial is the agent's own text too -- trial_record writes its
// title, changes, expected and actual results into a project the clearance
// rule cleared, and recall and trial_query serve them to that project's
// agents -- so it is stamped like a task (plan fenced-sessions P6).
var ProvenanceKinds = []string{core.ItemKindMemory, core.ItemKindNote, core.ItemKindTask, core.ItemKindTrial, core.ItemKindSession}

const (
	// provenanceDefaultLimit and provenanceMaxLimit bound a list.
	provenanceDefaultLimit = 100
	provenanceMaxLimit     = 1000
)

// ProvenanceStamp is one row of item_provenance: the item ItemID of kind
// ItemKind, in Project ("" for the global scope), may contain content from
// Source, cleared by the grant or move approval ApprovalID -- or, for a fenced
// home's native flow, by the conversation's own session there, whose id
// ApprovalID then holds (migration 039's column has no foreign key).
type ProvenanceStamp struct {
	ItemKind   string
	ItemID     string
	Project    string
	Source     string
	ApprovalID string
	CreatedAt  time.Time
	// Via is what ApprovalID names, read back on every list (core.StampVia*:
	// the approval's kind, or session for a session's id), "" when this
	// database holds neither -- an approval an archive merge carried no row
	// for. ProvenanceUpsert ignores it: it is derived, never stored.
	Via string
}

// Stamp is s as a core.Stamp: what an item shows.
func (s ProvenanceStamp) Stamp() core.Stamp {
	return core.Stamp{Project: s.Source, Via: s.Via, Ref: s.ApprovalID}
}

// provenanceExecutor is the write subset shared by *sql.DB and *sql.Tx, so a
// stamp lands inside the transaction that writes its item when there is one.
type provenanceExecutor interface {
	ExecContext(ctx context.Context, query string, args ...any) (sql.Result, error)
}

func provenanceValid(s ProvenanceStamp) error {
	switch {
	case !slices.Contains(ProvenanceKinds, s.ItemKind):
		return fmt.Errorf("%w: item kind %q: valid values are %s", ErrInvalidProvenance, s.ItemKind, strings.Join(ProvenanceKinds, ", "))
	case strings.TrimSpace(s.ItemID) == "" || s.ItemID != strings.TrimSpace(s.ItemID):
		return fmt.Errorf("%w: item id %q", ErrInvalidProvenance, s.ItemID)
	case s.Project != strings.TrimSpace(s.Project):
		return fmt.Errorf("%w: project %q", ErrInvalidProvenance, s.Project)
	case strings.TrimSpace(s.Source) == "" || s.Source != strings.TrimSpace(s.Source):
		return fmt.Errorf("%w: source %q", ErrInvalidProvenance, s.Source)
	case s.Source == s.Project:
		return fmt.Errorf("%w: an item never needs a stamp for its own project %q", ErrInvalidProvenance, s.Project)
	case strings.TrimSpace(s.ApprovalID) == "":
		return fmt.Errorf("%w: a stamp names the approval that cleared the write", ErrInvalidProvenance)
	}
	return nil
}

// ProvenanceUpsert records stamp s at now. An item already stamped for the same
// source keeps its first approval and its first time, and takes s's project:
// stamping again after the item was relocated keeps the index on its project.
func ProvenanceUpsert(ctx context.Context, ex provenanceExecutor, s ProvenanceStamp, now time.Time) error {
	if err := provenanceValid(s); err != nil {
		return fmt.Errorf("store.ProvenanceUpsert: %w", err)
	}
	if _, err := ex.ExecContext(ctx, `INSERT INTO item_provenance
		(item_kind, item_id, project_slug, source_project, approval_id, created_at) VALUES (?, ?, ?, ?, ?, ?)
		ON CONFLICT (item_kind, item_id, source_project) DO UPDATE SET project_slug = excluded.project_slug`,
		s.ItemKind, s.ItemID, s.Project, s.Source, s.ApprovalID, core.FormatTime(now)); err != nil {
		return fmt.Errorf("store.ProvenanceUpsert: %w", err)
	}
	return nil
}

// provenanceCols reads a stamp with what its approval_id names: the
// approval's kind when the approval is here, session when the id is a
// session's, else "".
const provenanceCols = `p.item_kind, p.item_id, p.project_slug, p.source_project, p.approval_id, p.created_at,
	CASE WHEN a.kind = '` + AccessKindGrant + `' THEN '` + core.StampViaGrant + `'
	     WHEN a.kind = '` + AccessKindMove + `' THEN '` + core.StampViaMove + `'
	     WHEN a.id IS NULL AND s.id IS NOT NULL THEN '` + core.StampViaSession + `'
	     ELSE '' END`

// provenanceFrom is the FROM clause provenanceCols reads.
const provenanceFrom = ` FROM item_provenance p
	LEFT JOIN access_approvals a ON a.id = p.approval_id
	LEFT JOIN sessions s ON s.id = p.approval_id`

func provenanceScan(rows *sql.Rows) ([]ProvenanceStamp, error) {
	var out []ProvenanceStamp
	for rows.Next() {
		var (
			s       ProvenanceStamp
			created string
		)
		if err := rows.Scan(&s.ItemKind, &s.ItemID, &s.Project, &s.Source, &s.ApprovalID, &created, &s.Via); err != nil {
			return nil, err
		}
		t, err := core.ParseTime(created)
		if err != nil {
			return nil, fmt.Errorf("stamp %s/%s created_at: %w", s.ItemKind, s.ItemID, err)
		}
		s.CreatedAt = t
		out = append(out, s)
	}
	return out, rows.Err()
}

// provenanceQuery runs one list over item_provenance.
func provenanceQuery(ctx context.Context, db *sql.DB, op, where string, args ...any) ([]ProvenanceStamp, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+provenanceCols+provenanceFrom+` WHERE `+where, args...)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	defer func() { _ = rows.Close() }()
	out, err := provenanceScan(rows)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	return out, nil
}

// provenanceLimit is limit, or the default when it is not positive, capped.
func provenanceLimit(limit int) int {
	if limit <= 0 {
		return provenanceDefaultLimit
	}
	return min(limit, provenanceMaxLimit)
}

// ProvenanceForItem returns an item's stamps, sorted by source: what its reads,
// recall hits and console page show.
func ProvenanceForItem(ctx context.Context, db *sql.DB, kind, id string) ([]ProvenanceStamp, error) {
	return provenanceQuery(ctx, db, "store.ProvenanceForItem",
		`p.item_kind = ? AND p.item_id = ? ORDER BY p.source_project`, kind, id)
}

// ProvenanceForProject returns the stamps of the items in project, newest
// first, at most limit (100 when it is not positive, 1000 at most): with source
// set, only the items that may contain content from it -- a project page's
// "came from" filter.
func ProvenanceForProject(ctx context.Context, db *sql.DB, project, source string, limit int) ([]ProvenanceStamp, error) {
	where, args := `p.project_slug = ?`, []any{project}
	if source != "" {
		where += ` AND p.source_project = ?`
		args = append(args, source)
	}
	args = append(args, provenanceLimit(limit))
	return provenanceQuery(ctx, db, "store.ProvenanceForProject",
		where+` ORDER BY p.created_at DESC, p.item_kind, p.item_id LIMIT ?`, args...)
}

// ProvenanceFromSource returns the stamps naming source, in every project,
// newest first, at most limit (as ProvenanceForProject): where a fenced
// project's content may have gone.
func ProvenanceFromSource(ctx context.Context, db *sql.DB, source string, limit int) ([]ProvenanceStamp, error) {
	return provenanceQuery(ctx, db, "store.ProvenanceFromSource",
		`p.source_project = ? ORDER BY p.created_at DESC, p.item_kind, p.item_id LIMIT ?`, source, provenanceLimit(limit))
}
