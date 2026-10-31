package retrieve

import (
	"context"
	"errors"
	"fmt"
	"slices"
	"sort"
	"time"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/llm"
	"github.com/arctop/seamless/internal/store"
)

// rrfK is the Reciprocal Rank Fusion constant. Fusing by rank (not raw score)
// makes semantic and FTS results comparable despite living on different scales,
// and damps the influence of any single source's top rank.
const rrfK = 60

const (
	// DefaultRecallLimit is used when a Go caller leaves RecallInput.Limit at
	// its zero value.
	DefaultRecallLimit = 10
	// MaxRecallLimit is the service-level ceiling shared by MCP and A2A. Recall
	// has a fixed candidate depth and token budget; accepting more only enlarged
	// a caller-controlled preallocation without producing more useful results.
	MaxRecallLimit = 100
)

// ErrInvalidRecallLimit marks a RecallInput limit outside the service contract.
var ErrInvalidRecallLimit = errors.New("invalid recall limit")

// recallSourceDepth is how many candidates to pull from each source before
// fusing; a few multiples of the requested limit gives RRF room to reorder.
const recallSourceDepth = 24

// linkExpandFrom is how many of the top fused memory hits are scanned for
// [[name]] links; each linked neighbor is pulled in as a third retrieval signal.
const linkExpandFrom = 5

// favoriteBoost multiplies a favorited item's fused score in Recall. It is
// applied after fusion and link expansion, so it can promote a favorite past
// similarly-ranked non-favorites but can never resurrect an item the candidate
// queries (scope, validity, depth) already excluded. Console Search does not
// use it: the search page partitions favorites in its own sort instead, keeping
// the semantic floor untouched.
const favoriteBoost = 1.15

// recallUtilityBoost scales the utility multiplier on a fused score:
// 1 + boost*utility, so the range is [1.0, 1.10). Like favoriteBoost it is
// applied post-fusion, after link expansion -- it can promote a proven memory
// past near-ties (adjacent RRF ranks differ by ~1.5-3%) but can never
// resurrect an item the candidate queries already excluded, and it stays
// strictly below favoriteBoost so an explicit star always beats implicit
// demand. Console Search never enters this path.
const recallUtilityBoost = 0.10

// Hit is one recall result. Kind tells the agent how to read it: a memory by its
// Name (memory_read), a note by its ID (notes_read), a task by its ID
// (tasks_list id=), a trial by its lab (trial_query lab=), a session by the
// findings already carried in Description.
type Hit struct {
	Kind        string  `json:"kind"` // core.ItemKind*: memory | note | task | trial | session
	ID          string  `json:"id"`
	Name        string  `json:"name"`  // memory name / note slug / task plan slug / trial lab / session name
	Title       string  `json:"title"` // display title
	Description string  `json:"description"`
	Project     string  `json:"project,omitempty"`
	Age         string  `json:"age"`
	Source      string  `json:"source"` // semantic | fts | fused | identifier | link | browse
	Score       float64 `json:"score"`
	// Status is the work-record kinds' lifecycle word -- a task's status, a
	// trial's outcome, a session's status. It is what tells an agent whether the
	// task it just found is still open or was dropped two months ago, which is
	// the difference between acting on a hit and merely learning from it.
	// Memories and notes have no such word and omit it, so the knowledge payload
	// is unchanged from what it was before the work record joined the corpus.
	Status string `json:"status,omitempty"`
	// Snippet is the matched text in context, with the matched terms wrapped in
	// store.SnippetStartMark/SnippetEndMark. Only Search sets it, and only for
	// hits an FTS leg found -- Recall always leaves it empty, so omitempty keeps
	// the MCP recall payload byte-identical to what it was before Search existed.
	// It is RAW item text: a renderer must HTML-escape before substituting the
	// sentinels for markup.
	Snippet string `json:"snippet,omitempty"`
	// Similarity is the raw cosine similarity from the semantic leg, for hits
	// that leg found. Like Snippet, only Search sets it -- Recall always leaves
	// it zero, so omitempty keeps the MCP recall payload unchanged. A lexical-only
	// hit has no vector distance to report and also omits it.
	Similarity float64 `json:"similarity,omitempty"`
	// Favorite marks a starred item. omitempty keeps the recall payload
	// byte-identical to the pre-favorites contract for unstarred hits.
	Favorite bool `json:"favorite,omitempty"`
	// IdentifierMatch is set only by the human-facing Search entry point when
	// the query matched a stable id, memory name, or note slug. Recall never
	// sets it, so the agent-facing payload remains unchanged.
	IdentifierMatch store.IdentifierMatchKind `json:"identifierMatch,omitempty"`
	// Updated is carried internally so the human search page can sort and
	// window hydrated hits. It is deliberately excluded from Recall's JSON
	// contract, whose payload predates console Search and must remain stable.
	Updated time.Time `json:"-"`
}

