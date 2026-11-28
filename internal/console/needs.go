package console

// The "needs you" model (plan:console-ux-pass UX 12): everything that waits
// on the owner, as one ordered list of items. The Overview's strip draws it,
// and the sidebar's queue badges are meant to read the same items (Kind,
// Count, Href) rather than count again.
//
// The order says what is most urgent. First what an agent is blocked on right
// now: a pending access request, a held write, a lock declaration, an open
// gate waiting on the owner (gates waiting on someone else follow, named), a
// plan awaiting approval, a lapsed claim and a claimless one. Then the
// owner's own queues: mishaps not yet reviewed, an update that failed or
// paused itself, backups failing, missing or older than a week. Then the
// knowledge queue: gardener proposals, what automatic apply applied, memories
// going stale, and last momentum's finish lines, the only positive entries.
//
// Each item is one sentence (Title), a count, a link (Href), and, where one
// action settles it, that action inline (Action), posting to the route the
// item's own screen posts to, with the Overview as its next. An item whose
// optional feature is off does not exist: its read never runs. Every read
// goes through the request's handle (s.db(ctx)), so a locked project's
// gates, claims, plans and mishaps are absent from the read itself. A failed
// read costs its item, never the page.

import (
	"cmp"
	"context"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strconv"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/features"
	"github.com/arctop/seamless/internal/plans"
	"github.com/arctop/seamless/internal/store"
)

// The item kinds: stable keys for the strip's data-need attribute, the JSON
// answer and the sidebar.
const (
	needAccess      = "access"
	needHeld        = "held"
	needLocks       = "lock-declarations"
	needGates       = "gates"
	needGatesOthers = "gates-others"
	needPlans       = "plan-approval"
	needLapsed      = "claims-lapsed"
	needUnclaimed   = "claims-unclaimed"
	needMishaps     = "mishaps"
	needUpdates     = "updates"
	needBackups     = "backups"
	needProposals   = "proposals"
	needAutoApplied = "auto-applied"
	needStale       = "stale"
	needFinishLine  = "finish-line"
)

// needsAnchor is the strip's id: an inline action returns to it.
const needsAnchor = "needs"

// backupOldAfter is when the newest backup counts as old.
const backupOldAfter = 7 * 24 * time.Hour

// attnCard is one item of the needs-you model, one card on the Overview's
// strip: something waiting on the owner, with the screen that answers it and,
// where one action settles it, that action. An empty bucket is no item at all
// -- an empty "0 mishaps" card would spend the most valuable strip on the page
// saying nothing.
type attnCard struct {
	// Kind is the item's stable key (the need* constants).
	Kind string
	Sev  string // "danger" | "warn" | "info" | "ok" (the finish-line cards, the strip's only positive entries)
	Icon string
	// Count is how many things the item stands for: requests, gates, reports.
	Count int
	Title string
	Sub   string
	// Href is the screen that answers the item, whatever the level (the JSON
	// answer is the same at every level); Linked says whether this request's
	// level offers it as a link (linkOffer).
	Href   string
	Linked bool `json:"-"`
	// Blocking marks an item an agent is blocked on right now.
	Blocking bool `json:",omitempty"`
	// Action settles the item in one step, nil when none does.
	Action *needAction `json:",omitempty"`
	// Bar is the finish-line card's progress draw (finishBar); empty on every
	// other card.
	Bar template.HTML
}

// needAction is an inline action: a form posting Fields to Post. Confirm,
// when set, arms the button in the page first (data-confirm).
type needAction struct {
	Label        string
	Icon         string
	Post         string
	Fields       []needField
	Title        string `json:",omitempty"`
	Confirm      string `json:",omitempty"`
	ConfirmLabel string `json:",omitempty"`
}

// needField is one hidden field of an inline action.
type needField struct{ Name, Value string }

// needsInput is what the overview already read that the model reuses.
type needsInput struct {
	now time.Time
	// next is where an inline action returns: this Overview, at the strip.
	next string
	// work is the work in motion (nowWork): its loose ends are the claims.
	work                   nowData
	pending                int
	staleUnseen, staleDays int
	offer                  linkOffer
}

