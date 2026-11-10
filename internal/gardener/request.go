package gardener

// Natural-language maintenance requests. The owner describes an organization
// problem ("these two are duplicates", "archive anything about the old port")
// and the gardener interprets it into PENDING proposals for review. Like every
// other pass it only ever writes gardener_proposals rows -- it never mutates a
// memory. Interpretation reuses the existing merge/archive apply paths verbatim.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// Sentinel errors for Request, checked with errors.Is.
var (
	ErrNoChat       = errors.New("gardener: natural-language requests need an LLM chat client")
	ErrEmptyRequest = errors.New("gardener: empty request")
	ErrUnparseable  = errors.New("gardener: could not parse the interpreter's output")
)

const (
	// requestTimeout bounds one interpretation. It is tighter than the llm
	// chatTimeout (60s) so a slow provider fails the console POST promptly rather
	// than hanging the request.
	requestTimeout = 45 * time.Second
	// maxRequestCandidates caps how many active memories are shown to the model,
	// bounding prompt size. A personal store holds tens per scope; this is a guard
	// against a pathologically large corpus, not an expected limit.
	maxRequestCandidates = 150
)

// CanRequest reports whether natural-language requests are available (a chat
// client is configured). The console and MCP gate their UI on it without needing
// to see the private chat field.
func (s *Service) CanRequest() bool { return s.chat != nil }

// RequestResult summarizes one interpretation pass.
type RequestResult struct {
	Created []string       `json:"created"` // proposal ids, in creation order
	ByKind  map[string]int `json:"byKind"`  // e.g. {"merge":2,"archive":1}
	Total   int            `json:"total"`
	Skipped []string       `json:"skipped"` // human-readable notes on dropped ops
	Summary string         `json:"summary"` // one-line summary

	// Guidance is a human/agent-readable note on how to proceed when the request
	// is better served by another tool -- most importantly when it recognizes a
	// project split and points the caller at gardener_split. Empty when the
	// request was handled inline.
	Guidance string `json:"guidance,omitempty"`
	// SplitIntent is true when the request was recognized as a project split,
	// whether or not a source project was identified. It lets a caller offer its
	// own follow-up (the console shows a project picker) without parsing Guidance.
	SplitIntent bool `json:"splitIntent,omitempty"`
	// SplitSource is set to the source project slug when the request was
	// recognized as a project split that should be planned with gardener_split.
	// The general request never plans a split itself (it needs a whole-project
	// classification and a known source); it only routes to it.
	SplitSource string `json:"splitSource,omitempty"`
}

// RequestScope selects the active memories an interpretation may reference.
//
// It is a struct rather than a scope string because that string was overloaded:
// "" used to mean "every project on the machine", the inverse of every other
// scope in the codebase, where an empty project slug IS the global scope. That
// inversion made the two callers unable to route their project argument through
// the shared scope guards -- normalizeProject maps "global" to "", which would
// have turned a globals-only request into a whole-machine scan. Splitting the
// meanings apart is what lets the boundary own its tokens and this package own
// none.
type RequestScope struct {
	// Project scopes to one project's memories plus the globals. The zero value
	// -- "" -- is globals only, matching store.ActiveMemories.
	Project string
	// AllProjects widens to every project, ignoring Project. It is only ever set
	// from an explicit caller intent (gardener_request's project="all", the
	// console's "All projects" option), never as a default: an unscoped request
	// that silently read every project is what the input-boundary work exists to
	// remove.
	AllProjects bool
}

// callerScope is the project scope an interpretation reads and writes as, for
// the isolation fences. A whole-machine pass is unbound, so it runs as the
// global scope: it sees everything except what a fenced project keeps to itself.
func (sc RequestScope) callerScope() string {
	if sc.AllProjects {
		return ""
	}
	return sc.Project
}

