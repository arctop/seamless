package console

// A project page's "came from <project>" filter (plan fenced-sessions P10),
// over the provenance index (store.CameFromSources, store.CameFromItems):
// which of the project's memories, notes and tasks may contain content from
// another project. ?from=<project> narrows the Memories, Notes and Plans &
// tasks tabs to those items; the Plans & tasks tab then lists the tasks
// themselves instead of the plans they belong to. A source that is a locked
// project this request does not read is named and nothing more: the items
// listed are this project's own.
//
// Opt-in by its data: a project none of whose items carries a stamp -- every
// project of an install that never turned isolation.requests on -- renders
// no filter, and its page is as it was.

import (
	"context"
	"net/url"
	"slices"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// cameFromTabs are the workspace tabs the filter narrows, each with the item
// kind it lists.
var cameFromTabs = map[string]string{"memories": core.ItemKindMemory, "notes": core.ItemKindNote, "tasks": core.ItemKindTask}

// cameFromView is the filter's state on one project page.
type cameFromView struct {
	// Active is the source the tabs are narrowed to, "" for none.
	Active string
	// Sources are the projects this project's items may contain content
	// from, sorted, each with its counts by kind.
	Sources []store.CameFromSource
	// Tasks are the project's tasks that may contain content from Active,
	// newest stamp first: the Plans & tasks tab while the filter is on.
	Tasks []taskRow
	// locked are the sources that are locked projects this request does not
	// read (their names only).
	locked map[string]bool
}

// cameFromBar is one tab's filter bar.
type cameFromBar struct {
	Tab    string
	Active string
	// Any is the bar's "any project" link: the tab without the filter.
	Any   string
	Chips []cameFromChip
}

// cameFromChip is one source in a tab's bar.
type cameFromChip struct {
	Project string
	Locked  bool
	Count   int
	On      bool
	Href    string
}

// Bar is the filter bar of tab (memories, notes or tasks): every source with
// its count of that tab's kind, and the active one -- also when no item names
// it, so the page says what it is filtered by.
func (v *cameFromView) Bar(tab string) cameFromBar {
	kind := cameFromTabs[tab]
	b := cameFromBar{Tab: tab, Active: v.Active, Any: "?tab=" + url.QueryEscape(tab)}
	chip := func(project string, n int) cameFromChip {
		return cameFromChip{Project: project, Locked: v.locked[project], Count: n, On: project == v.Active,
			Href: "?tab=" + url.QueryEscape(tab) + "&from=" + url.QueryEscape(project)}
	}
	for _, src := range v.Sources {
		b.Chips = append(b.Chips, chip(src.Project, src.Counts[kind]))
	}
	if v.Active != "" && !slices.ContainsFunc(v.Sources, func(s store.CameFromSource) bool { return s.Project == v.Active }) {
		b.Chips = append(b.Chips, chip(v.Active, 0))
	}
	return b
}

// cameFromFor reads the filter for project: nil when no item of it carries a
// stamp and no filter is asked for. With from set it also returns the ids the
// tabs keep, by kind.
func (s *Service) cameFromFor(ctx context.Context, project, from string) (*cameFromView, map[string]map[string]bool, error) {
	sources, err := store.CameFromSources(ctx, s.db(ctx), project)
	if err != nil {
		return nil, nil, err
	}
	if len(sources) == 0 && from == "" {
		return nil, nil, nil
	}
	v := &cameFromView{Active: from, Sources: sources, locked: map[string]bool{}}
	for _, src := range append(slices.Clone(sources), store.CameFromSource{Project: from}) {
		if src.Project != "" && lockedHere(ctx, src.Project) {
			v.locked[src.Project] = true
		}
	}
	if from == "" {
		return v, nil, nil
	}
	ids, err := store.CameFromItems(ctx, s.db(ctx), project, from)
	if err != nil {
		return nil, nil, err
	}
	keep := map[string]map[string]bool{}
	for _, kind := range store.CameFromKinds {
		keep[kind] = map[string]bool{}
		for _, id := range ids[kind] {
			keep[kind][id] = true
		}
	}
	if taskIDs := ids[core.ItemKindTask]; len(taskIDs) > 0 {
		got, err := store.TasksByIDs(ctx, s.db(ctx), taskIDs)
		if err != nil {
			return nil, nil, err
		}
		var tasks []core.Task
		for _, id := range taskIDs {
			if t, ok := got[id]; ok {
				tasks = append(tasks, t)
			}
		}
		v.Tasks = taskRows(tasks)
		stamps := s.stampsByID(ctx, core.ItemKindTask, taskIDs)
		for i := range v.Tasks {
			v.Tasks[i].MayContain = core.StampsShown(stamps[v.Tasks[i].ID], project)
		}
	}
	return v, keep, nil
}

// applyCameFrom narrows the workspace's Memories and Notes tabs to the items
// the filter keeps (the inherited memories are other projects': they leave
// the tab while it is on), and gives the rows of this project their stamps.
func (s *Service) applyCameFrom(ctx context.Context, data *projectWorkspaceData, keep map[string]map[string]bool) {
	if data.CameFrom == nil {
		return
	}
	if keep != nil {
		data.Memories = slices.DeleteFunc(data.Memories, func(m projMemoryVM) bool { return !keep[core.ItemKindMemory][m.ID] })
		data.Notes = slices.DeleteFunc(data.Notes, func(n projNoteVM) bool { return !keep[core.ItemKindNote][n.ID] })
		data.Inherited = nil
	}
	memIDs := make([]string, len(data.Memories))
	for i, m := range data.Memories {
		memIDs[i] = m.ID
	}
	memStamps := s.stampsByID(ctx, core.ItemKindMemory, memIDs)
	for i := range data.Memories {
		data.Memories[i].MayContain = core.StampsShown(memStamps[data.Memories[i].ID], data.Slug)
	}
	noteIDs := make([]string, len(data.Notes))
	for i, n := range data.Notes {
		noteIDs[i] = n.ID
	}
	noteStamps := s.stampsByID(ctx, core.ItemKindNote, noteIDs)
	for i := range data.Notes {
		data.Notes[i].MayContain = core.StampsShown(noteStamps[data.Notes[i].ID], data.Slug)
	}
}
