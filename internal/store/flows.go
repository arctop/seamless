package store

// Carries and flows (migration 039). A grant or move request states its carry
// -- the conversation's home, every fenced project it holds once approved, and
// every project it may write to once approved -- and approving it clears each
// flow the carry lists: content of a held project may be written into a write
// destination. access_flows records the flows approvals listed; a grant row
// implies its own (store.HolderFlows returns both). Revocation, expiry and a
// restore never delete a flow: a conversation keeps what it read, so it keeps
// where the owner let that go.

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"slices"
	"strings"
	"time"
	"unicode/utf8"

	"github.com/arctop/seamless/internal/core"
)

const (
	// carryMaxProjects bounds each list of a carry: a conversation holding
	// more fenced projects than this is not one an owner can review.
	carryMaxProjects = 64
	// carryMaxSlugBytes bounds one slug in a carry.
	carryMaxSlugBytes = 255
	// flowMaxRows bounds the flows one holder's lookup returns.
	flowMaxRows = 4096
)

// Carry is what a grant or move request states and its approval binds. The
// zero Carry states nothing (a lock's request, and every request from before
// migration 039). Build one with CarryOf, which normalizes it, so equal carries
// encode equally.
type Carry struct {
	// Home is the conversation's home when it asked ("" for the global scope).
	Home string `json:"home"`
	// Holds is every fenced project the conversation holds once approved: its
	// label, plus what the approval adds. Sorted, distinct, never "".
	Holds []string `json:"holds"`
	// WriteTo is every project it may write to once approved. Sorted,
	// distinct, never "": the global scope is never cleared for fenced content.
	WriteTo []string `json:"write_to"`
}

// CarryOf builds a normalized carry: the lists sorted and deduplicated, every
// slug checked.
func CarryOf(home string, holds, writeTo []string) (Carry, error) {
	if home != "" {
		if err := carrySlug("home", home); err != nil {
			return Carry{}, err
		}
	}
	c := Carry{Home: home}
	for _, l := range []struct {
		name string
		in   []string
		out  *[]string
	}{{"holds", holds, &c.Holds}, {"write_to", writeTo, &c.WriteTo}} {
		if len(l.in) > carryMaxProjects {
			return Carry{}, fmt.Errorf("%w: carry %s names %d projects, at most %d", ErrInvalidAccessRequest, l.name, len(l.in), carryMaxProjects)
		}
		for _, slug := range l.in {
			if err := carrySlug(l.name, slug); err != nil {
				return Carry{}, err
			}
		}
		// Never nil: an empty list encodes as [] whichever way it was built.
		*l.out = append([]string{}, slices.Compact(slices.Sorted(slices.Values(l.in)))...)
	}
	return c, nil
}

// carrySlug checks one slug of a carry.
func carrySlug(what, slug string) error {
	if strings.TrimSpace(slug) == "" || slug != strings.TrimSpace(slug) || len(slug) > carryMaxSlugBytes || !utf8.ValidString(slug) {
		return fmt.Errorf("%w: carry %s %q", ErrInvalidAccessRequest, what, slug)
	}
	return nil
}

// IsZero reports that c states nothing.
func (c Carry) IsZero() bool { return c.Home == "" && len(c.Holds) == 0 && len(c.WriteTo) == 0 }

// Flows lists what approving c clears: each held project into each write
// destination other than itself, sorted.
func (c Carry) Flows() []FlowPair {
	var out []FlowPair
	for _, from := range c.Holds {
		for _, to := range c.WriteTo {
			if from != to {
				out = append(out, FlowPair{From: from, To: to})
			}
		}
	}
	return out
}

// with returns c as the owner approved it for project: held, and a write
// destination exactly when the approval is write -- the owner's choice of
// access, not the request's, decides whether project may be written.
func (c Carry) with(project string, write bool) Carry {
	out := Carry{Home: c.Home, Holds: slices.Clone(c.Holds)}
	if !slices.Contains(out.Holds, project) {
		out.Holds = append(out.Holds, project)
		slices.Sort(out.Holds)
	}
	for _, d := range c.WriteTo {
		if d != project {
			out.WriteTo = append(out.WriteTo, d)
		}
	}
	if write {
		out.WriteTo = append(out.WriteTo, project)
		slices.Sort(out.WriteTo)
	}
	return out
}

// carryJSON encodes c for the carry column: "" for the zero carry. It
// normalizes first, so a carry that did not come from CarryOf is checked too.
func carryJSON(c Carry) (string, error) {
	if c.IsZero() {
		return "", nil
	}
	n, err := CarryOf(c.Home, c.Holds, c.WriteTo)
	if err != nil {
		return "", err
	}
	b, err := json.Marshal(n)
	if err != nil {
		return "", fmt.Errorf("encode carry: %w", err)
	}
	return string(b), nil
}

