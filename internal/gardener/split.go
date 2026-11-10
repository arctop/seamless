package gardener

// Project split: the flagship natural-language topology change. The owner asks to
// split one project (e.g. example-app) into children (example-ios, example-android)
// with cross-platform memories kept in a shared parent (example-mobile-apps). Like
// every other pass this only ever writes PENDING proposals -- it never creates a
// project or moves a memory itself. It emits one `split` setup proposal (create
// the child/shared projects, link them as a family, parent the children, retire
// the source) plus one `reproject` proposal per memory, all tagged with the same
// plan slug so the console reviews them as a retargetable, resumable batch.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// ErrNoSource is returned by Split when no source project is given.
var ErrNoSource = errors.New("gardener: split needs a source project")

// ErrIsolatedProject is returned by Split when the source project is isolated.
// A split relocates the source's memories into new projects and leaves it with
// children, and isolation requires a standalone project -- so the fence has to
// come down before the split, not after.
var ErrIsolatedProject = errors.New("gardener: project is isolated")

// splitTimeout bounds one split interpretation. Classifying a whole project's
// memories is a larger prompt than a single request, so it gets the full chat
// budget rather than the tighter request timeout.
const splitTimeout = 60 * time.Second

// Split interprets a natural-language request to split source into child projects
// (with cross-platform memories kept in a shared parent) and creates the PENDING
// proposals for review: one `split` setup proposal plus one `reproject` per
// memory, all under plan "split-<source>". It never creates a project or moves a
// memory -- the owner applies each proposal. instruction is the free-text ask
// (which children, what stays shared); an empty instruction still works (the model
// infers reasonable children from the memories), but a source project is required.
func (s *Service) Split(ctx context.Context, source, instruction string) (RequestResult, error) {
	if s.chat == nil {
		return RequestResult{}, ErrNoChat
	}
	source = strings.TrimSpace(source)
	if source == "" {
		return RequestResult{}, ErrNoSource
	}
	instruction = strings.TrimSpace(instruction)

	// An isolated source is excluded from the scan entirely: every assignment
	// this pass could produce moves one of its memories out of the fence, and the
	// setup proposal would hand it a family and children.
	iso, err := store.IsolationOf(ctx, s.db, source)
	if err != nil {
		return RequestResult{}, fmt.Errorf("gardener.Split: %w", err)
	}
	if iso.FencesOutbound() {
		return RequestResult{}, fmt.Errorf("gardener.Split: %w: %s is %s -- set it back to open to split it",
			ErrIsolatedProject, source, iso)
	}

	candidates, err := s.splitCandidates(ctx, source)
	if err != nil {
		return RequestResult{}, fmt.Errorf("gardener.Split: %w", err)
	}
	if len(candidates) == 0 {
		return RequestResult{ByKind: map[string]int{}, Summary: "no memories in " + source + " to split"}, nil
	}

	cctx, cancel := context.WithTimeout(ctx, splitTimeout)
	defer cancel()
	out, err := s.chat.Complete(cctx, splitSystemPrompt, splitUserPrompt(source, instruction, candidates))
	if err != nil {
		return RequestResult{}, fmt.Errorf("gardener.Split: %w", err)
	}

	plan, err := parseSplitPlan(out)
	if err != nil {
		return RequestResult{}, err // wraps ErrUnparseable
	}
	children, shared, targets, err := validateSplitTargets(plan, source)
	if err != nil {
		return RequestResult{}, err
	}

	seen, err := loadSeen(ctx, s.db)
	if err != nil {
		return RequestResult{}, fmt.Errorf("gardener.Split: %w", err)
	}
	planSlug := "split-" + source
	res := RequestResult{ByKind: map[string]int{}}

	// The setup proposal comes first so the plan group can apply it before the
	// per-memory reprojects (though apply order is not enforced -- each is idempotent).
	setupKey := "split:" + source
	if !seen.blocked(setupKey) {
		sharedObj := map[string]any{}
		if shared.slug != "" {
			sharedObj = map[string]any{"slug": shared.slug, "label": shared.label}
		}
		childObjs := make([]any, 0, len(children))
		for _, c := range children {
			childObjs = append(childObjs, map[string]any{"slug": c.slug, "label": c.label})
		}
		id, cerr := s.createProposal(ctx, store.ProposalSplit, setupKey, map[string]any{
			"source_project": source, "children": childObjs, "shared": sharedObj,
			"family": shared.slug, "retire_source": true, "plan": planSlug,
			"source": "request", "request_text": instruction,
		}, seen)
		if cerr != nil {
			return res, fmt.Errorf("gardener.Split: %w", cerr)
		}
		res.Created = append(res.Created, id)
		res.ByKind[store.ProposalSplit]++
		res.Total++
	} else {
		res.Skipped = append(res.Skipped, "split setup is already proposed")
	}

	// One reproject proposal per assignment: memory [N] -> target slug.
	for _, a := range plan.Assignments {
		mem, ok := candidateAt(candidates, a.Memory)
		if !ok {
			res.Skipped = append(res.Skipped, fmt.Sprintf("assignment references memory #%d, not in the candidate list", a.Memory))
			continue
		}
		to := core.Slugify(strings.TrimSpace(a.To))
		if !targets[to] {
			res.Skipped = append(res.Skipped, fmt.Sprintf("%s -> %q is not one of the split targets", mem.Name, a.To))
			continue
		}
		if to == source {
			res.Skipped = append(res.Skipped, mem.Name+" is already in the source project")
			continue
		}
		key := "reproject:" + mem.ID
		if seen.blocked(key) {
			res.Skipped = append(res.Skipped, mem.Name+" is already proposed for a move")
			continue
		}
		id, rerr := s.createProposal(ctx, store.ProposalReproject, key, map[string]any{
			"id": mem.ID, "name": mem.Name, "from": source, "to": to,
			"rationale": strings.TrimSpace(a.Rationale), "plan": planSlug,
			"source": "request", "request_text": instruction,
		}, seen)
		if rerr != nil {
			return res, fmt.Errorf("gardener.Split: %w", rerr)
		}
		res.Created = append(res.Created, id)
		res.ByKind[store.ProposalReproject]++
		res.Total++
	}

	res.Summary = splitSummary(res, source)
	s.record(ctx, "", map[string]any{
		"action": "split", "source": source, "plan": planSlug,
		"created": res.Total, "reprojects": res.ByKind[store.ProposalReproject],
	})
	return res, nil
}

