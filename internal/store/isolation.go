// Project isolation: the policy fence against agent-to-agent knowledge
// leakage. Every enforcement point (write funnel, read funnel, by-id paths,
// briefing, gardener) routes through CanRead/CanWrite so the whole matrix
// lives here and in one table-driven test. Like the favorite setters, the
// state UPDATE never bumps updated_at -- isolation is owner curation, not
// authorship, and must not churn recency sorts.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
)

// ErrProjectNotFound is returned when a slug has no projects-table row.
var ErrProjectNotFound = errors.New("project not found")

// ErrIsolationHasChildren is returned by TightenProjectIsolation when the
// project still has child projects. Isolation requires a standalone project,
// and re-parenting someone else's children is not this call's decision to make.
var ErrIsolationHasChildren = errors.New("project has child projects")

// ErrNotATighten is returned when the requested state is not tighter than the
// current one. Loosening is immediate and ceremony-free -- nothing has leaked
// while the fence was up -- so it goes through SetProjectIsolation instead.
var ErrNotATighten = errors.New("not a tighten")

// ErrIsolationStandalone is returned by SetProjectParent when either side of a
// parent link is isolated. It is the same topology rule ErrIsolationHasChildren
// enforces from the tighten side, judged from the link side: isolation requires
// a standalone project, so a fenced project may neither take a parent nor be
// one.
//
// It is deliberately NOT an alias of gardener.ErrIsolatedProject: that sentinel
// answers a different question (whether a fenced project may be SPLIT), and one
// name for two refusals would make errors.Is agree where the two do not.
var ErrIsolationStandalone = errors.New("isolation requires a standalone project")

// SetProjectIsolation sets a project's isolation state without bumping
// updated_at. Unlike the favorite setters it does NOT treat an unknown slug as
// a no-op: a fence the owner believes is up but never applied fails open, so
// not-found is an error here.
func SetProjectIsolation(ctx context.Context, db *sql.DB, slug string, state core.Isolation) error {
	if !state.Valid() {
		return fmt.Errorf("store.SetProjectIsolation: invalid isolation %q", state)
	}
	res, err := db.ExecContext(ctx,
		`UPDATE projects SET isolation = ? WHERE slug = ?`, string(state), slug)
	if err != nil {
		return fmt.Errorf("store.SetProjectIsolation: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("store.SetProjectIsolation: %w", err)
	}
	if n == 0 {
		return fmt.Errorf("store.SetProjectIsolation: %w: %q", ErrProjectNotFound, slug)
	}
	return nil
}

// IsolationOf returns a project's isolation state. The global scope ("") is
// never isolated, and an unregistered slug is open by construction -- a
// project must have a projects-table row before it can be fenced.
func IsolationOf(ctx context.Context, db *sql.DB, slug string) (core.Isolation, error) {
	state, err := isolationOfTx(ctx, db, slug)
	if err != nil {
		return "", fmt.Errorf("store.IsolationOf: %w", err)
	}
	return state, nil
}

// isolationOfTx is IsolationOf over any read executor, so a topology mutator can
// judge the fence inside the transaction it is about to write under rather than
// on the pool, where a tighten could land between the check and the UPDATE.
// rowQuerier carries no QueryRowContext, hence the rows dance for one column.
func isolationOfTx(ctx context.Context, q rowQuerier, slug string) (core.Isolation, error) {
	if slug == "" {
		return core.IsolationOpen, nil
	}
	rows, err := q.QueryContext(ctx,
		`SELECT isolation FROM projects WHERE slug = ? LIMIT 1`, slug)
	if err != nil {
		return "", err
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return "", err
		}
		return core.IsolationOpen, nil
	}
	var state string
	if err := rows.Scan(&state); err != nil {
		return "", err
	}
	return core.Isolation(state), nil
}

// CanRead reports whether a session bound to callerProject may read
// targetProject's knowledge ("" is the global scope). A scope always reads
// itself; a fenced (confidential or sealed) target never leaks to another
// scope; a sealed caller reads nothing outside itself, global included.
//
// Before any of that, the request's peer_auth View (internal/access) has the
// first word: a project the caller's OS account may not reach is unreadable
// whatever the binding says -- and that includes the caller's OWN bound
// project, so a binding to a sealed project that slipped past every other gate
// still reads nothing. With peer_auth off no View is set and this check is a
// no-op.
func CanRead(ctx context.Context, db *sql.DB, callerProject, targetProject string) (bool, error) {
	if !access.Visible(ctx, targetProject) || !access.Visible(ctx, callerProject) {
		return false, nil
	}
	if callerProject == targetProject {
		return true, nil
	}
	target, err := IsolationOf(ctx, db, targetProject)
	if err != nil {
		return false, fmt.Errorf("store.CanRead: %w", err)
	}
	if target.FencesOutbound() {
		return false, nil
	}
	caller, err := IsolationOf(ctx, db, callerProject)
	if err != nil {
		return false, fmt.Errorf("store.CanRead: %w", err)
	}
	return !caller.FencesInbound(), nil
}