// carryFromJSON decodes the carry column ("" is the zero carry).
func carryFromJSON(s string) (Carry, error) {
	if s == "" {
		return Carry{}, nil
	}
	var c Carry
	if err := json.Unmarshal([]byte(s), &c); err != nil {
		return Carry{}, fmt.Errorf("decode carry: %w", err)
	}
	return CarryOf(c.Home, c.Holds, c.WriteTo)
}

// FlowPair is one flow: content of From may be written into To.
type FlowPair struct {
	From, To string
}

// Flow is one row of access_flows: for Holder, content of From may be written
// into To, as approval ApprovalID listed.
type Flow struct {
	ID         string
	ApprovalID string
	Holder     Holder
	From, To   string
	CreatedAt  time.Time
}

const flowCols = `id, approval_id, holder, from_project, to_project, created_at`

func flowScan(rows *sql.Rows) ([]Flow, error) {
	var out []Flow
	for rows.Next() {
		var (
			f       Flow
			holder  string
			created string
		)
		if err := rows.Scan(&f.ID, &f.ApprovalID, &holder, &f.From, &f.To, &created); err != nil {
			return nil, err
		}
		var err error
		if f.Holder, err = HolderFromKey(holder); err != nil {
			return nil, fmt.Errorf("flow %s holder: %w", f.ID, err)
		}
		if f.CreatedAt, err = core.ParseTime(created); err != nil {
			return nil, fmt.Errorf("flow %s created_at: %w", f.ID, err)
		}
		out = append(out, f)
	}
	return out, rows.Err()
}

// flowRecordTx records, inside an approval's transaction, each pair as a flow
// approval approvalID cleared for holder. A pair the holder already has keeps
// the approval that first listed it. It returns the flows it added.
func flowRecordTx(ctx context.Context, tx *sql.Tx, holder Holder, approvalID string, pairs []FlowPair, now time.Time) ([]Flow, error) {
	if holder.IsZero() || approvalID == "" {
		return nil, fmt.Errorf("%w: a flow names its holder and its approval", ErrInvalidAccessRequest)
	}
	var added []Flow
	for _, p := range pairs {
		if p.From == "" || p.To == "" || p.From == p.To {
			return nil, fmt.Errorf("%w: flow %q -> %q", ErrInvalidAccessRequest, p.From, p.To)
		}
		id, err := core.NewID()
		if err != nil {
			return nil, err
		}
		res, err := tx.ExecContext(ctx, `INSERT INTO access_flows (`+flowCols+`) VALUES (?, ?, ?, ?, ?, ?)
			ON CONFLICT (holder, from_project, to_project) DO NOTHING`,
			id, approvalID, holder.Key(), p.From, p.To, core.FormatTime(now))
		if err != nil {
			return nil, fmt.Errorf("record flow %s -> %s: %w", p.From, p.To, err)
		}
		n, err := res.RowsAffected()
		if err != nil {
			return nil, fmt.Errorf("record flow %s -> %s: %w", p.From, p.To, err)
		}
		if n == 1 {
			added = append(added, Flow{ID: id, ApprovalID: approvalID, Holder: holder, From: p.From, To: p.To, CreatedAt: now})
		}
	}
	return added, nil
}