// needsFor builds the model in its order and marks each item's link.
func (s *Service) needsFor(ctx context.Context, in needsInput) []attnCard {
	feats := s.effectiveFeatures(ctx)
	var out []attnCard
	out = append(out, s.needAccessRequests(ctx, in.now)...)
	out = append(out, s.needHeldWrites(ctx)...)
	if features.Enabled(feats, features.RepoLocks) {
		out = append(out, s.needLockDeclarations(ctx, in.next)...)
	}
	out = append(out, s.needOpenGates(ctx, in.next)...)
	out = append(out, s.needPlanApprovals(ctx, in.next)...)
	out = append(out, needClaims(in.work.Loose, in.next)...)
	out = append(out, s.needMishapReview(ctx)...)
	out = append(out, s.needUpdate(in.now)...)
	out = append(out, s.needBackup(in.now)...)
	out = append(out, s.needKnowledge(ctx, in)...)
	for i := range out {
		out[i].Linked = in.offer.offers(out[i].Href)
	}
	return out
}

// needsTotal is the strip's head: the items its cards stand for, each card's
// Count added up (36 gates waiting are 36, not one card), and the tooltip that
// says so. The finish-line cards are good news rather than something waiting
// on the owner, and their Count is a plan's steps left, so they are not
// counted. Zero when the strip holds only finish lines: the head then shows
// no number.
func needsTotal(cards []attnCard) (int, string) {
	n, counted, finish := 0, 0, 0
	for _, c := range cards {
		if c.Kind == needFinishLine {
			finish++
			continue
		}
		n += c.Count
		counted++
	}
	if n == 0 {
		return 0, ""
	}
	tip := plural(n, "item", "items") + " on " + plural(counted, "card", "cards")
	if finish > 0 {
		tip += "; plans near the finish line are not counted"
	}
	return n, tip
}

// overviewNext is where an inline action on the Overview returns: the
// Overview as this request asked for it (its window kept; its format and
// one-shot flash not), at the strip.
func overviewNext(r *http.Request) string {
	u := *r.URL
	q := u.Query()
	q.Del("format")
	q.Del("notice")
	q.Del("error")
	u.RawQuery = q.Encode()
	return safeNext(u.RequestURI()) + "#" + needsAnchor
}

// needAccessRequests is the pending access requests, while a project is
// locked (peer_auth) or read grants are on. Approving one is a passkey
// ceremony on its own page, so the item links there and acts on nothing.
func (s *Service) needAccessRequests(ctx context.Context, now time.Time) []attnCard {
	if s.cfg.Approvals == nil && s.cfg.Grants == nil {
		return nil
	}
	pending, err := store.PendingAccessApprovals(ctx, s.db(ctx), now)
	if err != nil {
		s.logger.Warn("console: access requests for the needs strip", "error", err)
		return nil
	}
	if len(pending) == 0 {
		return nil
	}
	var projects []string
	for _, a := range pending {
		if !slices.Contains(projects, a.Project) {
			projects = append(projects, a.Project)
		}
	}
	href := "/console/access"
	if len(pending) == 1 {
		href += "/" + url.PathEscape(pending[0].ID)
	}
	return []attnCard{{
		Kind: needAccess, Sev: "warn", Icon: "lock", Count: len(pending), Blocking: true,
		Title: plural(len(pending), "access request waits for you", "access requests wait for you"),
		Sub:   "for " + approval.JoinAnd(projects) + " · oldest " + agoPhrase(pending[0].RequestedAt),
		Href:  href,
	}}
}

// needHeldWrites is the writes held for the owner: every held write while
// write screening is on, the restricted accounts' proposals while it is off
// and they wait there. Releasing one is read first on the Held screen.
func (s *Service) needHeldWrites(ctx context.Context) []attnCard {
	n := 0
	switch s.heldModeFor(s.effectiveFeatures(ctx)) {
	case heldScreening:
		held, err := store.CountPendingHolds(ctx, s.db(ctx))
		if err != nil {
			s.logger.Warn("console: held writes for the needs strip", "error", err)
			return nil
		}
		n = held
	case heldProposals:
		held, err := s.pendingProposals(ctx)
		if err != nil {
			s.logger.Warn("console: held proposals for the needs strip", "error", err)
			return nil
		}
		n = len(held)
	}
	if n == 0 {
		return nil
	}
	sub := "read each one, then release or discard it"
	if n == 1 {
		sub = "read it, then release or discard it"
	}
	return []attnCard{{
		Kind: needHeld, Sev: "warn", Icon: "lock", Count: n, Blocking: true,
		Title: plural(n, "write held for your review", "writes held for your review"),
		Sub:   sub, Href: "/console/held",
	}}
}