// RecallScopes lists every valid RecallInput.Scope value. An empty Scope is a
// valid Go-API default meaning "all" (recallScopeKinds treats it that way), so
// this set is for boundaries that can distinguish an absent key from an
// explicitly wrong one -- it is not a precondition of RecallInput.
//
// "all" spans durable knowledge AND the work record: an agent asking "why was
// this dropped" should not have to know in advance whether the answer was
// written as a memory, a note, a task body, a trial, or a session's findings.
// The narrow scopes exist for a caller that does know.
var RecallScopes = []string{"all", "memories", "notes", "tasks", "trials", "sessions"}

// knowledgeScopes are the RecallScopes that a memory-kind filter can coexist
// with. Everything else names a corpus with no frontmatter kind, so pairing it
// with Kind can only ever match nothing.
var knowledgeScopes = []string{"", "all", "memories"}

// RecallInput parameterizes a recall. Project is the session's bound scope;
// results are limited to that project plus global items.
type RecallInput struct {
	// Query is the search text. It may be empty only when Kind is set: a kind
	// with no query is the browse mode (list that kind newest-first), the
	// mechanism behind briefing hints like "recall kind=convention".
	Query   string
	Project string
	Scope   string // RecallScopes (default all)
	// Kind, when non-empty, restricts hits to memories of that frontmatter
	// kind (core.MemoryKinds). It implies memories-only, so any Scope that
	// excludes memories is rejected as contradictory, and link expansion is
	// skipped: a kind filter is a targeted enumeration, and a [[link]] neighbor
	// of another kind would violate it.
	Kind  string
	Limit int
}

type fusedItem struct {
	kind            string
	score           float64
	semantic        bool
	fts             bool
	linked          bool // pulled in via a [[name]] link from a top hit
	snippet         string
	cosine          float64 // raw semantic-leg similarity; meaningful only when semantic
	identifierMatch store.IdentifierMatchKind
	identifierOrder int
}

// candidates runs both retrieval legs and fuses them with RRF, returning the
// accumulator keyed by item id. It is the shared heart of Recall and Search:
// the semantic/FTS legs, the RRF weighting, and -- critically -- the embedder
// degradation contract (a local ErrConfig surfaces; a remote failure warns and
// falls back to lexical-only) live here once, so the two entry points cannot
// drift on the behavior that matters most when the provider misbehaves.
//
// semantic requests the cosine leg; it is skipped when no embedder is
// configured. projects/kinds/memKind are applied inside the candidate queries
// (never after fusion) so the whole depth stays in scope.
//
// The FTS leg always asks for snippets: the snippet projection cannot perturb
// which rows return or in what order (pinned by
// store.TestFTSSearch_SnippetVariantMatchesPlainOrdering), so Recall paying for
// a snippet it discards is cheaper than two divergent query paths.
func (s *Service) candidates(ctx context.Context, query string, kinds, projects []string, memKind string, since time.Time, depth int, semantic bool, who string) (map[string]*fusedItem, error) {
	acc := make(map[string]*fusedItem)
	add := func(itemID, kind string, rank int, isSemantic bool, snippet string, cosine float64) {
		f := acc[itemID]
		if f == nil {
			f = &fusedItem{kind: kind}
			acc[itemID] = f
		}
		f.score += 1.0 / float64(rrfK+rank+1)
		if isSemantic {
			f.semantic = true
			f.cosine = cosine
		} else {
			f.fts = true
			if snippet != "" && f.snippet == "" {
				f.snippet = snippet
			}
		}
	}

	if semantic && s.embedder != nil {
		qvec, err := s.embedder.Embed(ctx, query)
		switch {
		case errors.Is(err, llm.ErrConfig):
			// A client that cannot build its own request is a local defect, not
			// a provider outage. Degrading here would trade a loud failure for
			// quietly worse results on every recall for the life of the daemon,
			// with nothing to tell the owner semantic search had stopped. The
			// llm factories reject a bad base_url at construction, so this is
			// unreachable in a correctly built client -- which is why reaching
			// it must surface rather than hide.
			return nil, fmt.Errorf("%s: %w", who, err)
		case err != nil:
			// The provider is unreachable, throttling, or rejecting the key:
			// nothing here is wrong and it may clear on its own, so lexical-only
			// results are honest and better than none. seamlessd doctor's
			// embedderCheck is where the owner sees this standing condition.
			s.logger.Warn(who+": embed failed, FTS only", "error", err)
		default:
			hits, err := store.CosineSearchSince(ctx, s.db, qvec, s.embedder.Model(), kinds, projects, memKind, since, depth)
			if err != nil {
				return nil, err
			}
			for rank, h := range hits {
				add(h.ItemID, h.Kind, rank, true, "", h.Score)
			}
		}
	}
	ftsHits, err := store.FTSSearchSnippetsSince(ctx, s.db, query, kinds, projects, memKind, since, depth)
	if err != nil {
		return nil, err
	}
	for rank, h := range ftsHits {
		add(h.ItemID, h.Kind, rank, false, h.Snippet, 0)
	}
	return acc, nil
}