// ProjectIsolation pairs a project slug with its isolation state.
type ProjectIsolation struct {
	Slug  string         `json:"slug"`
	State core.Isolation `json:"state"`
}

// IsolatedSlugs returns the isolated projects among slugs, in the order given,
// each with the state that fences it. Topology writers call it to refuse a link
// before building one: isolation requires a standalone project, so a fenced slug
// may not join a family or take a parent. Blank and unregistered slugs are open
// by construction and never appear.
func IsolatedSlugs(ctx context.Context, db *sql.DB, slugs []string) ([]ProjectIsolation, error) {
	out, err := isolatedSlugsTx(ctx, db, slugs)
	if err != nil {
		return nil, fmt.Errorf("store.IsolatedSlugs: %w", err)
	}
	return out, nil
}

// isolatedSlugsTx is IsolatedSlugs over any read executor, for a topology writer
// that guards inside its own transaction (see SetProjectParent).
func isolatedSlugsTx(ctx context.Context, q rowQuerier, slugs []string) ([]ProjectIsolation, error) {
	var out []ProjectIsolation
	for _, slug := range slugs {
		slug = strings.TrimSpace(slug)
		if slug == "" {
			continue
		}
		state, err := isolationOfTx(ctx, q, slug)
		if err != nil {
			return nil, err
		}
		if state.FencesOutbound() {
			out = append(out, ProjectIsolation{Slug: slug, State: state})
		}
	}
	return out, nil
}

// isolatedLabels renders "<slug> is <state>" for each fenced project, joined for
// a refusal's copy. It names the slug and the state and NOTHING else: a topology
// refusal must not hide that the project exists (the owner needs to know which
// side to unfence) and must not echo a byte of what it holds.
func isolatedLabels(blocked []ProjectIsolation) string {
	parts := make([]string, len(blocked))
	for i, b := range blocked {
		parts[i] = fmt.Sprintf("%s is %s", b.Slug, b.State)
	}
	return strings.Join(parts, ", ")
}

// IsolationTightenEffects reports what tightening one project's isolation would
// change, or what blocks it. Isolation requires a standalone project (the
// topology rule), so a tighten detaches the project from every family and from
// its parent, and refuses outright while it still has children.
//
// PreviewIsolationTighten produces it without mutating anything -- the console's
// confirm panel and the CLI's --yes path describe the change from it -- and
// TightenProjectIsolation returns it again after performing exactly that.
type IsolationTightenEffects struct {
	// Project is the slug being tightened.
	Project string `json:"project"`
	// From is the project's current state, To the requested one.
	From core.Isolation `json:"from"`
	To   core.Isolation `json:"to"`
	// Families names every family the project would be removed from, sorted.
	Families []string `json:"families,omitempty"`
	// Parent is the parent slug the tighten would detach from, "" when the
	// project already has no parent.
	Parent string `json:"parent,omitempty"`
	// Children lists the slugs parented to this project, sorted. Non-empty means
	// the tighten is BLOCKED: they must be re-parented first, and an apply writes
	// nothing.
	Children []string `json:"children,omitempty"`
}

// Blocked reports whether child projects prevent the tighten.
func (e IsolationTightenEffects) Blocked() bool { return len(e.Children) > 0 }

// DetachesTopology reports whether applying would change topology (leave a
// family, clear the parent link) rather than just flip the isolation flag. It is
// what makes the confirm step consequential.
func (e IsolationTightenEffects) DetachesTopology() bool {
	return len(e.Families) > 0 || e.Parent != ""
}

// PreviewIsolationTighten reports what tightening slug to state would change,
// mutating nothing.
//
// Blocking children are part of the report, not an error: read Blocked() and
// render Children so the owner can re-parent them. Errors are reserved for what
// makes the question unanswerable -- an unknown slug (ErrProjectNotFound), an
// unrecognized state, or a state that is not tighter than the current one
// (ErrNotATighten; loosening needs no ceremony, call SetProjectIsolation).
// Requesting the state the project is already in is a legal no-op tighten: it
// re-asserts the standalone rule instead of erroring on a double submit.
func PreviewIsolationTighten(ctx context.Context, db *sql.DB, slug string, state core.Isolation) (IsolationTightenEffects, error) {
	return isolationTightenEffects(ctx, db, slug, state)
}