// needLockDeclarations is the restricted accounts' lock declarations waiting
// for the owner (the repo-locks feature). One alone is approved inline.
func (s *Service) needLockDeclarations(ctx context.Context, next string) []attnCard {
	locks, err := store.ListRepoLocks(ctx, s.db(ctx))
	if err != nil {
		s.logger.Warn("console: lock declarations for the needs strip", "error", err)
		return nil
	}
	var waiting []core.RepoLock
	for _, l := range locks {
		if l.State == core.RepoLockRequested {
			waiting = append(waiting, l)
		}
	}
	if len(waiting) == 0 {
		return nil
	}
	card := attnCard{
		Kind: needLocks, Sev: "warn", Icon: "git-branch", Count: len(waiting), Blocking: true,
		Title: plural(len(waiting), "lock declaration waits for you", "lock declarations wait for you"),
		Href:  "/console/locks",
	}
	if len(waiting) > 1 {
		card.Sub = "approve or discard each on Locks"
		return []attnCard{card}
	}
	l := waiting[0]
	card.Sub = "LOCK " + l.Label()
	if l.Account != "" {
		card.Sub += " · declared by " + l.Account
	}
	card.Sub += " " + agoPhrase(l.CreatedAt)
	card.Href = "/console/locks/" + url.PathEscape(l.ID)
	card.Action = &needAction{
		Label: "Approve", Icon: "check", Post: "/console/locks/" + url.PathEscape(l.ID) + "/approve",
		Fields: []needField{{"next", next}},
		Title:  "Make the declaration an active lock, which agents may then hold",
	}
	return []attnCard{card}
}

// needOpenGates is the open gates: the gates and the steps whose own gate is
// open, that wait on the owner, as one item (one alone is marked as handled
// inline), then those waiting on anyone else, named. A step that waits only
// because a gate blocks it is counted through that gate.
func (s *Service) needOpenGates(ctx context.Context, next string) []attnCard {
	db := s.db(ctx)
	gates, err := store.AllGateTasks(ctx, db)
	if err != nil {
		s.logger.Warn("console: gates for the needs strip", "error", err)
		return nil
	}
	gated, err := store.AllGatedTasks(ctx, db)
	if err != nil {
		s.logger.Warn("console: gated steps for the needs strip", "error", err)
		return nil
	}
	var mine, others []core.Task
	for _, b := range slices.Concat(gates, gated) {
		t := b.Task
		// A gate is open until it closes; a step waits on its own gate only
		// while that gate is open.
		if !t.IsGate() && !t.Gate.Open() {
			continue
		}
		if ownerGateOpen(t) {
			mine = append(mine, t)
		} else {
			others = append(others, t)
		}
	}
	var out []attnCard
	if len(mine) > 0 {
		oldest := mine[0]
		for _, t := range mine[1:] {
			if t.CreatedAt.Before(oldest.CreatedAt) {
				oldest = t
			}
		}
		card := attnCard{
			Kind: needGates, Sev: "warn", Icon: "flag", Count: len(mine), Blocking: true,
			Title: plural(len(mine), "gate waits on you", "gates wait on you"),
			Sub:   "oldest: " + shownTitle(oldest.Title) + " · " + agoPhrase(oldest.CreatedAt),
			Href:  "/console/tasks",
		}
		if len(mine) == 1 {
			card.Sub = shownTitle(oldest.Title)
			if oldest.Gate.What != "" {
				card.Sub += ": " + oldest.Gate.What
			}
			card.Href = "/console/tasks/" + url.PathEscape(oldest.ID)
			card.Action = &needAction{
				Label: "Mark as handled", Icon: "check", Post: "/console/tasks/" + url.PathEscape(oldest.ID) + "/gate/clear",
				Fields: []needField{{"next", next}},
				Title:  "The gate is cleared: what waits on it goes ahead once nothing else blocks it",
			}
		}
		out = append(out, card)
	}
	if len(others) > 0 {
		card := attnCard{
			Kind: needGatesOthers, Sev: "info", Icon: "flag", Count: len(others), Blocking: true,
			Title: plural(len(others), "gate waits on ", "gates wait on ") + gateHolders(others),
			Sub:   "the work behind them waits until they are handled",
			Href:  "/console/tasks",
		}
		if len(others) == 1 {
			card.Sub = shownTitle(others[0].Title)
			card.Href = "/console/tasks/" + url.PathEscape(others[0].ID)
		}
		out = append(out, card)
	}
	return out
}

