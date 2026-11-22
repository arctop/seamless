package console

// The project directory and the families (Settings > Workspaces).

import (
	"errors"
	"fmt"
	"net/http"
	"sort"
	"strings"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/validate"
)

// repoMapping is one (host, path) -> project route, read from the repo_map
// table rather than the legacy flat JSON mirror: once several machines share one
// daemon, two of them can mount the same path on different repositories, so a
// route without its host names nothing.
type repoMapping struct {
	// Host is the machine the path lives on. Empty is the LEGACY bucket -- rows
	// written before host scoping, or by a seeder that never named its machine
	// -- and renders as "unknown", never as "this machine".
	Host    string `json:"host,omitempty"`
	Repo    string `json:"repo"`
	Project string `json:"project"`
}

// repoRoute is one mapped repo path under a workspace scope, with the machine
// it lives on.
type repoRoute struct {
	Host string `json:"host,omitempty"`
	Path string `json:"path"`
}

type familyGroup struct {
	Name    string   `json:"name"`
	Members []string `json:"members"`
}

type familyScopeRef struct {
	Slug       string `json:"slug"`
	Registered bool   `json:"registered"`
}

type workspaceFamily struct {
	Name        string `json:"name"`
	MemberCount int    `json:"memberCount"`
}

type familyProjectOption struct {
	Slug       string `json:"slug"`
	Name       string `json:"name,omitempty"`
	Registered bool   `json:"registered"`
	Retired    bool   `json:"retired"`
	Selected   bool   `json:"selected"`
}

type familyEditor struct {
	Name    string                `json:"name"`
	Members []familyScopeRef      `json:"members"`
	Options []familyProjectOption `json:"options"`
}

// workspaceScope joins the three ways Settings describes a scope: its project
// row, the repo paths that resolve to it, and the families that share context
// with it. Registered is false for slugs referenced only by routing, lineage,
// or family metadata, so the console can expose those dangling references
// instead of silently dropping them from the unified directory.
type workspaceScope struct {
	Slug             string            `json:"slug"`
	Name             string            `json:"name,omitempty"`
	Description      string            `json:"description,omitempty"`
	ParentSlug       string            `json:"parentSlug,omitempty"`
	Registered       bool              `json:"registered"`
	Retired          bool              `json:"retired"`
	ParentRegistered bool              `json:"parentRegistered"`
	Repos            []repoRoute       `json:"repos"`
	Families         []workspaceFamily `json:"families"`
}

// FindText is what the directory's filter matches a row against: its slug,
// its name and each repo route's path, as the family dialog's filter matches
// a project's slug and name. The page lowercases it and the query alike.
func (w workspaceScope) FindText() string {
	parts := make([]string, 0, 2+len(w.Repos))
	parts = append(parts, w.Slug)
	if w.Name != "" && w.Name != w.Slug {
		parts = append(parts, w.Name)
	}
	for _, r := range w.Repos {
		parts = append(parts, r.Path)
	}
	return strings.Join(parts, " ")
}

// workspaceRoutedOnlyAfter is how many directory rows without a repo route it
// takes for the directory to open on "With repo routes": past it, rows that
// show a project and nothing else outnumber the ones the directory is for.
const workspaceRoutedOnlyAfter = 12

// workspaceDirectory is what the directory's tools and columns follow,
// counted from its rows (a project, or a slug a route, a parent or a family
// names). Page state only: it is a method's answer, never a JSON field, so
// ?format=json keeps exactly the fields it had.
type workspaceDirectory struct {
	Routed   int // rows with at least one repo route
	Unrouted int // rows with none
	// Toggle offers the "With repo routes" switch, only where it changes what
	// shows: some rows have a route and some do not.
	Toggle bool
	// RoutedOnly is the switch's starting state: on when more than
	// workspaceRoutedOnlyAfter rows have no route. Without script the bar
	// that holds the switch stays hidden and every row shows.
	RoutedOnly bool
	// Family draws the Family column: some row has a family or a parent.
	Family bool
}