// hydrated holds the rows behind a fused accumulator, one map per kind. Recall
// and Search share it so the per-kind projection into a Hit -- and the residual
// validity guard on memories -- cannot drift between the agent-facing and the
// human-facing entry point.
type hydrated struct {
	mems     map[string]core.Memory
	notes    map[string]core.Note
	tasks    map[string]core.Task
	trials   map[string]core.Trial
	sessions map[string]core.Session
}

// hit projects one hydrated row into a Hit. ok is false when the id was not
// hydrated (an fts/index row whose entity vanished between the candidate query
// and here) or when a memory has been invalidated since -- both cases the
// callers skip rather than surface.
func (h hydrated) hit(id, kind string) (Hit, bool) {
	switch kind {
	case core.ItemKindNote:
		n, ok := h.notes[id]
		if !ok {
			return Hit{}, false
		}
		return noteHit(n), true
	case core.ItemKindTask:
		t, ok := h.tasks[id]
		if !ok {
			return Hit{}, false
		}
		return taskHit(t), true
	case core.ItemKindTrial:
		tr, ok := h.trials[id]
		if !ok {
			return Hit{}, false
		}
		return trialHit(tr), true
	case core.ItemKindSession:
		sess, ok := h.sessions[id]
		if !ok {
			return Hit{}, false
		}
		return sessionHit(sess), true
	default:
		m, ok := h.mems[id]
		if !ok {
			return Hit{}, false
		}
		// The candidate queries already drop invalidated memories, and link
		// expansion resolves through MemoryByName, which only returns active
		// ones; this is the residual guard for a memory superseded between the
		// candidate query and this hydration.
		if !m.Active() {
			return Hit{}, false
		}
		return memoryHit(m), true
	}
}

// favorite reports whether a hydrated item is starred. Sessions have no star
// (favorites-cross-surface-contract lists the seven starrable kinds and a
// session is one of them only as a row in the sessions table, not as a search
// hit), so an unhydrated or unstarrable id simply reads false.
func (h hydrated) favorite(id, kind string) bool {
	switch kind {
	case core.ItemKindNote:
		return h.notes[id].Favorite
	case core.ItemKindTask:
		return h.tasks[id].Favorite
	case core.ItemKindTrial:
		return h.trials[id].Favorite
	case core.ItemKindSession:
		return false
	default:
		return h.mems[id].Favorite
	}
}

// hydrate loads the rows behind a fused accumulator, one batched query per kind
// present in the candidate set. A kind with no candidates costs nothing.
func (s *Service) hydrate(ctx context.Context, ordered []string, acc map[string]*fusedItem) (hydrated, error) {
	byKind := make(map[string][]string, len(core.KnowledgeItemKinds)+len(core.WorkItemKinds))
	for _, id := range ordered {
		kind := acc[id].kind
		if kind == "" {
			kind = core.ItemKindMemory
		}
		byKind[kind] = append(byKind[kind], id)
	}

	var (
		h   hydrated
		err error
	)
	if h.mems, err = store.MemoriesByIDs(ctx, s.db, byKind[core.ItemKindMemory]); err != nil {
		return hydrated{}, err
	}
	if h.notes, err = store.NotesByIDs(ctx, s.db, byKind[core.ItemKindNote]); err != nil {
		return hydrated{}, err
	}
	if h.tasks, err = store.TasksByIDs(ctx, s.db, byKind[core.ItemKindTask]); err != nil {
		return hydrated{}, err
	}
	if h.trials, err = store.TrialsByIDs(ctx, s.db, byKind[core.ItemKindTrial]); err != nil {
		return hydrated{}, err
	}
	if h.sessions, err = store.SessionsByIDs(ctx, s.db, byKind[core.ItemKindSession]); err != nil {
		return hydrated{}, err
	}
	return h, nil
}