// gateHoldersNamed is how many holders the others card names before it
// counts the rest: the title is one line of a strip card, and a deployment
// with many gates on many people once named 14 there.
const gateHoldersNamed = 3

// gateHolders names who the gates wait on, for the others card's title: the
// holders with the most gates first, a name spelled two ways counted once
// (the first spelling wins), and past gateHoldersNamed the first two named
// and the rest counted ("dana, lee and 5 others").
func gateHolders(gates []core.Task) string {
	type holder struct {
		name string
		n    int
	}
	var hs []holder
	at := map[string]int{}
	for _, t := range gates {
		name := cmp.Or(strings.TrimSpace(t.Gate.Who), "people")
		key := strings.ToLower(name)
		if i, ok := at[key]; ok {
			hs[i].n++
			continue
		}
		at[key] = len(hs)
		hs = append(hs, holder{name: name, n: 1})
	}
	slices.SortStableFunc(hs, func(a, b holder) int { return cmp.Compare(b.n, a.n) })
	names := make([]string, 0, gateHoldersNamed)
	for _, h := range hs {
		names = append(names, h.name)
		if len(names) == gateHoldersNamed {
			break
		}
	}
	if len(hs) > gateHoldersNamed {
		rest := len(hs) - (gateHoldersNamed - 1)
		return strings.Join(names[:gateHoldersNamed-1], ", ") + " and " + strconv.Itoa(rest) + " others"
	}
	return approval.JoinAnd(names)
}

// needPlanApprovals is the captured Claude Code plans presented and awaiting
// the owner's approval (the board's count: plan-status presented). One alone
// is approved inline.
func (s *Service) needPlanApprovals(ctx context.Context, next string) []attnCard {
	captures, err := store.NotesByTag(ctx, s.db(ctx), "", plans.TagPlan)
	if err != nil {
		s.logger.Warn("console: plans awaiting approval for the needs strip", "error", err)
		return nil
	}
	var waiting []core.Note
	for _, n := range captures {
		if planAwaitsApproval(n) {
			waiting = append(waiting, n)
		}
	}
	if len(waiting) == 0 {
		return nil
	}
	card := attnCard{
		Kind: needPlans, Sev: "warn", Icon: "map", Count: len(waiting), Blocking: true,
		Title: plural(len(waiting), "plan awaits your approval", "plans await your approval"),
		Href:  "/console/plans",
	}
	if len(waiting) > 1 {
		var slugs []string
		for _, n := range waiting {
			slugs = append(slugs, "plan:"+plans.SlugFromTags(n.Tags))
		}
		card.Sub = approval.JoinAnd(slugs)
		return []attnCard{card}
	}
	n := waiting[0]
	slug := plans.SlugFromTags(n.Tags)
	card.Sub = "plan:" + slug
	if n.Project != "" {
		card.Sub += " in " + n.Project
	}
	card.Sub += " · presented " + agoPhrase(n.Updated)
	card.Href = planHref(n.Project, slug)
	card.Action = &needAction{
		Label: "Approve", Icon: "check", Post: planApproveHref(n.Project, slug),
		Fields: []needField{{"next", next}},
		Title:  "Approve the plan, as Claude Code's approval would, and create its tracking task",
	}
	return []attnCard{card}
}