// Request interprets a single natural-language maintenance request against the
// active memories in scope and creates PENDING proposals for review. It never
// mutates a memory.
func (s *Service) Request(ctx context.Context, text string, scope RequestScope) (RequestResult, error) {
	if s.chat == nil {
		return RequestResult{}, ErrNoChat
	}
	text = strings.TrimSpace(text)
	if text == "" {
		return RequestResult{}, ErrEmptyRequest
	}

	candidates, err := s.requestCandidates(ctx, scope)
	if err != nil {
		return RequestResult{}, fmt.Errorf("gardener.Request: %w", err)
	}
	if len(candidates) == 0 {
		return RequestResult{ByKind: map[string]int{}, Summary: "no active memories in scope"}, nil
	}

	// The set of real projects a memory may be moved into (reproject) or that a
	// split may target: registered projects plus any project a candidate memory
	// already lives in. Moving into a project not in this set would be creating a
	// new project -- which is a split, not a reproject.
	projects, err := store.ListProjects(ctx, s.db)
	if err != nil {
		return RequestResult{}, fmt.Errorf("gardener.Request: %w", err)
	}
	// Isolation fence on the move destinations: the prompt's project list and the
	// reproject targets are what this pass may write INTO, so they go through the
	// write side of the matrix -- a sealed project admits nothing from outside,
	// and a pass running as a fenced project writes nowhere but itself.
	// routeSplit below keeps the unfiltered set on purpose: a fenced slug is
	// still a real project, and Split refuses it by name rather than pretending
	// it does not exist.
	targets, err := s.writableProjects(ctx, scope, projects)
	if err != nil {
		return RequestResult{}, fmt.Errorf("gardener.Request: %w", err)
	}
	known := knownProjects(targets, candidates)

	cctx, cancel := context.WithTimeout(ctx, requestTimeout)
	defer cancel()
	out, err := s.chat.Complete(cctx, requestSystemPrompt, requestUserPrompt(text, candidates, targets))
	if err != nil {
		return RequestResult{}, fmt.Errorf("gardener.Request: %w", err)
	}

	plan, err := parseRequestPlan(out)
	if err != nil {
		return RequestResult{}, err // already wraps ErrUnparseable
	}

	// A split creates NEW child projects and a shared parent -- it needs a
	// whole-project classification and a known source, so the general request
	// never plans it inline. It recognizes the intent and routes to gardener_split.
	if plan.Split != nil {
		return s.routeSplit(ctx, text, plan.Split, knownProjects(projects, candidates)), nil
	}

	// Seed the dedup set from every existing proposal key (any status) so a
	// request never re-raises something already proposed, applied, or dismissed.
	seen, err := loadSeen(ctx, s.db)
	if err != nil {
		return RequestResult{}, fmt.Errorf("gardener.Request: %w", err)
	}

	res := RequestResult{ByKind: map[string]int{}}
	for _, op := range plan.Ops {
		kind, key, payload, skip := mapRequestOp(op, candidates, text, known)
		if skip != "" {
			res.Skipped = append(res.Skipped, skip)
			continue
		}
		if kind == store.ProposalConsolidate {
			// Consolidate is the one op whose body the LLM authored; record the
			// producer at generation time for the apply step to stamp.
			payload["model"] = s.chat.Model()
		}
		if seen.blocked(key) {
			res.Skipped = append(res.Skipped, kind+" is already proposed")
			continue
		}
		id, err := s.createProposal(ctx, kind, key, payload, seen)
		if err != nil {
			return res, fmt.Errorf("gardener.Request: %w", err)
		}
		res.Created = append(res.Created, id)
		res.ByKind[kind]++
		res.Total++
	}
	res.Summary = requestSummary(res)

	s.record(ctx, "", map[string]any{
		"action": "request", "text": truncateRunes(text, 200),
		"created": res.Total, "merges": res.ByKind[store.ProposalMerge],
		"archives": res.ByKind[store.ProposalArchive], "consolidations": res.ByKind[store.ProposalConsolidate],
		"reprojects": res.ByKind[store.ProposalReproject], "rekinds": res.ByKind[store.ProposalRekind],
	})
	return res, nil
}

// knownProjects is the set of project slugs a request may move a memory into: every
// registered project plus every project a candidate memory already lives in.
func knownProjects(projects []core.Project, candidates []core.Memory) map[string]bool {
	known := make(map[string]bool, len(projects)+len(candidates))
	for _, p := range projects {
		known[p.Slug] = true
	}
	for _, m := range candidates {
		if m.Project != "" {
			known[m.Project] = true
		}
	}
	return known
}