// WorkspaceDirectory counts the Workspaces directory for its template.
func (d settingsData) WorkspaceDirectory() workspaceDirectory {
	return newWorkspaceDirectory(d.Workspaces)
}

func newWorkspaceDirectory(rows []workspaceScope) workspaceDirectory {
	var dir workspaceDirectory
	for _, row := range rows {
		if len(row.Repos) > 0 {
			dir.Routed++
		} else {
			dir.Unrouted++
		}
		if row.ParentSlug != "" || len(row.Families) > 0 {
			dir.Family = true
		}
	}
	dir.Toggle = dir.Routed > 0 && dir.Unrouted > 0
	dir.RoutedOnly = dir.Toggle && dir.Unrouted > workspaceRoutedOnlyAfter
	return dir
}

// settingsFamilySave creates a family or replaces one family's name and member
// set. The picker submits a closed list of known project slugs; accepting an
// arbitrary slug here would let a typo create a plausible but inert family
// member that never contributes context.
func (s *Service) settingsFamilySave(w http.ResponseWriter, r *http.Request) {
	names, ok := r.PostForm["name"]
	if !ok || len(names) != 1 {
		settingsRegistryFlash(w, r, "Enter one family name.")
		return
	}
	name := strings.TrimSpace(names[0])
	if err := validate.Name(name); err != nil {
		settingsRegistryFlash(w, r, "Family name: "+err.Error())
		return
	}

	previousName := ""
	if previous, present := r.PostForm["original_name"]; present {
		if len(previous) != 1 || strings.TrimSpace(previous[0]) == "" {
			settingsRegistryFlash(w, r, "The family being edited is missing.")
			return
		}
		previousName = strings.TrimSpace(previous[0])
		if err := validate.Name(previousName); err != nil {
			settingsRegistryFlash(w, r, "Original family name: "+err.Error())
			return
		}
	}

	members := uniqueNonEmpty(r.PostForm["members"])
	if len(members) == 0 {
		settingsRegistryFlash(w, r, "Choose at least one project for the family.")
		return
	}
	projects, err := store.ListProjects(r.Context(), s.db(r.Context()))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	families, err := store.ProjectFamilies(r.Context(), s.db(r.Context()))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	allowed := make(map[string]bool, len(projects)+len(members))
	for _, project := range projects {
		allowed[project.Slug] = true
	}
	if previousName != "" {
		currentMembers, exists := families[previousName]
		if !exists {
			settingsRegistryFlash(w, r, fmt.Sprintf("Family %q no longer exists.", previousName))
			return
		}
		for _, member := range currentMembers {
			allowed[member] = true
		}
	}
	for _, member := range members {
		if err := validate.Name(member); err != nil {
			settingsRegistryFlash(w, r, "Project slug: "+err.Error())
			return
		}
		if !allowed[member] {
			settingsRegistryFlash(w, r, fmt.Sprintf("Unknown project scope %q.", member))
			return
		}
	}

	// Isolation requires a standalone project, so a fenced slug may not join a
	// family. The guard lives here rather than inside SaveProjectFamily on
	// purpose: gardener.Apply calls AddFamilyMembers during a split, and a new
	// error out of the store would land in serverError's 500 branch instead of
	// this flash. The owner's other route into a family -- tightening a project
	// that is already in one -- is handled by TightenProjectIsolation, which
	// detaches it as part of the tighten.
	fenced, err := store.IsolatedSlugs(r.Context(), s.db(r.Context()), members)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if len(fenced) > 0 {
		labels := make([]string, len(fenced))
		for i, f := range fenced {
			labels[i] = fmt.Sprintf("%s (%s)", f.Slug, f.State)
		}
		settingsRegistryFlash(w, r, fmt.Sprintf(
			"Isolated projects cannot join a family: %s. Set them back to open first.",
			strings.Join(labels, ", ")))
		return
	}

	_, err = store.SaveProjectFamily(r.Context(), s.writeDB(), previousName, name, members)
	switch {
	case errors.Is(err, store.ErrFamilyExists):
		settingsRegistryFlash(w, r, fmt.Sprintf("A family named %q already exists.", name))
		return
	case errors.Is(err, store.ErrFamilyNotFound):
		settingsRegistryFlash(w, r, fmt.Sprintf("Family %q no longer exists.", previousName))
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	verb := "created"
	if previousName != "" {
		verb = "updated"
	}
	settingsRegistryNotice(w, r, fmt.Sprintf("Family %q %s.", name, verb))
}

func (s *Service) settingsFamilyDelete(w http.ResponseWriter, r *http.Request) {
	names, ok := r.PostForm["original_name"]
	if !ok || len(names) != 1 {
		settingsRegistryFlash(w, r, "Choose one family to delete.")
		return
	}
	name := strings.TrimSpace(names[0])
	if err := validate.Name(name); err != nil {
		settingsRegistryFlash(w, r, "Family name: "+err.Error())
		return
	}
	_, err := store.RemoveFamilyMembers(r.Context(), s.writeDB(), name, nil)
	switch {
	case errors.Is(err, store.ErrFamilyNotFound):
		settingsRegistryFlash(w, r, fmt.Sprintf("Family %q no longer exists.", name))
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	settingsRegistryNotice(w, r, fmt.Sprintf("Family %q deleted.", name))
}

func settingsRegistryFlash(w http.ResponseWriter, r *http.Request, msg string) {
	settingsBack(w, r, "workspaces", "error", msg, "workspace-registry")
}

func settingsRegistryNotice(w http.ResponseWriter, r *http.Request, msg string) {
	settingsBack(w, r, "workspaces", "notice", msg, "workspace-registry")
}

// sortedRepoMap projects the repo_map rows for display. store.RepoMapRows
// already returns them ordered by host then path, so this preserves that order
// rather than re-sorting by path alone: grouping a machine's routes together is
// the whole point of showing the host.
func sortedRepoMap(rows []store.RepoMapRow) []repoMapping {
	out := make([]repoMapping, 0, len(rows))
	for _, r := range rows {
		out = append(out, repoMapping{Host: r.Host, Repo: r.Path, Project: r.Slug})
	}
	return out
}

func sortedFamilies(m map[string][]string) []familyGroup {
	out := make([]familyGroup, 0, len(m))
	for name, members := range m {
		sorted := append([]string(nil), members...)
		sort.Strings(sorted)
		out = append(out, familyGroup{Name: name, Members: sorted})
	}
	sort.Slice(out, func(i, j int) bool { return out[i].Name < out[j].Name })
	return out
}

// lessRoute orders routes by host then path, matching store.RepoMapRows so a
// machine's checkouts stay grouped wherever they are rendered.
func lessRoute(a, b repoRoute) bool {
	if a.Host != b.Host {
		return a.Host < b.Host
	}
	return a.Path < b.Path
}

// buildWorkspaceRegistry joins the project registry, the repo_map rows and the
// family settings into the Settings/Context workspace view, returning the scopes
// plus the routes that resolve to no scope at all.
func buildWorkspaceRegistry(projects []core.Project, repoRows []store.RepoMapRow, families map[string][]string) ([]workspaceScope, []repoRoute) {
	bySlug := make(map[string]*workspaceScope, len(projects))
	ensure := func(slug string) *workspaceScope {
		if scope, ok := bySlug[slug]; ok {
			return scope
		}
		scope := &workspaceScope{Slug: slug}
		bySlug[slug] = scope
		return scope
	}

	for _, project := range projects {
		slug := strings.TrimSpace(project.Slug)
		if slug == "" {
			continue
		}
		scope := ensure(slug)
		scope.Name = project.Name
		scope.Description = project.Description
		scope.ParentSlug = strings.TrimSpace(project.ParentSlug)
		scope.Registered = true
		scope.Retired = project.Retired()
	}
	for _, project := range projects {
		if parent := strings.TrimSpace(project.ParentSlug); parent != "" {
			ensure(parent)
		}
	}

	var unboundRepos []repoRoute
	for _, row := range repoRows {
		repo := repoRoute{Host: row.Host, Path: row.Path}
		slug := strings.TrimSpace(row.Slug)
		if slug == "" {
			unboundRepos = append(unboundRepos, repo)
			continue
		}
		scope := ensure(slug)
		scope.Repos = append(scope.Repos, repo)
	}

	for _, family := range sortedFamilies(families) {
		members := uniqueNonEmpty(family.Members)
		for _, slug := range members {
			ensure(slug)
		}
		for _, slug := range members {
			bySlug[slug].Families = append(bySlug[slug].Families, workspaceFamily{
				Name:        family.Name,
				MemberCount: len(members),
			})
		}
	}

	out := make([]workspaceScope, 0, len(bySlug))
	for _, scope := range bySlug {
		sort.Slice(scope.Repos, func(i, j int) bool { return lessRoute(scope.Repos[i], scope.Repos[j]) })
		sort.Slice(scope.Families, func(i, j int) bool {
			return scope.Families[i].Name < scope.Families[j].Name
		})
		if scope.ParentSlug != "" {
			scope.ParentRegistered = bySlug[scope.ParentSlug].Registered
		}
		out = append(out, *scope)
	}
	sort.Slice(out, func(i, j int) bool {
		if out[i].Registered != out[j].Registered {
			return out[i].Registered
		}
		if out[i].Retired != out[j].Retired {
			return !out[i].Retired
		}
		return out[i].Slug < out[j].Slug
	})
	sort.Slice(unboundRepos, func(i, j int) bool { return lessRoute(unboundRepos[i], unboundRepos[j]) })
	return out, unboundRepos
}

func buildFamilyEditors(workspaces []workspaceScope, families map[string][]string) ([]familyEditor, []familyProjectOption) {
	bySlug := make(map[string]workspaceScope, len(workspaces))
	createOptions := make([]familyProjectOption, 0, len(workspaces))
	for _, workspace := range workspaces {
		bySlug[workspace.Slug] = workspace
		if workspace.Registered && !workspace.Retired {
			createOptions = append(createOptions, familyProjectOption{
				Slug:       workspace.Slug,
				Name:       workspace.Name,
				Registered: true,
			})
		}
	}

	groups := sortedFamilies(families)
	editors := make([]familyEditor, 0, len(groups))
	for _, group := range groups {
		members := uniqueNonEmpty(group.Members)
		selected := make(map[string]bool, len(members))
		editor := familyEditor{
			Name:    group.Name,
			Members: make([]familyScopeRef, 0, len(members)),
		}
		for _, slug := range members {
			selected[slug] = true
			editor.Members = append(editor.Members, familyScopeRef{
				Slug:       slug,
				Registered: bySlug[slug].Registered,
			})
		}
		for _, workspace := range workspaces {
			if (!workspace.Registered || workspace.Retired) && !selected[workspace.Slug] {
				continue
			}
			editor.Options = append(editor.Options, familyProjectOption{
				Slug:       workspace.Slug,
				Name:       workspace.Name,
				Registered: workspace.Registered,
				Retired:    workspace.Retired,
				Selected:   selected[workspace.Slug],
			})
		}
		editors = append(editors, editor)
	}
	return editors, createOptions
}

func uniqueNonEmpty(values []string) []string {
	seen := make(map[string]bool, len(values))
	out := make([]string, 0, len(values))
	for _, value := range values {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		out = append(out, value)
	}
	sort.Strings(out)
	return out
}