// needClaims is the in-progress work no live agent carries, from Now's loose
// ends: the lapsed claims, then the claimless starts. Neither has a live
// holder to interrupt, so one alone is released inline without a confirm,
// Now's rule; a live claim whose holder went quiet is not an item.
func needClaims(loose []nowLooseVM, next string) []attnCard {
	var lapsed, claimless []nowLooseVM
	for _, l := range loose {
		switch l.State {
		case "lapsed":
			lapsed = append(lapsed, l)
		case "claimless":
			claimless = append(claimless, l)
		}
	}
	var out []attnCard
	build := func(rows []nowLooseVM, kind, sev, one, many, sub string) {
		if len(rows) == 0 {
			return
		}
		card := attnCard{
			Kind: kind, Sev: sev, Icon: "timer", Count: len(rows), Blocking: true,
			Title: plural(len(rows), one, many), Sub: sub, Href: "/console/now",
		}
		if len(rows) == 1 {
			l := rows[0]
			card.Sub = shownTitle(l.Title)
			if l.LapsedAt != nil {
				card.Sub += " · lapsed " + agoPhrase(*l.LapsedAt)
			}
			if l.Holder != "" {
				card.Sub += " · claimed by " + l.Holder
			}
			card.Href = "/console/tasks/" + url.PathEscape(l.ID)
			card.Action = &needAction{
				Label: "Release claim", Icon: "undo-2", Post: "/console/tasks/" + url.PathEscape(l.ID) + "/release",
				Fields: []needField{{"next", next}},
				Title:  "No live agent holds it: the task reopens for any agent to claim",
			}
		}
		out = append(out, card)
	}
	build(lapsed, needLapsed, "danger", "claim lapsed", "claims lapsed",
		"their holders' leases ran out mid-task: release them, or let an agent reclaim them")
	build(claimless, needUnclaimed, "warn", "task in progress with no claim", "tasks in progress with no claim",
		"marked in progress without a claim: release them so an agent can take them")
	return out
}

// needMishapReview is the agent-reported mishaps since the owner last marked
// them reviewed on the Mishaps screen (every report before the first review),
// at their true count. Reviewing them is reading them, so the item links
// there and its Mark reviewed clears it.
func (s *Service) needMishapReview(ctx context.Context) []attnCard {
	if s.cfg.Events == nil {
		return nil
	}
	n, at, reviewed, err := s.unreviewedMishaps(ctx, s.db(ctx))
	if err != nil {
		s.logger.Warn("console: mishaps for the needs strip", "error", err)
		return nil
	}
	if n == 0 {
		return nil
	}
	sub := ""
	if latest, lerr := s.recentMishaps(ctx, 1); lerr == nil && len(latest) > 0 {
		m := latest[0]
		where := "global scope"
		if m.Project != "" {
			where = m.Project
		}
		sub = "latest "
		if m.Type != "" {
			sub += m.Type + " "
		}
		sub += agoPhrase(m.When) + " in " + where + " · "
	}
	if reviewed {
		sub += "since you marked them reviewed " + agoPhrase(at)
	} else {
		sub += "review recurrence, then mark them reviewed"
	}
	return []attnCard{{
		Kind: needMishaps, Sev: "danger", Icon: "triangle-alert", Count: n,
		Title: plural(n, "mishap to review", "mishaps to review"),
		Sub:   sub, Href: mishapsPath,
	}}
}

// needUpdate is an update that failed, rolled back or paused itself: the
// trouble banner's words (updateAlertFor), built from parsed versions and
// fixed words only.
func (s *Service) needUpdate(now time.Time) []attnCard {
	if s.cfg.Updates == nil {
		return nil
	}
	a := updateAlertFor(s.cfg.Updates.Status(), now)
	if a == nil {
		return nil
	}
	sev := "warn"
	if a.Danger {
		sev = "danger"
	}
	return []attnCard{{
		Kind: needUpdates, Sev: sev, Icon: "refresh-cw", Count: 1,
		Title: strings.TrimSuffix(a.Head, "."), Sub: a.Line, Href: updatesSectionHref,
	}}
}

// needBackup is backups in trouble: the backup service stopped, automatic
// backups failing or overdue (the every-page alert's words), no backup at
// all, or a newest backup older than a week.
func (s *Service) needBackup(now time.Time) []attnCard {
	if s.cfg.Backups == nil {
		return nil
	}
	st := s.cfg.Backups.Status()
	card := attnCard{Kind: needBackups, Sev: "warn", Icon: "archive", Count: 1, Href: backupsSectionHref}
	switch {
	case st.Stopped:
		card.Sev, card.Title = "danger", "Backups stopped"
		card.Sub = "the backup service stopped after an internal error; Settings says what to do"
	case st.Alert(now):
		a := backupAlertFor(st, now)
		card.Title, card.Sub = strings.TrimSuffix(a.Head, "."), a.Line
		if st.Failures >= 2 {
			card.Sev = "danger"
		}
	case st.LastBackup == nil && st.Running == nil:
		card.Title = "No backup yet"
		card.Sub = "automatic backups are off: back up now, or turn them on in Settings"
		if st.Settings.Auto {
			card.Sub = "the first automatic backup has not finished yet"
		}
	case st.LastBackup != nil && now.Sub(st.LastBackup.At) > backupOldAfter:
		card.Title = "The newest backup is " + strconv.Itoa(int(now.Sub(st.LastBackup.At)/(24*time.Hour))) + " days old"
		card.Sub = "made " + agoPhrase(st.LastBackup.At) + "; automatic backups are off"
		if st.Settings.Auto {
			card.Sub = "made " + agoPhrase(st.LastBackup.At) + "; automatic backups run " + everyWords(st.Settings.Every)
		}
	default:
		return nil
	}
	return []attnCard{card}
}