// routeSplit turns a recognized split intent into guidance pointing at
// gardener_split. It never plans the split itself: splitting needs a whole-project
// classification and a structured source, so the general request only recognizes
// the intent and hands the caller the exact way to run it.
func (s *Service) routeSplit(ctx context.Context, text string, sp *reqSplit, known map[string]bool) RequestResult {
	res := RequestResult{ByKind: map[string]int{}, SplitIntent: true}
	src := core.Slugify(strings.TrimSpace(sp.Source))
	switch {
	case src == "":
		res.Guidance = "This reads like a project split, but no source project was identified. Run gardener_split with the exact slug of the project to split (see project_list)."
	case !known[src]:
		res.Guidance = fmt.Sprintf("This reads like a request to split %q, which is not a known project. Run gardener_split with the exact source slug (see project_list).", src)
	default:
		res.SplitSource = src
		res.Guidance = fmt.Sprintf("Recognized a request to split %q into new child projects. Plan it with gardener_split (source=%s) -- it classifies %s's memories into the children and a shared parent as reviewable proposals.", src, src, src)
	}
	res.Summary = res.Guidance
	s.record(ctx, "", map[string]any{
		"action": "request", "text": truncateRunes(text, 200),
		"recognized": "split", "source": res.SplitSource,
	})
	return res
}

// requestCandidates loads the active memories the interpreter may reference,
// scoped and capped.
//
// The "global" special-case this used to carry is gone: the caller normalizes
// its own tokens, so an empty Project means globals here exactly as it does in
// store.ActiveMemories, and nothing in this package knows what "global" spells.
func (s *Service) requestCandidates(ctx context.Context, scope RequestScope) ([]core.Memory, error) {
	var (
		mems []core.Memory
		err  error
	)
	if scope.AllProjects {
		mems, err = store.AllActiveMemories(ctx, s.db)
	} else {
		mems, err = store.ActiveMemories(ctx, s.db, scope.Project)
	}
	if err != nil {
		return nil, err
	}
	// Isolation fence on the scan: keep only what this pass's scope may read. A
	// whole-machine pass runs unbound, so every fenced project drops out of it;
	// a pass scoped to a sealed project drops the globals ActiveMemories folds
	// in. Filtering before the cap stops fenced memories from eating the
	// candidate budget.
	gate := newScopeGate(s.db, scope.callerScope())
	kept := make([]core.Memory, 0, len(mems))
	for _, m := range mems {
		ok, err := gate.readable(ctx, m.Project)
		if err != nil {
			return nil, err
		}
		if ok {
			kept = append(kept, m)
		}
	}
	if len(kept) > maxRequestCandidates {
		kept = kept[:maxRequestCandidates]
	}
	return kept, nil
}

// writableProjects narrows the registered projects to the ones this request may
// propose moving a memory into. It is the destination half of the isolation
// fence (requestCandidates is the source half).
func (s *Service) writableProjects(ctx context.Context, scope RequestScope, projects []core.Project) ([]core.Project, error) {
	gate := newScopeGate(s.db, scope.callerScope())
	out := make([]core.Project, 0, len(projects))
	for _, p := range projects {
		ok, err := gate.writable(ctx, p.Slug)
		if err != nil {
			return nil, err
		}
		if ok {
			out = append(out, p)
		}
	}
	return out, nil
}

// requestKindList renders core.MemoryKinds for the interpreter prompt, so the
// prompt's kind vocabulary follows the enum instead of a hand-written copy.
func requestKindList() string {
	parts := make([]string, len(core.MemoryKinds))
	for i, k := range core.MemoryKinds {
		parts[i] = string(k)
	}
	return strings.Join(parts, ", ")
}

