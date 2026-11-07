package console

// The Context screen explains the durable scope topology that shapes a
// SessionStart briefing: global memories, optional parent-memory inheritance,
// configured sibling-family crossover, and retired-project split lineage. It
// deliberately does not infer task execution provenance. Plans and tasks own
// execution state; Context owns where knowledge is eligible to flow.

import (
	"context"
	"errors"
	"fmt"
	"net/http"
	"net/url"
	"slices"
	"sort"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// contextScopeKeys are the strict scope enum for /console/context. The all view
// renders every known project scope; project focuses the same topology on one
// named scope. Explicit invalid input is rejected rather than silently defaulted.
var (
	contextScopeKeys  = []string{"all", "project"}
	contextParamNames = []string{"scope", "project", "format"}
)

type contextData struct {
	Scope    string                `json:"scope"`
	Project  string                `json:"project,omitempty"`
	Options  []contextScopeOption  `json:"options"`
	Summary  contextSummary        `json:"summary"`
	Rules    contextRules          `json:"rules"`
	Projects []contextProject      `json:"projects"`
	Families []contextFamily       `json:"families,omitempty"`
	Lineages []contextSplitLineage `json:"lineages,omitempty"`
}

type contextScopeOption struct {
	Slug       string `json:"slug"`
	Label      string `json:"label"`
	Selected   bool   `json:"selected"`
	Registered bool   `json:"registered"`
	Retired    bool   `json:"retired"`
}

type contextSummary struct {
	Scopes      int `json:"scopes"`
	ParentLinks int `json:"parentLinks"`
	Families    int `json:"families"`
	Lineages    int `json:"lineages"`
	Warnings    int `json:"warnings"`
}

// contextRules is the effective briefing policy (file/env base plus any console
// override). The view shows the policy beside the topology because a configured
// edge and an enabled flow are different facts.
type contextRules struct {
	GlobalMemories     int  `json:"globalMemories"`
	GlobalTargets      int  `json:"globalTargets"`
	ParentEnabled      bool `json:"parentEnabled"`
	ParentLinks        int  `json:"parentLinks"`
	SiblingFindings    int  `json:"siblingFindings"`
	SiblingMemories    bool `json:"siblingMemories"`
	BriefingOverridden bool `json:"briefingOverridden"`
}

type contextProject struct {
	Slug           string        `json:"slug"`
	Name           string        `json:"name"`
	Description    string        `json:"description,omitempty"`
	Href           string        `json:"href,omitempty"`
	FocusHref      string        `json:"focusHref"`
	Role           string        `json:"role"`
	TileClass      string        `json:"tileClass,omitempty"`
	TileIcon       string        `json:"tileIcon"`
	Registered     bool          `json:"registered"`
	Retired        bool          `json:"retired"`
	LocalMemories  int           `json:"localMemories"`
	GlobalMemories int           `json:"globalMemories"`
	ParentMemories int           `json:"parentMemories"`
	ParentSlug     string        `json:"parentSlug,omitempty"`
	Incoming       []contextFlow `json:"incoming"`
	Outgoing       []contextFlow `json:"outgoing"`
	Warnings       []string      `json:"warnings,omitempty"`
}

type contextFlow struct {
	Kind    string `json:"kind"`
	Icon    string `json:"icon"`
	Label   string `json:"label"`
	Detail  string `json:"detail"`
	Href    string `json:"href,omitempty"`
	Enabled bool   `json:"enabled"`
}

type contextFamily struct {
	Name            string            `json:"name"`
	Members         []contextScopeRef `json:"members"`
	FindingsEnabled bool              `json:"findingsEnabled"`
	FindingsCount   int               `json:"findingsCount"`
	MemoriesEnabled bool              `json:"memoriesEnabled"`
	Warnings        []string          `json:"warnings,omitempty"`
}

type contextScopeRef struct {
	Slug       string `json:"slug"`
	Name       string `json:"name,omitempty"`
	FocusHref  string `json:"focusHref,omitempty"`
	Global     bool   `json:"global,omitempty"`
	Registered bool   `json:"registered"`
	Retired    bool   `json:"retired"`
}

type contextSplitLineage struct {
	Source       contextScopeRef     `json:"source"`
	RetiredAt    time.Time           `json:"retiredAt"`
	Moves        int                 `json:"moves"`
	Destinations []contextMoveTarget `json:"destinations,omitempty"`
}

type contextMoveTarget struct {
	Scope contextScopeRef `json:"scope"`
	Moves int             `json:"moves"`
}

func (s *Service) contextView(w http.ResponseWriter, r *http.Request) {
	scope, selected, err := contextParams(r.URL.Query())
	if err != nil {
		s.badRequest(w, r, err.Error())
		return
	}

	data, found, err := s.buildContextData(r.Context(), selected)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if selected != "" && !found {
		s.notFound(w, r, fmt.Sprintf("unknown project scope %q", selected))
		return
	}
	data.Scope = scope
	data.Project = selected
	s.render(w, r, "context", pageData{Title: "Context", Active: "projects", Screen: "context", Data: data})
}

// contextParams treats the Context query string as a bookmarkable API: a
// default applies only when a key is absent, while empty, duplicate, unknown,
// and contradictory inputs are errors rather than plausible-looking fallbacks.
func contextParams(values url.Values) (scope, project string, err error) {
	for key, vals := range values {
		if !slices.Contains(contextParamNames, key) {
			return "", "", fmt.Errorf("invalid parameter %q: valid parameters are %s",
				key, strings.Join(contextParamNames, ", "))
		}
		if len(vals) != 1 {
			return "", "", fmt.Errorf("parameter %q must be provided exactly once", key)
		}
	}
	if format, ok := values["format"]; ok && format[0] != "json" {
		return "", "", fmt.Errorf("invalid format %q: valid value is json (or omit)", format[0])
	}

	scope = "all"
	if raw, ok := values["scope"]; ok {
		scope = raw[0]
		if !slices.Contains(contextScopeKeys, scope) {
			return "", "", fmt.Errorf("invalid scope %q: valid values are %s",
				scope, strings.Join(contextScopeKeys, ", "))
		}
	}
	projectValues, hasProject := values["project"]
	if scope != "project" {
		if hasProject {
			return "", "", errors.New("parameter \"project\" requires scope=project")
		}
		return scope, "", nil
	}
	if !hasProject || strings.TrimSpace(projectValues[0]) == "" {
		return "", "", errors.New("scope=project requires a ?project=<slug> param naming the scope to inspect")
	}
	return scope, projectValues[0], nil
}

// relationsRedirect preserves old bookmarks while making /console/context the
// one canonical URL. The query string is carried intact so a focused legacy
// link remains focused after the redirect.
func (s *Service) relationsRedirect(w http.ResponseWriter, r *http.Request) {
	target := "/console/context"
	if r.URL.RawQuery != "" {
		target += "?" + r.URL.RawQuery
	}
	http.Redirect(w, r, target, http.StatusPermanentRedirect)
}

func contextProjectHref(slug string) string {
	return "/console/context?scope=project&project=" + url.QueryEscape(slug)
}

func projectContextHref(slug string) string {
	return "/console/projects/" + url.PathEscape(slug) + "?tab=context"
}

// buildContextData joins the registry, repo mappings, family settings, active
// memory counts, effective briefing policy, and the append-only memory-move log.
// selected filters the rendered scopes and summary while the rule strip remains
// registry-wide. found reports whether selected names any known registered or
// referenced scope.
func (s *Service) buildContextData(ctx context.Context, selected string) (contextData, bool, error) {
	projects, err := store.ListProjects(ctx, s.db(ctx))
	if err != nil {
		return contextData{}, false, fmt.Errorf("console.buildContextData: list projects: %w", err)
	}
	repoRows, err := store.RepoMapRows(ctx, s.db(ctx))
	if err != nil {
		return contextData{}, false, fmt.Errorf("console.buildContextData: repo mappings: %w", err)
	}
	familyMap, err := store.ProjectFamilies(ctx, s.db(ctx))
	if err != nil {
		return contextData{}, false, fmt.Errorf("console.buildContextData: project families: %w", err)
	}
	briefing, overridden, err := store.BriefingConfig(ctx, s.db(ctx), s.cfg.BriefingCfg)
	if err != nil {
		return contextData{}, false, fmt.Errorf("console.buildContextData: briefing config: %w", err)
	}

	workspaces, _ := buildWorkspaceRegistry(projects, repoRows, familyMap)
	bySlug := make(map[string]workspaceScope, len(workspaces))
	for _, workspace := range workspaces {
		bySlug[workspace.Slug] = workspace
	}

	// Active memory metadata also discovers file-backed, unregistered scopes.
	// It is the smallest source that answers both questions Context needs: each
	// local eligibility-pool size and any memory-bearing scope missing from the
	// registry. Avoid the project-board rollup here because its reach report scans
	// telemetry that does not participate in briefing topology.
	memories, err := store.AllActiveMemories(ctx, s.db(ctx))
	if err != nil {
		return contextData{}, false, fmt.Errorf("console.buildContextData: active memories: %w", err)
	}
	memoryCounts := make(map[string]int)
	for _, memory := range memories {
		memoryCounts[memory.Project]++
		if memory.Project == "" {
			continue
		}
		if _, ok := bySlug[memory.Project]; !ok {
			workspace := workspaceScope{Slug: memory.Project}
			workspaces = append(workspaces, workspace)
			bySlug[memory.Project] = workspace
		}
	}
	sort.Slice(workspaces, func(i, j int) bool {
		if workspaces[i].Registered != workspaces[j].Registered {
			return workspaces[i].Registered
		}
		if workspaces[i].Retired != workspaces[j].Retired {
			return !workspaces[i].Retired
		}
		return workspaces[i].Slug < workspaces[j].Slug
	})

	childrenOf := make(map[string][]string)
	for _, workspace := range workspaces {
		if workspace.ParentSlug != "" {
			childrenOf[workspace.ParentSlug] = append(childrenOf[workspace.ParentSlug], workspace.Slug)
		}
	}
	for parent := range childrenOf {
		sort.Strings(childrenOf[parent])
	}

	refs := func(slugs []string) []contextScopeRef {
		out := make([]contextScopeRef, 0, len(slugs))
		for _, slug := range uniqueNonEmpty(slugs) {
			out = append(out, contextRef(slug, bySlug[slug]))
		}
		return out
	}

	families := make([]contextFamily, 0, len(familyMap))
	familiesByMember := make(map[string][]contextFamily)
	for _, group := range sortedFamilies(familyMap) {
		members := uniqueNonEmpty(group.Members)
		family := contextFamily{
			Name: group.Name, Members: refs(members),
			FindingsEnabled: briefing.SiblingFindingsCount > 0,
			FindingsCount:   briefing.SiblingFindingsCount,
			MemoriesEnabled: briefing.IncludeSiblingMemories,
		}
		if len(members) < 2 {
			family.Warnings = append(family.Warnings, "Fewer than two scopes; this family currently shares nothing.")
		}
		for _, member := range family.Members {
			if !member.Registered {
				family.Warnings = append(family.Warnings, fmt.Sprintf("%s is not registered.", member.Slug))
			}
			if member.Retired {
				family.Warnings = append(family.Warnings, fmt.Sprintf("%s is retired but remains in this family.", member.Slug))
			}
		}
		if !family.FindingsEnabled && !family.MemoriesEnabled {
			family.Warnings = append(family.Warnings, "Both sibling channels are disabled in briefing settings.")
		}
		families = append(families, family)
		for _, member := range members {
			familiesByMember[member] = append(familiesByMember[member], family)
		}
	}

	globalMemories := memoryCounts[""]
	allProjects := make([]contextProject, 0, len(workspaces))
	parentLinks := 0
	globalTargets := 0
	for _, workspace := range workspaces {
		globalTargets++
		project := contextProject{
			Slug: workspace.Slug, Name: workspace.Name, Description: workspace.Description,
			FocusHref: contextProjectHref(workspace.Slug), Registered: workspace.Registered,
			Retired: workspace.Retired, LocalMemories: memoryCounts[workspace.Slug],
			GlobalMemories: globalMemories, ParentSlug: workspace.ParentSlug,
		}
		if project.Name == "" {
			project.Name = project.Slug
		}
		if project.Registered {
			project.Href = projectContextHref(project.Slug)
		}
		project.Role, project.TileClass, project.TileIcon = contextProjectRole(
			workspace, len(childrenOf[workspace.Slug]) > 0)

		project.Incoming = append(project.Incoming, contextFlow{
			Kind: "global", Icon: "database", Label: "Global memory", Enabled: true,
			Detail: fmt.Sprintf("%s eligible in every project briefing before age and budget trims.",
				plural(globalMemories, "active memory", "active memories")),
		})
		if workspace.ParentSlug != "" {
			parentLinks++
			project.ParentMemories = memoryCounts[workspace.ParentSlug]
			parent := bySlug[workspace.ParentSlug]
			detail := "Parent-memory inheritance is disabled in briefing settings."
			if briefing.IncludeParentMemories {
				detail = fmt.Sprintf("%s from the shared parent are eligible at SessionStart.",
					plural(project.ParentMemories, "active memory", "active memories"))
			}
			project.Incoming = append(project.Incoming, contextFlow{
				Kind: "parent", Icon: "git-branch", Label: workspace.ParentSlug,
				Detail: detail, Href: contextRef(workspace.ParentSlug, parent).FocusHref,
				Enabled: briefing.IncludeParentMemories,
			})
			if !parent.Registered {
				project.Warnings = append(project.Warnings,
					fmt.Sprintf("Parent %s is referenced but not registered.", workspace.ParentSlug))
			}
			if parent.Retired {
				project.Warnings = append(project.Warnings,
					fmt.Sprintf("Parent %s is retired; its active memories remain eligible while this edge is enabled.", workspace.ParentSlug))
			}
			if workspace.ParentSlug == workspace.Slug {
				project.Warnings = append(project.Warnings, "This scope points to itself as its parent.")
			}
		}

		for _, family := range familiesByMember[workspace.Slug] {
			siblings := familySiblings(family, workspace.Slug)
			if len(siblings) == 0 {
				continue
			}
			detail, enabled := familyFlowDetail(family, siblings)
			flow := contextFlow{
				Kind: "family", Icon: "share-2", Label: "family:" + family.Name,
				Detail: detail, Enabled: enabled,
			}
			project.Incoming = append(project.Incoming, flow)
			project.Outgoing = append(project.Outgoing, flow)
		}

		for _, childSlug := range childrenOf[workspace.Slug] {
			child := bySlug[childSlug]
			detail := "This parent edge is configured, but inheritance is disabled."
			if briefing.IncludeParentMemories {
				detail = fmt.Sprintf("Makes %s eligible in %s at SessionStart.",
					plural(project.LocalMemories, "active memory", "active memories"), childSlug)
			}
			project.Outgoing = append(project.Outgoing, contextFlow{
				Kind: "parent", Icon: "git-branch", Label: childSlug,
				Detail: detail, Href: contextRef(childSlug, child).FocusHref,
				Enabled: briefing.IncludeParentMemories,
			})
		}

		if !workspace.Registered {
			project.Warnings = append(project.Warnings, "This scope has data or routing metadata but no project registry row.")
		}
		if workspace.Retired && project.LocalMemories > 0 {
			project.Warnings = append(project.Warnings,
				fmt.Sprintf("Retired scope still owns %s.", plural(project.LocalMemories, "active memory", "active memories")))
		}
		allProjects = append(allProjects, project)
	}

	lineages, err := s.contextLineages(ctx, projects, bySlug)
	if err != nil {
		return contextData{}, false, fmt.Errorf("console.buildContextData: split lineage: %w", err)
	}

	data := contextData{
		Summary: contextSummary{
			Scopes: len(allProjects), ParentLinks: parentLinks, Families: len(families),
			Lineages: len(lineages), Warnings: contextWarningCount(allProjects, families, lineages),
		},
		Rules: contextRules{
			GlobalMemories: globalMemories, GlobalTargets: globalTargets,
			ParentEnabled: briefing.IncludeParentMemories, ParentLinks: parentLinks,
			SiblingFindings:    briefing.SiblingFindingsCount,
			SiblingMemories:    briefing.IncludeSiblingMemories,
			BriefingOverridden: overridden,
		},
	}

	found := selected == ""
	for _, project := range allProjects {
		data.Options = append(data.Options, contextScopeOption{
			Slug: project.Slug, Label: project.Name, Selected: project.Slug == selected,
			Registered: project.Registered, Retired: project.Retired,
		})
		if selected == "" || project.Slug == selected {
			data.Projects = append(data.Projects, project)
		}
		if project.Slug == selected {
			found = true
		}
	}
	for _, family := range families {
		if selected == "" || familyHasMember(family, selected) {
			data.Families = append(data.Families, family)
		}
	}
	for _, lineage := range lineages {
		if selected == "" || lineageTouches(lineage, selected) {
			data.Lineages = append(data.Lineages, lineage)
		}
	}
	if selected != "" && found {
		data.Summary = contextSummary{
			Scopes:      len(data.Projects),
			ParentLinks: contextParentLinkCount(data.Projects),
			Families:    len(data.Families),
			Lineages:    len(data.Lineages),
			Warnings:    contextWarningCount(data.Projects, data.Families, data.Lineages),
		}
	}
	return data, found, nil
}

func contextParentLinkCount(projects []contextProject) int {
	links := make(map[string]bool)
	for _, project := range projects {
		if project.ParentSlug != "" {
			links[project.ParentSlug+"\x00"+project.Slug] = true
		}
		for _, flow := range project.Outgoing {
			if flow.Kind == "parent" {
				links[project.Slug+"\x00"+flow.Label] = true
			}
		}
	}
	return len(links)
}

func contextWarningCount(projects []contextProject, families []contextFamily, lineages []contextSplitLineage) int {
	count := 0
	for _, project := range projects {
		count += len(project.Warnings)
	}
	for _, family := range families {
		count += len(family.Warnings)
	}
	for _, lineage := range lineages {
		for _, destination := range lineage.Destinations {
			if !destination.Scope.Global && destination.Scope.FocusHref == "" {
				count++
			}
		}
	}
	return count
}

func contextProjectRole(workspace workspaceScope, parent bool) (role, class, iconName string) {
	switch {
	case workspace.Retired:
		return "retired lineage", "", "archive"
	case !workspace.Registered:
		return "unregistered scope", "", "triangle-alert"
	case workspace.ParentSlug != "":
		return "child scope", "", "box"
	case parent:
		return "shared parent", "grp", "folder-tree"
	default:
		return "standalone scope", "", "box"
	}
}

func contextRef(slug string, workspace workspaceScope) contextScopeRef {
	ref := contextScopeRef{
		Slug: slug, Name: workspace.Name,
		Registered: workspace.Registered, Retired: workspace.Retired,
	}
	if workspace.Slug != "" {
		ref.FocusHref = contextProjectHref(slug)
	}
	if ref.Name == "" {
		ref.Name = slug
	}
	return ref
}

func familySiblings(family contextFamily, slug string) []string {
	var siblings []string
	for _, member := range family.Members {
		if member.Slug != slug {
			siblings = append(siblings, member.Slug)
		}
	}
	return siblings
}

func familyFlowDetail(family contextFamily, siblings []string) (string, bool) {
	peers := strings.Join(siblings, ", ")
	switch {
	case family.FindingsEnabled && family.MemoriesEnabled:
		return fmt.Sprintf("Shares recent findings plus eligible non-gate memories with %s; the findings cap is %d total across all family peers.",
			peers, family.FindingsCount), true
	case family.FindingsEnabled:
		return fmt.Sprintf("Shares recent findings with %s (cap %d total across all family peers); sibling memories are off.",
			peers, family.FindingsCount), true
	case family.MemoriesEnabled:
		return fmt.Sprintf("Shares eligible non-gate memories with %s; sibling findings are off.", peers), true
	default:
		return fmt.Sprintf("Linked to %s, but both sibling briefing channels are off.", peers), false
	}
}

func familyHasMember(family contextFamily, slug string) bool {
	for _, member := range family.Members {
		if member.Slug == slug {
			return true
		}
	}
	return false
}

func lineageTouches(lineage contextSplitLineage, slug string) bool {
	if lineage.Source.Slug == slug {
		return true
	}
	for _, destination := range lineage.Destinations {
		if destination.Scope.Slug == slug {
			return true
		}
	}
	return false
}

// contextLineages reconstructs retired-project split destinations from the
// append-only memory.moved log. It pages through the full domain-event history;
// domain events are not retention-pruned, so a fixed recent limit would turn an
// old split into a plausible but incomplete lineage.
func (s *Service) contextLineages(ctx context.Context, projects []core.Project, bySlug map[string]workspaceScope) ([]contextSplitLineage, error) {
	moves := make(map[string]map[string]int)
	if s.cfg.Events != nil {
		beforeTS, beforeID := "", ""
		for {
			page, err := s.eventLog(ctx).ByKinds(ctx, []core.EventKind{core.EventMemoryMoved}, beforeTS, beforeID, 500)
			if err != nil {
				return nil, fmt.Errorf("console.contextLineages: memory moves: %w", err)
			}
			for _, event := range page {
				from := payloadStr(event.Payload, "from")
				if from == "" {
					continue
				}
				rawTo, hasTo := event.Payload["to"]
				to, validTo := rawTo.(string)
				if !hasTo || !validTo {
					continue
				}
				if moves[from] == nil {
					moves[from] = make(map[string]int)
				}
				moves[from][to]++
			}
			if len(page) < 500 {
				break
			}
			last := page[len(page)-1]
			beforeTS, beforeID = core.FormatTime(last.TS), last.ID
		}
	}

	var out []contextSplitLineage
	for _, project := range projects {
		if !project.Retired() || project.RetiredAt == nil {
			continue
		}
		lineage := contextSplitLineage{
			Source: contextRef(project.Slug, bySlug[project.Slug]), RetiredAt: *project.RetiredAt,
		}
		targets := make([]string, 0, len(moves[project.Slug]))
		for target := range moves[project.Slug] {
			targets = append(targets, target)
		}
		sort.Strings(targets)
		for _, target := range targets {
			count := moves[project.Slug][target]
			lineage.Moves += count
			ref := contextRef(target, bySlug[target])
			if target == "" {
				ref = contextScopeRef{Slug: "global", Name: "Global memory", Global: true, Registered: true}
			}
			lineage.Destinations = append(lineage.Destinations, contextMoveTarget{
				Scope: ref, Moves: count,
			})
		}
		out = append(out, lineage)
	}
	sort.Slice(out, func(i, j int) bool {
		if !out[i].RetiredAt.Equal(out[j].RetiredAt) {
			return out[i].RetiredAt.After(out[j].RetiredAt)
		}
		return out[i].Source.Slug < out[j].Source.Slug
	})
	return out, nil
}