// TightenProjectIsolation applies exactly what PreviewIsolationTighten
// described, in one transaction: leave every family, clear the parent link, then
// set the isolation state. It recomputes the effects inside that transaction and
// returns them, so a caller that previewed first can see whether the world moved
// in between. With children present nothing is written and the error is
// ErrIsolationHasChildren -- the returned effects still list them.
//
// The isolation UPDATE never bumps updated_at (isolation is owner curation, not
// authorship, and must not churn recency sorts); the parent detach does, because
// that is a real topology change and SetProjectParent's contract.
func TightenProjectIsolation(ctx context.Context, db *sql.DB, slug string, state core.Isolation, now time.Time) (IsolationTightenEffects, error) {
	tx, err := db.BeginTx(ctx, nil)
	if err != nil {
		return IsolationTightenEffects{}, fmt.Errorf("store.TightenProjectIsolation: begin: %w", err)
	}
	defer func() { _ = tx.Rollback() }() // no-op after Commit

	eff, err := isolationTightenEffects(ctx, tx, slug, state)
	if err != nil {
		return IsolationTightenEffects{}, err
	}
	if eff.Blocked() {
		return eff, fmt.Errorf("store.TightenProjectIsolation: %q %w: re-parent %s first",
			eff.Project, ErrIsolationHasChildren, strings.Join(eff.Children, ", "))
	}

	if len(eff.Families) > 0 {
		families, err := projectFamiliesTx(ctx, tx)
		if err != nil {
			return eff, fmt.Errorf("store.TightenProjectIsolation: %w", err)
		}
		for _, name := range eff.Families {
			// setProjectFamiliesTx drops a family left with no members.
			families[name] = slices.DeleteFunc(slices.Clone(families[name]),
				func(m string) bool { return m == eff.Project })
		}
		if err := setProjectFamiliesTx(ctx, tx, families); err != nil {
			return eff, fmt.Errorf("store.TightenProjectIsolation: %w", err)
		}
	}
	if eff.Parent != "" {
		if _, err := tx.ExecContext(ctx,
			`UPDATE projects SET parent_slug = '', updated_at = ? WHERE slug = ?`,
			core.FormatTime(now.UTC()), eff.Project); err != nil {
			return eff, fmt.Errorf("store.TightenProjectIsolation: detach parent: %w", err)
		}
	}
	res, err := tx.ExecContext(ctx,
		`UPDATE projects SET isolation = ? WHERE slug = ?`, string(eff.To), eff.Project)
	if err != nil {
		return eff, fmt.Errorf("store.TightenProjectIsolation: %w", err)
	}
	n, err := res.RowsAffected()
	if err != nil {
		return eff, fmt.Errorf("store.TightenProjectIsolation: %w", err)
	}
	if n == 0 {
		return eff, fmt.Errorf("store.TightenProjectIsolation: %w: %q", ErrProjectNotFound, eff.Project)
	}
	if err := tx.Commit(); err != nil {
		return eff, fmt.Errorf("store.TightenProjectIsolation: commit: %w", err)
	}
	return eff, nil
}

// isolationExecutor is the read+write subset shared by *sql.DB and *sql.Tx that
// the tighten path needs: the topology reads (rowQuerier) plus the families
// read-modify-write (settingsExecutor). It is what lets the preview run on the
// pool and the apply run the identical computation inside its transaction --
// the pool is capped at one connection (see Open), so an apply that reached for
// the *sql.DB helpers mid-transaction would deadlock rather than disagree.
type isolationExecutor interface {
	rowQuerier
	settingsExecutor
}

// isolationTightenEffects computes the tighten report via any executor. It is
// the single implementation behind both the preview and the apply, so what the
// owner confirms is what the transaction performs.
func isolationTightenEffects(ctx context.Context, q isolationExecutor, slug string, state core.Isolation) (IsolationTightenEffects, error) {
	slug = strings.TrimSpace(slug)
	if slug == "" {
		// The global scope has no row and is never isolated; guarding here also
		// keeps the children query off `parent_slug = ''`, which matches every
		// rootless project.
		return IsolationTightenEffects{}, fmt.Errorf("store: tighten isolation: %w: %q", ErrProjectNotFound, slug)
	}
	if !state.Valid() {
		return IsolationTightenEffects{}, fmt.Errorf("store: tighten isolation: invalid isolation %q", state)
	}
	p, ok, err := projectBySlugTx(ctx, q, slug)
	if err != nil {
		return IsolationTightenEffects{}, fmt.Errorf("store: tighten isolation: %w", err)
	}
	if !ok {
		return IsolationTightenEffects{}, fmt.Errorf("store: tighten isolation: %w: %q", ErrProjectNotFound, slug)
	}
	from := p.Isolation
	if from == "" {
		from = core.IsolationOpen // core.Project documents "" as open
	}
	if !from.Valid() {
		return IsolationTightenEffects{}, fmt.Errorf(
			"store: tighten isolation: project %q holds unrecognized isolation %q", slug, p.Isolation)
	}
	if !state.FencesOutbound() || isolationRank(state) < isolationRank(from) {
		return IsolationTightenEffects{}, fmt.Errorf("store: tighten isolation: %s -> %s is %w", from, state, ErrNotATighten)
	}

	eff := IsolationTightenEffects{Project: slug, From: from, To: state, Parent: p.ParentSlug}

	rows, err := q.QueryContext(ctx,
		`SELECT slug FROM projects WHERE parent_slug = ? ORDER BY slug`, slug)
	if err != nil {
		return IsolationTightenEffects{}, fmt.Errorf("store: tighten isolation: children: %w", err)
	}
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var child string
		if err := rows.Scan(&child); err != nil {
			return IsolationTightenEffects{}, fmt.Errorf("store: tighten isolation: children: %w", err)
		}
		eff.Children = append(eff.Children, child)
	}
	if err := rows.Err(); err != nil {
		return IsolationTightenEffects{}, fmt.Errorf("store: tighten isolation: children: %w", err)
	}

	families, err := projectFamiliesTx(ctx, q)
	if err != nil {
		return IsolationTightenEffects{}, fmt.Errorf("store: tighten isolation: %w", err)
	}
	for name, members := range families {
		if slices.Contains(members, slug) {
			eff.Families = append(eff.Families, name)
		}
	}
	slices.Sort(eff.Families)
	return eff, nil
}