var requestSystemPrompt = `You convert a single natural-language request to REORGANIZE an AI agent's memory store into a set of PROPOSED, reviewable operations. You never modify anything yourself; a human reviews and applies each operation. Reference memories only by their candidate number.

Operations (return these in "ops"):
- merge: fold one memory into another EXISTING memory. {"op":"merge","keep":<n>,"drop":<m>}. keep is retained as-is; drop is superseded by keep (still readable, pointing at keep). Use this when one existing memory should simply absorb another.
- archive: retire a memory that is no longer relevant. {"op":"archive","target":<n>,"reason":"<short why>"}. The memory is marked invalid but stays readable.
- consolidate: replace several redundant memories with ONE new unified memory that you write. {"op":"consolidate","name":"<short-kebab-name>","kind":"<kind>","description":"<one line, <=150 chars>","body":"<full markdown body>","sources":[<n>,<m>,...]}. Every named source is superseded by the new memory. Use this (not merge) when the truth is spread across several memories and a fresh combined write is clearer than keeping any single one.
- reproject: move a memory to a DIFFERENT, ALREADY-EXISTING project. {"op":"reproject","target":<n>,"to":"<existing-project-slug>","reason":"<short why>"}. "to" MUST be one of the existing project slugs listed below. Use this when a memory is filed under the wrong project. Do NOT use reproject to move a memory into a project that does not exist yet -- that is a split.
- rekind: change a memory's KIND classification in place. {"op":"rekind","target":<n>,"kind":"<kind>","reason":"<short why>"}. The memory keeps its identity, project, and body; only its kind changes. The most common correction: constraint = what any agent must or must not do regardless of task; convention = a project-local choice or layout fact (naming, branding, where things live or deploy, file sync sets).

Splitting a project (special -- NOT an op):
- If the request is to SPLIT one project into NEW child projects (e.g. "split example-app into example-ios and example-android"), do NOT emit ops. Instead return {"split":{"source":"<project-slug-to-split>","instruction":"<the request text>"}}. Splitting creates new projects and a shared parent and is planned by a separate pass; here you only identify the single project being split.

Rules:
- Reference memories ONLY by their [N] candidate number. Never invent numbers.
- For a merge, keep and drop must be different memories.
- For consolidate and rekind, kind is one of: ` + requestKindList() + `. For a consolidate, name is a short kebab-case identifier; write a genuine unified body from the sources and do not invent facts beyond them.
- For reproject, "to" must be one of the existing project slugs. Creating a new project is a split, not a reproject.
- Propose only what the request asks for. If nothing applies, return {"ops":[]}.
- Output ONLY a JSON object -- either {"split":{...}} OR {"ops":[...]} -- with no prose and no markdown code fences.`

// requestUserPrompt renders the request, the existing project slugs (so moves and
// splits reference real projects), and a numbered candidate list. The model
// references memories by their [N] index, resolved back to ids server-side.
func requestUserPrompt(text string, mems []core.Memory, projects []core.Project) string {
	var b strings.Builder
	b.WriteString("Request:\n")
	b.WriteString(text)
	if len(projects) > 0 {
		b.WriteString("\n\nExisting projects (use these exact slugs for a reproject; any other name would be a new project = a split):\n")
		for _, p := range projects {
			fmt.Fprintf(&b, "- %s\n", p.Slug)
		}
	}
	b.WriteString("\n\nCandidate memories (reference by [N]):\n")
	for i, m := range mems {
		fmt.Fprintf(&b, "[%d] %s (%s, %s) -- %s\n", i+1, m.Name, projectLabel(m.Project), m.Kind, m.Description)
	}
	return b.String()
}

func projectLabel(project string) string {
	if project == "" {
		return "global"
	}
	return project
}

// reqOp is one operation the interpreter emits. Candidate references are 1-based
// indices into the candidate list, so 0 is a clean "absent" sentinel.
type reqOp struct {
	Op     string `json:"op"`     // "merge" | "archive" | "consolidate" | "reproject" | "rekind"
	Keep   int    `json:"keep"`   // merge: candidate to retain
	Drop   int    `json:"drop"`   // merge: candidate to fold into keep
	Target int    `json:"target"` // archive/reproject/rekind: candidate to retire/move/reclassify
	To     string `json:"to"`     // reproject: destination project slug
	Reason string `json:"reason"` // archive/reproject/rekind rationale

	// consolidate: a new unified memory written from several sources.
	// Kind doubles as a rekind's target kind.
	Name        string `json:"name"`
	Kind        string `json:"kind"`
	Description string `json:"description"`
	Body        string `json:"body"`
	Sources     []int  `json:"sources"`
}

// reqSplit is the top-level split directive: the interpreter returns it (instead
// of ops) when the request is to split one project into new children. The general
// request only routes it to gardener_split; it never plans the split itself.
type reqSplit struct {
	Source      string `json:"source"`      // project slug to split
	Instruction string `json:"instruction"` // the split ask, forwarded to gardener_split
}