// splitCandidates loads the active memories that belong to source itself (not
// globals -- a split only relocates the project's own memories), capped.
func (s *Service) splitCandidates(ctx context.Context, source string) ([]core.Memory, error) {
	mems, err := store.ActiveMemories(ctx, s.db, source)
	if err != nil {
		return nil, err
	}
	own := make([]core.Memory, 0, len(mems))
	for _, m := range mems {
		if m.Project == source { // drop globals that ActiveMemories folds in
			own = append(own, m)
		}
	}
	if len(own) > maxRequestCandidates {
		own = own[:maxRequestCandidates]
	}
	return own, nil
}

const splitSystemPrompt = `You plan how to split ONE project's memory store into child projects, keeping cross-platform knowledge in a shared parent. You never create anything yourself; a human reviews and applies each proposed step.

Return ONLY a JSON object of this exact shape (no prose, no markdown fences):
{
  "children": [{"slug":"<kebab-slug>","label":"<human label>"}, ...],
  "shared":   {"slug":"<kebab-slug>","label":"<human label>"},
  "assignments": [{"memory":<n>,"to":"<slug>","rationale":"<short why>"}, ...]
}

Rules:
- children: the 2+ projects the source splits into (e.g. example-ios, example-android). Use kebab-case slugs.
- shared: ONE parent project for memories that apply to every child (cross-platform). Omit it (use {}) only if nothing is shared.
- assignments: for EACH candidate memory, choose exactly one target -- a child slug, or the shared slug for cross-platform memories. Reference memories by their [N] number only; never invent numbers. "to" must be one of the child slugs or the shared slug.
- Put a memory in the shared parent when it is platform-agnostic; put it in a child when it is specific to that platform.
- Output ONLY the JSON object.`