// isolationRank orders the states loosest-first so a tighten can be told from a
// loosen. core.Isolations is declared loosest-first, so the rank is its index.
func isolationRank(i core.Isolation) int { return slices.Index(core.Isolations, i) }

// GlobalMemoriesFromProjectSessions returns the ACTIVE global-scope memories
// whose source session was bound to project, newest-updated first. It is the
// provenance audit behind a tighten: knowledge this project's agents wrote
// OUTSIDE the fence before the fence existed, which no read/write check can
// reach afterwards because the memory itself is global.
//
// The source_session stamp is matched against both spellings a memory can carry
// -- the session NAME for ambient stamps (cc/ab12cd34), the session ULID for
// bound ones (see MemoriesForSession) -- via a UNION subquery rather than a
// JOIN, so a stamp can never match two session rows and duplicate a memory.
//
// It reports only; relocating is a gardener proposal the owner applies.
func GlobalMemoriesFromProjectSessions(ctx context.Context, db *sql.DB, project string) ([]core.Memory, error) {
	project = strings.TrimSpace(project)
	if project == "" {
		// project_slug = '' marks real global sessions, so "" would ask "which
		// global memories came from a global session" -- never the audit's
		// question, and silently answering it would overstate the leak.
		return nil, errors.New("store.GlobalMemoriesFromProjectSessions: empty project is ambiguous -- project_slug='' rows are real global sessions; pass an explicit slug")
	}
	rows, err := db.QueryContext(ctx, `SELECT `+memoryCols+`
		FROM memories_index
		WHERE project = '' AND invalid_at IS NULL AND source_session <> ''
		  AND source_session IN (
		      SELECT id FROM sessions WHERE project_slug = ?
		      UNION
		      SELECT name FROM sessions WHERE project_slug = ?
		  )
		ORDER BY updated_at DESC, id DESC`, project, project)
	if err != nil {
		return nil, fmt.Errorf("store.GlobalMemoriesFromProjectSessions: %w", err)
	}
	defer func() { _ = rows.Close() }()
	mems, err := scanMemories(rows)
	if err != nil {
		return nil, fmt.Errorf("store.GlobalMemoriesFromProjectSessions: %w", err)
	}
	return mems, nil
}

// CanWrite reports whether a session bound to callerProject may write into
// targetProject ("" is the global scope). A scope always writes itself; a
// fenced caller writes nowhere else -- an agent-initiated outside write,
// global included, is an outbound leak; a sealed target admits nothing from
// outside, while a confidential target stays writable from outside (inbound
// is unchanged -- relocating knowledge INTO the fence is how it gets there).
//
// The peer_auth View has the first word here too: a configured OS user writes
// only into its own projects (never the global scope), and the owner never
// writes into a project sealed to another user.
func CanWrite(ctx context.Context, db *sql.DB, callerProject, targetProject string) (bool, error) {
	if !access.Writable(ctx, targetProject) || !access.Visible(ctx, callerProject) {
		return false, nil
	}
	if callerProject == targetProject {
		return true, nil
	}
	caller, err := IsolationOf(ctx, db, callerProject)
	if err != nil {
		return false, fmt.Errorf("store.CanWrite: %w", err)
	}
	if caller.FencesOutbound() {
		return false, nil
	}
	target, err := IsolationOf(ctx, db, targetProject)
	if err != nil {
		return false, fmt.Errorf("store.CanWrite: %w", err)
	}
	return !target.FencesInbound(), nil
}