// FlowsForHolder returns the flows approvals recorded for any of holders,
// oldest first (then by pair), at most flowMaxRows. It lists rows; HolderFlows is the set a
// fence judges, which adds the flows grants and moves imply.
func FlowsForHolder(ctx context.Context, db *sql.DB, holders []Holder) ([]Flow, error) {
	clause, args, ok, err := holderKeyClause("holder", holders)
	if err != nil {
		return nil, fmt.Errorf("store.FlowsForHolder: %w", err)
	}
	if !ok {
		return nil, nil
	}
	rows, err := db.QueryContext(ctx, `SELECT `+flowCols+` FROM access_flows WHERE `+clause+`
		ORDER BY created_at, from_project, to_project, id LIMIT `+fmt.Sprint(flowMaxRows), args...)
	if err != nil {
		return nil, fmt.Errorf("store.FlowsForHolder: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out, err := flowScan(rows)
	if err != nil {
		return nil, fmt.Errorf("store.FlowsForHolder: %w", err)
	}
	return out, nil
}

// Where a HolderFlow comes from.
const (
	// HolderFlowRecorded is a flow an approval listed (access_flows).
	HolderFlowRecorded = "flow"
	// HolderFlowGrant is a flow a grant implies: its target into the project
	// it was asked from, and for a write grant that project into its target.
	HolderFlowGrant = "grant"
	// HolderFlowMove is a flow a move implies: the home it left into the
	// project it entered.
	HolderFlowMove = "move"
)

// HolderFlow is one flow a holder may write by: content of From may be written
// into To. ApprovalID is the approval that first cleared it and ApprovalKind
// that approval's kind (AccessKindGrant or AccessKindMove), what a provenance
// stamp names; Via says which record cleared it.
type HolderFlow struct {
	From, To     string
	ApprovalID   string
	ApprovalKind string
	Via          string
	At           time.Time
}

// HolderFlows returns every flow cleared for any of holders, one per pair,
// sorted by From then To: the ones approvals recorded, plus the ones the
// holders' grants imply (live or ended -- a grant that ended does not unread
// what it read) and their moves imply. Where several records clear the same
// pair the earliest is named. It is the second input to the one rule (with
// HolderLabel); this function judges nothing.
func HolderFlows(ctx context.Context, db *sql.DB, holders []Holder) ([]HolderFlow, error) {
	const op = "store.HolderFlows"
	hs, err := holderLookup(holders)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	if len(hs) == 0 {
		return nil, nil
	}
	best := map[FlowPair]HolderFlow{}
	add := func(f HolderFlow) {
		k := FlowPair{From: f.From, To: f.To}
		cur, ok := best[k]
		if !ok || f.At.Before(cur.At) || (f.At.Equal(cur.At) && f.ApprovalID < cur.ApprovalID) {
			best[k] = f
		}
	}

	recorded, err := flowRecorded(ctx, db, hs)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	for _, f := range recorded {
		add(f)
	}

	grants, err := HolderAccessGrants(ctx, db, hs)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	for _, g := range grants {
		add(HolderFlow{From: g.Project, To: g.FromProject, ApprovalID: g.ApprovalID, ApprovalKind: AccessKindGrant,
			Via: HolderFlowGrant, At: g.GrantedAt})
		if g.Access == GrantAccessWrite {
			add(HolderFlow{From: g.FromProject, To: g.Project, ApprovalID: g.ApprovalID, ApprovalKind: AccessKindGrant,
				Via: HolderFlowGrant, At: g.GrantedAt})
		}
	}

	moves, err := MovesForHolder(ctx, db, hs)
	if err != nil {
		return nil, fmt.Errorf("%s: %w", op, err)
	}
	for _, m := range moves {
		if m.FromProject != "" {
			add(HolderFlow{From: m.FromProject, To: m.ToProject, ApprovalID: m.ApprovalID, ApprovalKind: AccessKindMove,
				Via: HolderFlowMove, At: m.MovedAt})
		}
	}

	out := make([]HolderFlow, 0, len(best))
	for _, f := range best {
		out = append(out, f)
	}
	slices.SortFunc(out, func(a, b HolderFlow) int {
		if c := strings.Compare(a.From, b.From); c != 0 {
			return c
		}
		return strings.Compare(a.To, b.To)
	})
	return out, nil
}

// flowRecorded returns the flows approvals recorded for holders, each with its
// approval's kind, oldest first.
func flowRecorded(ctx context.Context, db *sql.DB, holders []Holder) ([]HolderFlow, error) {
	clause, args, ok, err := holderKeyClause("f.holder", holders)
	if err != nil || !ok {
		return nil, err
	}
	rows, err := db.QueryContext(ctx, `SELECT f.from_project, f.to_project, f.approval_id, a.kind, f.created_at
		FROM access_flows f JOIN access_approvals a ON a.id = f.approval_id
		WHERE `+clause+` ORDER BY f.created_at, f.from_project, f.to_project, f.id LIMIT `+fmt.Sprint(flowMaxRows), args...)
	if err != nil {
		return nil, fmt.Errorf("recorded flows: %w", err)
	}
	defer func() { _ = rows.Close() }()
	var out []HolderFlow
	for rows.Next() {
		var (
			f       = HolderFlow{Via: HolderFlowRecorded}
			created string
		)
		if err := rows.Scan(&f.From, &f.To, &f.ApprovalID, &f.ApprovalKind, &created); err != nil {
			return nil, fmt.Errorf("recorded flows: %w", err)
		}
		if f.At, err = core.ParseTime(created); err != nil {
			return nil, fmt.Errorf("recorded flow created_at: %w", err)
		}
		out = append(out, f)
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("recorded flows: %w", err)
	}
	return out, nil
}