// splitUserPrompt renders the source project, the instruction, and the numbered
// candidate memories.
func splitUserPrompt(source, instruction string, mems []core.Memory) string {
	var b strings.Builder
	fmt.Fprintf(&b, "Split project: %s\n", source)
	if instruction != "" {
		b.WriteString("Instruction: ")
		b.WriteString(instruction)
		b.WriteString("\n")
	}
	b.WriteString("\nMemories to classify (reference by [N]):\n")
	for i, m := range mems {
		fmt.Fprintf(&b, "[%d] %s (%s) -- %s\n", i+1, m.Name, m.Kind, m.Description)
	}
	return b.String()
}

// splitTarget is one project the split creates (a child or the shared parent).
type splitTarget struct{ slug, label string }

// splitAssignment maps a candidate memory to a target project slug.
type splitAssignment struct {
	Memory    int    `json:"memory"`
	To        string `json:"to"`
	Rationale string `json:"rationale"`
}

type splitPlan struct {
	Children    []splitTarget     `json:"-"`
	Shared      splitTarget       `json:"-"`
	Assignments []splitAssignment `json:"assignments"`
}

// splitPlanJSON mirrors the model's JSON (slug/label objects); parseSplitPlan
// lifts it into splitPlan.
type splitPlanJSON struct {
	Children []struct {
		Slug  string `json:"slug"`
		Label string `json:"label"`
	} `json:"children"`
	Shared struct {
		Slug  string `json:"slug"`
		Label string `json:"label"`
	} `json:"shared"`
	Assignments []splitAssignment `json:"assignments"`
}

// parseSplitPlan extracts the split JSON from a completion, tolerating a code
// fence or surrounding prose. A body that will not unmarshal yields ErrUnparseable
// so the caller creates nothing.
func parseSplitPlan(raw string) (splitPlan, error) {
	str := stripCodeFence(strings.TrimSpace(raw))
	if !strings.HasPrefix(str, "{") {
		if i, j := strings.IndexByte(str, '{'), strings.LastIndexByte(str, '}'); i >= 0 && j > i {
			str = str[i : j+1]
		}
	}
	var j splitPlanJSON
	if err := json.Unmarshal([]byte(str), &j); err != nil {
		return splitPlan{}, fmt.Errorf("%w: %w", ErrUnparseable, err)
	}
	plan := splitPlan{Shared: splitTarget{slug: j.Shared.Slug, label: j.Shared.Label}, Assignments: j.Assignments}
	for _, c := range j.Children {
		plan.Children = append(plan.Children, splitTarget{slug: c.Slug, label: c.Label})
	}
	return plan, nil
}

// validateSplitTargets normalizes and validates the split's child/shared projects
// and returns the cleaned children, the shared parent, and the set of valid target
// slugs (children + shared). It errors if there are not at least two distinct
// children, or if any target slugifies to the source (a memory cannot be split
// back into the project being emptied).
func validateSplitTargets(plan splitPlan, source string) ([]splitTarget, splitTarget, map[string]bool, error) {
	targets := map[string]bool{}
	children := make([]splitTarget, 0, len(plan.Children))
	for _, c := range plan.Children {
		slug := core.Slugify(strings.TrimSpace(c.slug))
		if slug == "" || targets[slug] {
			continue
		}
		if slug == source {
			return nil, splitTarget{}, nil, fmt.Errorf("gardener.Split: child project %q is the source project", slug)
		}
		label := strings.TrimSpace(c.label)
		if label == "" {
			label = slug
		}
		children = append(children, splitTarget{slug: slug, label: label})
		targets[slug] = true
	}
	if len(children) < 2 {
		return nil, splitTarget{}, nil, fmt.Errorf("gardener.Split: a split needs at least two child projects (got %d)", len(children))
	}

	var shared splitTarget
	if slug := core.Slugify(strings.TrimSpace(plan.Shared.slug)); slug != "" && slug != source && !targets[slug] {
		label := strings.TrimSpace(plan.Shared.label)
		if label == "" {
			label = slug
		}
		shared = splitTarget{slug: slug, label: label}
		targets[slug] = true
	}
	return children, shared, targets, nil
}

// splitSummary renders a one-line outcome for a split interpretation.
func splitSummary(res RequestResult, source string) string {
	if res.Total == 0 {
		return "no split proposals matched"
	}
	return fmt.Sprintf("split %s: 1 setup + %d memory move(s) proposed", source, res.ByKind[store.ProposalReproject])
}