type reqPlan struct {
	Split *reqSplit `json:"split"` // set => a recognized split; ops is ignored
	Ops   []reqOp   `json:"ops"`
}

// parseRequestPlan extracts the {"ops":[...]} object from a completion, tolerating
// a code fence or surrounding prose. It never fabricates: a body that will not
// unmarshal yields ErrUnparseable so the caller creates nothing.
func parseRequestPlan(raw string) (reqPlan, error) {
	s := stripCodeFence(strings.TrimSpace(raw))
	if !strings.HasPrefix(s, "{") {
		if i, j := strings.IndexByte(s, '{'), strings.LastIndexByte(s, '}'); i >= 0 && j > i {
			s = s[i : j+1]
		}
	}
	var plan reqPlan
	if err := json.Unmarshal([]byte(s), &plan); err != nil {
		return reqPlan{}, fmt.Errorf("%w: %w", ErrUnparseable, err)
	}
	return plan, nil
}

// stripCodeFence removes a leading ```/```json fence line and a trailing ``` if
// the model wrapped its JSON in a Markdown code block.
func stripCodeFence(s string) string {
	if !strings.HasPrefix(s, "```") {
		return s
	}
	if _, rest, ok := strings.Cut(s, "\n"); ok {
		s = rest
	}
	s = strings.TrimSpace(s)
	s = strings.TrimSuffix(s, "```")
	return strings.TrimSpace(s)
}

// mapRequestOp validates one op against the candidate list and produces the
// proposal (kind, dedup key, payload). A non-empty skip note means the op was
// invalid and is dropped rather than fabricated. Keys match the gardener's own
// passes (mergeKey / archive:<id>) so a request shares dedup state with them.
func mapRequestOp(op reqOp, candidates []core.Memory, text string, known map[string]bool) (kind, key string, payload map[string]any, skip string) {
	switch op.Op {
	case store.ProposalReproject:
		m, ok := candidateAt(candidates, op.Target)
		if !ok {
			return "", "", nil, fmt.Sprintf("reproject references memory #%d, which is not in the candidate list", op.Target)
		}
		to := core.Slugify(strings.TrimSpace(op.To))
		if to == "" {
			return "", "", nil, fmt.Sprintf("reproject of %s is missing a destination project", m.Name)
		}
		if !known[to] {
			return "", "", nil, fmt.Sprintf("reproject target %q is not an existing project -- create it first, or split the source project", to)
		}
		if m.Project == to {
			return "", "", nil, fmt.Sprintf("%s is already in %s", m.Name, projectLabel(to))
		}
		return store.ProposalReproject, "reproject:" + m.ID, map[string]any{
			"id": m.ID, "name": m.Name, "from": m.Project, "to": to,
			"rationale": strings.TrimSpace(op.Reason), "source": "request", "request_text": text,
		}, ""
	case store.ProposalRekind:
		m, ok := candidateAt(candidates, op.Target)
		if !ok {
			return "", "", nil, fmt.Sprintf("rekind references memory #%d, which is not in the candidate list", op.Target)
		}
		to := core.MemoryKind(strings.TrimSpace(op.Kind))
		if !slices.Contains(core.MemoryKinds, to) {
			// Unlike a consolidate there is no safe default: the whole point of a
			// rekind is the specific classification, so an unknown kind drops the op.
			return "", "", nil, fmt.Sprintf("rekind of %s names unknown kind %q", m.Name, op.Kind)
		}
		if m.Kind == to {
			return "", "", nil, fmt.Sprintf("%s is already a %s", m.Name, to)
		}
		return store.ProposalRekind, "rekind:" + m.ID + ":" + string(to), map[string]any{
			"id": m.ID, "name": m.Name, "project": m.Project,
			"from": string(m.Kind), "to": string(to),
			"rationale": strings.TrimSpace(op.Reason), "source": "request", "request_text": text,
		}, ""
	case store.ProposalMerge:
		keep, ok := candidateAt(candidates, op.Keep)
		if !ok {
			return "", "", nil, fmt.Sprintf("merge references memory #%d, which is not in the candidate list", op.Keep)
		}
		drop, ok := candidateAt(candidates, op.Drop)
		if !ok {
			return "", "", nil, fmt.Sprintf("merge references memory #%d, which is not in the candidate list", op.Drop)
		}
		if keep.ID == drop.ID {
			return "", "", nil, "merge keep and drop are the same memory"
		}
		return store.ProposalMerge, mergeKey(keep.ID, drop.ID), map[string]any{
			"keep":         requestBrief(keep),
			"drop":         requestBrief(drop),
			"source":       "request",
			"request_text": text,
		}, ""
	case store.ProposalArchive:
		target, ok := candidateAt(candidates, op.Target)
		if !ok {
			return "", "", nil, fmt.Sprintf("archive references memory #%d, which is not in the candidate list", op.Target)
		}
		reason := strings.TrimSpace(op.Reason)
		if reason == "" {
			reason = "requested"
		}
		return store.ProposalArchive, "archive:" + target.ID, map[string]any{
			"id": target.ID, "name": target.Name, "project": target.Project,
			"kind": string(target.Kind), "description": target.Description,
			"reason": reason, "source": "request", "request_text": text,
		}, ""
	case store.ProposalConsolidate:
		name := strings.TrimSpace(op.Name)
		body := strings.TrimSpace(op.Body)
		if name == "" || body == "" {
			return "", "", nil, "consolidate op is missing a name or body"
		}
		var (
			srcObjs []map[string]any
			srcIDs  []string
			project string
		)
		for _, n := range op.Sources {
			m, ok := candidateAt(candidates, n)
			if !ok {
				continue // skip an out-of-range source reference
			}
			if project == "" {
				project = m.Project // the unified memory lives with its first source
			}
			srcObjs = append(srcObjs, map[string]any{"id": m.ID, "name": m.Name})
			srcIDs = append(srcIDs, m.ID)
		}
		if len(srcIDs) == 0 {
			return "", "", nil, "consolidate op references no valid source memories"
		}
		slices.Sort(srcIDs) // canonical key regardless of source order
		return store.ProposalConsolidate, "consolidate:" + strings.Join(srcIDs, "|"), map[string]any{
			"name": name, "kind": normalizeKind(op.Kind), "project": project,
			"description": strings.TrimSpace(op.Description), "body": body,
			"sources": srcObjs, "source": "request", "request_text": text,
		}, ""
	default:
		return "", "", nil, fmt.Sprintf("unknown operation %q", op.Op)
	}
}