// needKnowledge is the knowledge queue: gardener proposals, what automatic
// apply applied, memories going stale, and momentum's finish lines.
func (s *Service) needKnowledge(ctx context.Context, in needsInput) []attnCard {
	var out []attnCard
	if in.pending > 0 {
		sub := "waiting for review"
		// The oldest pending proposal is what makes the queue feel stale, and
		// PendingProposals is newest-first, so it is the last row. A failure here
		// costs the age line, not the card.
		if props, err := store.PendingProposals(ctx, s.db(ctx), ""); err != nil {
			s.logger.Warn("console: pending proposals for the needs strip", "error", err)
		} else if len(props) > 0 {
			sub = "oldest " + ago(props[len(props)-1].CreatedAt) + " · merges, retirements, and digests"
		}
		out = append(out, attnCard{
			Kind: needProposals, Sev: "warn", Icon: "sprout", Count: in.pending,
			Title: plural(in.pending, "gardener proposal waiting", "gardener proposals waiting"),
			Sub:   sub, Href: "/console/gardener",
		})
	}
	for _, c := range s.autoAppliedCards(ctx) {
		c.Kind = needAutoApplied
		out = append(out, c)
	}
	if in.staleUnseen > 0 {
		// The stale memories themselves: Retrieval's quiet-knowledge list (the
		// Memories library has no stale filter).
		out = append(out, attnCard{
			Kind: needStale, Sev: "info", Icon: "timer", Count: in.staleUnseen,
			Title: plural(in.staleUnseen, "memory going stale", "memories going stale"),
			Sub:   "no update, injection, or read in " + strconv.Itoa(in.staleDays) + "d · consider retiring",
			Href:  "/console/retrieval#retrieval-pressure-title",
		})
	}
	// Momentum's finish-line cards close the strip: the only positive entry, so
	// the problems keep the lead. The briefing's plan-line emphasis reads the
	// same rollup (store.PlanRollup), so an agent and the owner read one story.
	finish, err := store.FinishLinePlans(ctx, s.db(ctx))
	if err != nil {
		s.logger.Warn("console: finish-line plans for the needs strip", "error", err)
	}
	for _, p := range finish {
		out = append(out, attnCard{
			Kind: needFinishLine, Sev: "ok", Icon: "flag", Count: p.StepsLeft(),
			Title: p.Slug + ": " + p.FinishLinePhrase(),
			Sub:   "left: " + shownTitle(strings.Join(p.Remaining, " · ")),
			Href:  planHref(p.Project, p.Slug),
			Bar:   finishBar(p.PlanRollup),
		})
	}
	return out
}

// ownerNext resolves where an owner's action returns when its form named a
// next: that console page, carrying the action's one-shot flash (param is
// "notice" or "error"), a flash it already carried dropped and its fragment
// kept last. A next that is absent, or that safeNext refuses, returns
// fallback exactly as it is, so a route that adopts it answers every other
// caller as before. The task actions resolve theirs the same way (taskNext).
func ownerNext(raw, fallback, param, msg string) string {
	raw = strings.TrimSpace(raw)
	if raw == "" {
		return fallback
	}
	next := safeNext(raw)
	if next == "/console/" {
		return fallback
	}
	dest, frag, hasFrag := strings.Cut(next, "#")
	path, query, _ := strings.Cut(dest, "?")
	var kept []string
	for _, part := range strings.Split(query, "&") {
		if part == "" || strings.HasPrefix(part, "notice=") || strings.HasPrefix(part, "error=") {
			continue
		}
		kept = append(kept, part)
	}
	kept = append(kept, param+"="+url.QueryEscape(msg))
	out := path + "?" + strings.Join(kept, "&")
	if hasFrag {
		out += "#" + frag
	}
	return out
}