// Recall fuses semantic (cosine) and FTS results with RRF, hydrates the winners
// from the index, and packs them into the recall token budget. Scope and
// validity are both enforced inside the candidate queries, so the fused depth is
// entirely in-scope and entirely live. With no embedder configured it degrades
// to FTS only.
func (s *Service) Recall(ctx context.Context, in RecallInput) ([]Hit, error) {
	// peer_auth: a project the caller's OS account may not read is refused here,
	// whichever surface asked (MCP recall resolved the scope through its own
	// fence first; A2A names it straight from the message metadata).
	if !access.Visible(ctx, in.Project) {
		return nil, access.Refusal(ctx, in.Project)
	}
	kinds := recallScopeKinds(in.Scope)
	if in.Kind != "" {
		// An unknown kind would silently match nothing, indistinguishable from
		// "no such knowledge" -- the MCP boundary enforces the enum, this guards
		// the Go API.
		if !core.MemoryKind(in.Kind).Valid() {
			return nil, fmt.Errorf("retrieve.Recall: unknown kind %q", in.Kind)
		}
		// A kind filter names a memory frontmatter kind, so it is memories-only
		// by definition; combined with a scope that excludes memories nothing
		// could ever match, and a silent empty result would read as "no such
		// knowledge". Reject the contradiction instead. Any other scope narrows
		// to memories.
		if !slices.Contains(knowledgeScopes, in.Scope) {
			return nil, fmt.Errorf("retrieve.Recall: kind %q filters memories; scope=%s excludes them -- drop kind or widen scope", in.Kind, in.Scope)
		}
		kinds = []string{core.ItemKindMemory}
	}
	limit := in.Limit
	if limit == 0 {
		limit = DefaultRecallLimit
	}
	if limit < 1 || limit > MaxRecallLimit {
		return nil, fmt.Errorf("retrieve.Recall: %w: got %d, want 1-%d (or 0 for the default)",
			ErrInvalidRecallLimit, in.Limit, MaxRecallLimit)
	}

	// A kind with no query is the browse mode: a premade filter ("list the
	// conventions"), not a search, so it skips the retrieval legs entirely.
	if in.Query == "" {
		if in.Kind == "" {
			return nil, errors.New("retrieve.Recall: query is required unless kind is set")
		}
		return s.browseKind(ctx, in.Kind, in.Project, limit)
	}

	// The recall scope is the bound project plus global (project "") -- except
	// for a sealed project, which reads nothing outside itself, global included
	// (globalReadable asks store.CanRead). Filtering happens in the candidate
	// queries themselves: filtering only after fusion over a fixed depth would
	// let a query dominated by out-of-scope hits starve in-scope matches that
	// rank deeper than recallSourceDepth. Narrowing the scope is the whole of
	// the fence here -- fusion, boosts, budgeting, and the Hit payload are
	// untouched, so the recall JSON contract stays byte-identical.
	globalVisible, err := s.globalReadable(ctx, in.Project)
	if err != nil {
		return nil, err
	}
	projects := []string{in.Project}
	if in.Project != "" && globalVisible {
		projects = append(projects, "")
	}

	acc, err := s.candidates(ctx, in.Query, kinds, projects, in.Kind, time.Time{}, recallSourceDepth, true, "retrieve.Recall")
	if err != nil {
		return nil, err
	}
	ordered := rankFused(acc)

	hyd, err := s.hydrate(ctx, ordered, acc)
	if err != nil {
		return nil, err
	}

	// Link expansion: pull in memories referenced by [[name]] links in the top
	// hits, as a third retrieval signal alongside semantic and FTS. Requires the
	// body reader (index rows carry no body); degrades away when it is unset.
	// expandLinks adds neighbors to both acc and the memory map. Skipped under a
	// kind filter: a [[link]] neighbor of another kind would violate it (see
	// RecallInput.Kind). It stays memory-only: [[name]] is memory vocabulary,
	// and the work record carries no links to follow.
	if in.Kind == "" {
		if _, err := s.expandLinks(ctx, ordered, acc, hyd.mems, in.Project, globalVisible); err != nil {
			return nil, err
		}
	}

	// Favorite and utility boosts, then one re-rank covering both and any link
	// neighbors. Hydration misses (an id in acc but not hydrated) read as
	// unfavorited zero values and are skipped in the assembly loop below anyway.
	// A failed utility read costs the boost, never the recall (same posture as
	// every other stats consumer on an agent-facing path).
	utility, err := store.UtilityScores(ctx, s.db)
	if err != nil {
		s.logger.Warn("retrieve: utility scores unavailable, recall unboosted", "error", err)
		utility = nil
	}
	for id, f := range acc {
		if hyd.favorite(id, f.kind) {
			f.score *= favoriteBoost
		}
		if u := utility[id]; u > 0 {
			f.score *= 1 + recallUtilityBoost*u
		}
	}
	ordered = rankFused(acc)

	budget := s.budgets.RecallBudgetTokens
	if budget <= 0 {
		budget = 1000
	}

	out := make([]Hit, 0, min(limit, len(ordered)))
	used := 0
	for _, id := range ordered {
		f := acc[id]
		h, ok := hyd.hit(id, f.kind)
		if !ok {
			continue
		}
		// Residual kind guard, same spirit as scopeVisible below: the candidate
		// queries already filter, this catches index drift.
		if in.Kind != "" && string(hyd.mems[id].Kind) != in.Kind {
			continue
		}
		// The candidate queries already filter to scope; this is a final guard
		// for link-expanded neighbors and any index/fts project drift -- and for
		// the caller's peer_auth reach, which the candidate queries also apply.
		if !scopeVisible(h.Project, in.Project, globalVisible) || !access.Visible(ctx, h.Project) {
			continue
		}
		h.Source = fusedSource(f)
		h.Score = f.score

		cost := estTokens(h.Title + h.Description)
		if len(out) > 0 && (len(out) >= limit || used+cost > budget) {
			break
		}
		out = append(out, h)
		used += cost
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// browseKind is Recall's no-query mode: enumerate the scope's active memories
// of one kind, newest-first, under the same limit and token budget as a
// searching recall. Deliberately list-shaped, not rank-shaped: no fusion, no
// link expansion, and no favorite or utility boost -- library rails default to
// recency, and there is no fused score to boost. The MCP layer records a
// browse's hits as passive-class exposure (source "recall-browse"), never as
// query-gated demand: a listing is the briefing's kin, and crediting it would
// let browsing reinforce the ranking it reads
// (closed-loop-utility-signal-contract).
func (s *Service) browseKind(ctx context.Context, kind, project string, limit int) ([]Hit, error) {
	mems, err := s.scopedActiveMemories(ctx, project, nil)
	if err != nil {
		return nil, err
	}
	budget := s.budgets.RecallBudgetTokens
	if budget <= 0 {
		budget = 1000
	}
	out := make([]Hit, 0, min(limit, len(mems)))
	used := 0
	for _, m := range mems {
		if string(m.Kind) != kind {
			continue
		}
		h := memoryHit(m)
		h.Source = "browse"
		cost := estTokens(h.Title + h.Description)
		if len(out) > 0 && (len(out) >= limit || used+cost > budget) {
			break
		}
		out = append(out, h)
		used += cost
		if len(out) >= limit {
			break
		}
	}
	return out, nil
}

// rankFused returns item ids ordered by fused score, ties broken by id for
// determinism.
func rankFused(acc map[string]*fusedItem) []string {
	ids := make([]string, 0, len(acc))
	for id := range acc {
		ids = append(ids, id)
	}
	sort.Slice(ids, func(i, j int) bool {
		si, sj := acc[ids[i]].score, acc[ids[j]].score
		if si != sj {
			return si > sj
		}
		return ids[i] < ids[j]
	})
	return ids
}

func fusedSource(f *fusedItem) string {
	switch {
	case f.semantic && f.fts:
		return "fused"
	case f.semantic:
		return "semantic"
	case f.fts:
		return "fts"
	case f.linked:
		return "link"
	default:
		return "fused"
	}
}

func memoryHit(m core.Memory) Hit {
	return Hit{
		Kind: core.ItemKindMemory, ID: m.ID, Name: m.Name, Title: m.Name,
		Description: sanitizeField(m.Description, 200), Project: m.Project,
		Age: humanAge(m.Updated), Favorite: m.Favorite, Updated: m.Updated,
	}
}

func noteHit(n core.Note) Hit {
	return Hit{
		Kind: core.ItemKindNote, ID: n.ID, Name: n.Slug, Title: n.Title,
		Description: sanitizeField(n.Description, 200), Project: n.Project,
		Age: humanAge(n.Updated), Favorite: n.Favorite, Updated: n.Updated,
	}
}

// taskHit projects a task. A task has no one-line description field, so the
// body's opening stands in -- the same role Memory.Description plays, sourced
// from the only prose a task has. Name carries the plan slug (empty for a
// standalone task), which is how a hit announces that it is a step of a
// composition rather than a loose queue item.
func taskHit(t core.Task) Hit {
	return Hit{
		Kind: core.ItemKindTask, ID: t.ID, Name: t.PlanSlug, Title: t.Title,
		Description: sanitizeField(t.Body, 200), Project: t.ProjectSlug,
		Age: humanAge(t.UpdatedAt), Status: string(t.Status),
		Favorite: t.Favorite, Updated: t.UpdatedAt,
	}
}

// trialHit projects a trial. Name is the lab, which is the handle trial_query
// takes; the description leads with what actually happened, since a trial's
// value to a later agent is its Actual, not its plan.
func trialHit(tr core.Trial) Hit {
	return Hit{
		Kind: core.ItemKindTrial, ID: tr.ID, Name: tr.Lab, Title: tr.Title,
		Description: sanitizeField(tr.Actual, 200), Project: tr.ProjectSlug,
		Age: humanAge(tr.CreatedAt), Status: string(tr.Outcome),
		Favorite: tr.Favorite, Updated: tr.CreatedAt,
	}
}

// sessionHit projects a session's findings. Unlike every other kind there is no
// second read to follow up with -- no session_read tool exists -- so the
// description IS the payload, and it is the findings rather than a label.
func sessionHit(s core.Session) Hit {
	return Hit{
		Kind: core.ItemKindSession, ID: s.ID, Name: s.Name, Title: s.Name,
		Description: sanitizeField(s.Findings, 200), Project: s.ProjectSlug,
		Age: humanAge(s.UpdatedAt), Status: string(s.Status), Updated: s.UpdatedAt,
	}
}

// scopeVisible reports whether an item in hitProject is visible to a session
// bound to scope: global items (project "") are visible when the scope may
// read the global scope at all -- a sealed project may not, and its callers
// pass globalVisible false; otherwise the projects must match.
func scopeVisible(hitProject, scope string, globalVisible bool) bool {
	if hitProject == "" {
		return globalVisible
	}
	return hitProject == scope
}

// scopeKinds maps a scope to the KNOWLEDGE item kinds. It is Search's mapping:
// the console has its own task/trial/session/plan/project sections built from
// the structured tables, so widening the fused leg there would double-report
// those entities in two differently-ranked places on one page.
//
// The permissive default is the library posture AGENTS.md allows -- the zero
// value legitimately means "unset" -- and the boundaries (console searchScopes,
// MCP enumOf, seam's --scope) are where a present-but-wrong value is refused.
func scopeKinds(scope string) []string {
	switch scope {
	case "memories":
		return []string{core.ItemKindMemory}
	case "notes":
		return []string{core.ItemKindNote}
	default:
		return core.KnowledgeItemKinds
	}
}

// recallScopeKinds maps a scope to item kinds for the agent-facing Recall,
// where "all" spans the work record too. Recall has no sibling sections to
// defer to: it is the one search an agent gets, so leaving the work record out
// of the default would mean an agent finds a task only by already suspecting
// there is a task to find.
func recallScopeKinds(scope string) []string {
	switch scope {
	case "memories":
		return []string{core.ItemKindMemory}
	case "notes":
		return []string{core.ItemKindNote}
	case "tasks":
		return []string{core.ItemKindTask}
	case "trials":
		return []string{core.ItemKindTrial}
	case "sessions":
		return []string{core.ItemKindSession}
	default:
		return append(append([]string{}, core.KnowledgeItemKinds...), core.WorkItemKinds...)
	}
}