// normalizeKind validates a memory kind for a consolidated memory, defaulting to
// reference when the model supplies an unknown or empty kind.
func normalizeKind(kind string) string {
	k := core.MemoryKind(strings.TrimSpace(kind))
	if slices.Contains(core.MemoryKinds, k) {
		return string(k)
	}
	return string(core.KindReference)
}

// candidateAt resolves a 1-based reference to a candidate memory. ok is false for
// an out-of-range index (including the 0 sentinel).
func candidateAt(candidates []core.Memory, n int) (core.Memory, bool) {
	if n < 1 || n > len(candidates) {
		return core.Memory{}, false
	}
	return candidates[n-1], true
}

// requestBrief is the compact memory descriptor embedded in a merge payload,
// matching the shape the apply path and console card read (see memoryBrief).
func requestBrief(m core.Memory) map[string]any {
	return map[string]any{
		"id": m.ID, "name": m.Name, "project": m.Project,
		"description": m.Description, "kind": string(m.Kind),
	}
}

func requestSummary(res RequestResult) string {
	if res.Total == 0 {
		return "no proposals matched"
	}
	var parts []string
	if n := res.ByKind[store.ProposalMerge]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d merge", n))
	}
	if n := res.ByKind[store.ProposalArchive]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d archive", n))
	}
	if n := res.ByKind[store.ProposalConsolidate]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d consolidate", n))
	}
	if n := res.ByKind[store.ProposalReproject]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d move", n))
	}
	if n := res.ByKind[store.ProposalRekind]; n > 0 {
		parts = append(parts, fmt.Sprintf("%d rekind", n))
	}
	return "created " + strings.Join(parts, ", ") + " proposal(s)"
}

// truncateRunes bounds a string to n runes (for event payloads), never splitting
// a multi-byte rune.
func truncateRunes(s string, n int) string {
	r := []rune(s)
	if len(r) <= n {
		return s
	}
	return string(r[:n])
}
