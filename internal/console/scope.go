package console

// The project switcher (plan:console-ux-pass UX 27): the "In <project>"
// button a list screen draws in place of its Project menu, and its popover
// (templates/scope_shared.html, static/scope.js). The model is built here,
// once per request, from the screen's own Project menu (libProjectMenu: each
// scope the screen holds a match in, with what choosing it would show, the
// other filters applied) and the store: each project's last activity and
// star through the request's handle, and the locked projects this request
// does not read. The browser adds only Recent, from its own storage.

import (
	"context"
	"net/url"
	"sort"
	"time"

	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/validate"
)

// projectSwitcher is the switcher's model. HTML only.
type projectSwitcher struct {
	// Label names the current choice on the button: "All projects", the
	// global scope's label, or a project's slug. Value is its ?project=
	// value ("" for All projects), which scope.js remembers as recent.
	Label, Value string
	// All is All projects, counting what every scope here holds.
	All scopeChoice
	// Starred are the starred projects, most active first. Each is also in
	// Active or Quiet: the popover lists it twice while the field is empty.
	Starred []scopeChoice
	// Active are the scopes with activity in the last 30 days, most active
	// first, then the current choice wherever its activity is, then the
	// global scope, last.
	Active []scopeChoice
	// Quiet are the projects with no session or task in 30 days, most
	// recently active first: a plain list without script, found by typing
	// with it.
	Quiet []scopeChoice
}

// scopeChoice is one row of the switcher: a link to the same screen with its
// ?project= set and every other filter kept, or, for a locked project this
// request does not read, a link to its unlock page.
type scopeChoice struct {
	// Value is the ?project= value: "" (All projects), "global", or a slug.
	Value string
	// ID is the row's element id, one per row on the page: sc-o-<value>
	// (sc-o-all for All projects), sc-s-<value> for a Starred copy. The
	// live morph matches rows by it, and the field names the highlighted
	// row by it.
	ID string
	// Label is what the row shows; Name the project's display name when it
	// differs from its slug (typing matches it, and it is the row's title).
	Label, Name string
	Href        string
	// Count is what choosing it would show here. Counted is false for a
	// locked project: what it holds is not this request's to count.
	Count   int
	Counted bool
	// Hint is the row's quiet line: when work last moved there, or what the
	// global scope or a lock means. At is that moment, zero when the hint
	// is no time.
	Hint    string
	At      time.Time
	Starred bool
	Locked  bool
	On      bool
}

// scopeGlobalHint is the global scope's line: it is no project anyone
// follows, it is what every project shares.
const scopeGlobalHint = "every project reads these"

// scopeLockedHint is a locked project's line: its name with the way in,
// never a count of what it holds.
const scopeLockedHint = "locked: unlock to see it"

// projectSwitcher builds the switcher for a library page from its Project
// menu. Activity and stars come through the request's handle, so a locked
// project is absent from that read; the locked projects this request does
// not read come from the Projects board's own read for them
// (store.ProjectTallies on the main handle), of which the switcher keeps the
// slug, the name and the last activity, never a count.
func (s *Service) projectSwitcher(ctx context.Context, menu []libLink) (projectSwitcher, error) {
	facts, err := store.ProjectActivityBySlug(ctx, s.db(ctx))
	if err != nil {
		return projectSwitcher{}, err
	}
	var locked []store.ProjectTally
	if slugs := lockedProjects(ctx); len(slugs) > 0 {
		if locked, err = store.ProjectTallies(ctx, s.writeDB(), slugs); err != nil {
			return projectSwitcher{}, err
		}
	}
	return buildProjectSwitcher(menu, facts, locked, time.Now()), nil
}

// buildProjectSwitcher orders menu's choices for the switcher at now: a
// project with no session or task in projectQuietAfter is quiet (the
// Projects board's Quiet band) unless it is the current choice; the rest are
// most active first, the global scope last. A registered locked project
// joins the same order by its last activity, with a lock and no count; a
// locked slug no project is registered under is left out, as the board
// leaves it out.
func buildProjectSwitcher(menu []libLink, facts map[string]store.ProjectActivity, locked []store.ProjectTally, now time.Time) projectSwitcher {
	sw := projectSwitcher{Label: "All projects"}
	var global *scopeChoice
	var rows []scopeChoice
	lockedAt := map[string]int{}
	for _, t := range locked {
		if !t.Registered {
			continue
		}
		c := scopeChoice{Value: t.Slug, ID: "sc-o-" + t.Slug, Label: t.Slug, Href: "/console/unlock/" + url.PathEscape(t.Slug),
			Hint: scopeLockedHint, At: t.LastActive, Locked: true}
		if t.Name != t.Slug {
			c.Name = t.Name
		}
		lockedAt[t.Slug] = len(rows)
		rows = append(rows, c)
	}
	for _, l := range menu {
		switch l.Value {
		case "":
			sw.All = scopeChoice{ID: "sc-o-all", Label: l.Label, Href: l.Href, Counted: true, On: l.On}
			continue
		case validate.GlobalProject:
			global = &scopeChoice{Value: l.Value, ID: "sc-o-" + l.Value, Label: l.Label, Href: l.Href, Count: l.Count, Counted: true,
				Hint: scopeGlobalHint, On: l.On}
			sw.All.Count += l.Count
			continue
		}
		if i, ok := lockedAt[l.Value]; ok {
			// The current choice names a project this request does not
			// read: the menu kept it with nothing to count, the lock row
			// stands for it.
			rows[i].On = rows[i].On || l.On
			continue
		}
		f := facts[l.Value]
		rows = append(rows, scopeChoice{Value: l.Value, ID: "sc-o-" + l.Value, Label: l.Label, Name: l.Title, Href: l.Href,
			Count: l.Count, Counted: true, Hint: scopeActiveHint(f.LastActive, now), At: f.LastActive,
			Starred: f.Favorite, On: l.On})
		sw.All.Count += l.Count
	}

	sort.SliceStable(rows, func(i, j int) bool {
		a, b := rows[i], rows[j]
		if !a.At.Equal(b.At) {
			return a.At.After(b.At)
		}
		return a.Label < b.Label
	})
	for _, c := range rows {
		if c.On {
			sw.Label, sw.Value = c.Label, c.Value
		}
		if c.Starred {
			copied := c
			copied.ID = "sc-s-" + c.Value
			sw.Starred = append(sw.Starred, copied)
		}
		if c.On || !scopeQuiet(c.At, now) {
			sw.Active = append(sw.Active, c)
		} else {
			sw.Quiet = append(sw.Quiet, c)
		}
	}
	if global != nil {
		if global.On {
			sw.Label, sw.Value = global.Label, global.Value
		}
		sw.Active = append(sw.Active, *global)
	}
	return sw
}

// scopeQuiet reports whether a project last active at last is quiet at now:
// no session or task within projectQuietAfter, or none ever.
func scopeQuiet(last, now time.Time) bool {
	return last.IsZero() || now.Sub(last) > projectQuietAfter
}

// scopeActiveHint words a project's last activity: "active 4m ago", or "" when
// it has none.
func scopeActiveHint(last, now time.Time) string {
	if last.IsZero() {
		return ""
	}
	a := agoAt(last, now)
	if a == "just now" || a == "now" {
		return "active " + a
	}
	return "active " + a + " ago"
}
