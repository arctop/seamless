package console

// Console unlocks (peer_auth.locked): the owner opens a locked project in one
// browser's console for a limited time (approval.ConsoleLease, three hours).
//
// Two ways in, both ending in a live store.ConsoleUnlock row behind the
// browser's seamless_unlock cookie (HttpOnly, SameSite=Strict, Secure under
// TLS):
//
//   - a passkey, at /console/unlock/<project>: an assertion over a fresh
//     challenge naming the project and this browser (approval.BeginConsoleUnlock
//     and FinishConsoleUnlock);
//   - `sudo -k -u <daemon account> seamlessd console-open --unlock <project>`,
//     which writes a one-time link straight into the database as the daemon's
//     own account; GET /console/unlock?t=<secret> claims it.
//
// The live leases agents hold, and every browser's unlocks, are listed on the
// access page with Revoke and Lock again. Neither a revoke nor a lock-again may
// come from an agent's socket connection: the owner's browser or the TCP key
// only, like a deny. Every route here is a 404 unless a project is locked.

import (
	"errors"
	"fmt"
	"html/template"
	"net/http"
	"net/url"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/passkey"
	"github.com/arctop/seamless/internal/store"
)

// unlockData is the unlock page: a locked project's name and counts, this
// browser's unlock of it, and the two ways to unlock it.
type unlockData struct {
	Project store.ProjectTally `json:"project"`
	// Unlocked is whether this browser reads the project now; UnlockID and
	// Until name that unlock, for Lock again.
	Unlocked bool       `json:"unlocked"`
	UnlockID string     `json:"unlock_id,omitempty"`
	Until    *time.Time `json:"until,omitempty"`
	// Leased is whether this request reads the project through an agent
	// tree's lease (a seam command on the socket).
	Leased bool   `json:"leased"`
	Lease  string `json:"lease"`
	// PasskeysOn, HasPasskey and OnOrigin decide whether the passkey button
	// can work here; Origin is where it can.
	PasskeysOn bool   `json:"passkeys_on"`
	HasPasskey bool   `json:"has_passkey"`
	OnOrigin   bool   `json:"-"`
	Origin     string `json:"-"`
	// Browser is false for a bearer or socket caller: an unlock is per
	// browser, so only a browser is offered one.
	Browser bool `json:"-"`
	// Command is the terminal way in, run in the owner's own terminal.
	Command string `json:"command"`
	// Claimed marks the answer to an unlock link: the page then replaces the
	// link in the address bar with the project's unlock page, so a reload or
	// a history entry never holds the used secret.
	Claimed bool `json:"-"`
	// Item is the kind of item a locked page sent the owner here for (a plan
	// or a session: lockedItem), Next its page, to open once unlocked; both
	// empty otherwise.
	Item string `json:"-"`
	Next string `json:"-"`
}

// unlockProject answers the routes that name a project: it must be one
// peer_auth locks. It writes the 404 and reports false otherwise.
func (s *Service) unlockProject(w http.ResponseWriter, r *http.Request) (string, bool) {
	if s.approversOff(w, r) {
		return "", false
	}
	slug := r.PathValue("project")
	if s.cfg.Views == nil || !slices.Contains(s.cfg.Views.Lockable(), slug) {
		s.notFound(w, r, "project "+slug+" is not locked: there is nothing to unlock")
		return "", false
	}
	return slug, true
}

// leaseWord renders a lease length for the owner: "3 hours", "90 minutes".
func leaseWord(d time.Duration) string {
	if d >= time.Hour && d%time.Hour == 0 {
		return plural(int(d/time.Hour), "hour", "hours")
	}
	return plural(int(d.Round(time.Minute)/time.Minute), "minute", "minutes")
}

// shellWord quotes s for a POSIX shell when it needs it, for a command the
// page shows the owner to copy.
func shellWord(s string) string {
	if s != "" && strings.IndexFunc(s, func(r rune) bool {
		return !(r >= 'a' && r <= 'z' || r >= 'A' && r <= 'Z' || r >= '0' && r <= '9' || strings.ContainsRune("_./:=@-+,", r))
	}) < 0 {
		return s
	}
	return "'" + strings.ReplaceAll(s, "'", `'\''`) + "'"
}

// unlockCommand is the terminal way to unlock project: console-open as the
// daemon's own account, through sudo -k so it asks every time.
func (s *Service) unlockCommand(project string, lease time.Duration) string {
	account, seamlessd, env := "<daemon account>", "seamlessd", ""
	if t := s.cfg.Terminal; t != nil {
		if t.Account != "" {
			account = shellWord(t.Account)
		}
		if t.Seamlessd != "" {
			seamlessd = shellWord(t.Seamlessd)
		}
		if t.Config != "" {
			env = "env SEAMLESS_CONFIG=" + shellWord(t.Config) + " "
		}
	}
	return fmt.Sprintf("sudo -k -u %s %s%s console-open --unlock %s --for %s", account, env, seamlessd, shellWord(project), flagDuration(lease))
}

// flagDuration renders d as a --for value a person would type: "3h", "90m".
func flagDuration(d time.Duration) string {
	switch {
	case d > 0 && d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d > 0 && d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	default:
		return d.String()
	}
}

func (s *Service) unlockPage(w http.ResponseWriter, r *http.Request) {
	slug, ok := s.unlockProject(w, r)
	if !ok {
		return
	}
	d, err := s.unlockDataFor(r, slug)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if item := r.URL.Query().Get("item"); item == lockedItemPlan || item == lockedItemSession {
		d.Item = item
		if next := safeNext(r.URL.Query().Get("next")); next != "/console/" {
			d.Next = next
		}
	}
	s.render(w, r, "unlock", pageData{Title: "Unlock " + d.Project.Name, Active: "projects", Data: d})
}

// unlockDataFor assembles the unlock page for slug as this request sees it.
func (s *Service) unlockDataFor(r *http.Request, slug string) (unlockData, error) {
	ctx := r.Context()
	tallies, err := store.ProjectTallies(ctx, s.writeDB(), []string{slug})
	if err != nil {
		return unlockData{}, err
	}
	lease := s.cfg.Approvals.ConsoleLeaseLength()
	d := unlockData{
		Project: tallies[0], Lease: leaseWord(lease), PasskeysOn: s.cfg.Approvals.PasskeysOn(),
		Browser: s.classify(r) == credCookie, Command: s.unlockCommand(slug, lease),
	}
	if rc := reachOf(ctx); rc != nil {
		d.Leased = slices.Contains(rc.open, slug) && !slices.Contains(rc.unlocked, slug)
		if rc.token != "" && slices.Contains(rc.unlocked, slug) {
			rows, err := store.LiveConsoleUnlocks(ctx, s.writeDB(), rc.token, time.Now())
			if err != nil {
				return unlockData{}, err
			}
			for _, u := range rows {
				if u.Project == slug && (d.Until == nil || u.ExpiresAt.After(*d.Until)) {
					d.Unlocked, d.UnlockID, d.Until = true, u.ID, u.ExpiresAt
				}
			}
		}
	}
	creds, err := store.ApproverCredentials(ctx, s.db(ctx), store.ApproverPasskey)
	if err != nil {
		return unlockData{}, err
	}
	d.HasPasskey = len(creds) > 0
	if origins := s.cfg.Approvals.Origins(); len(origins) > 0 {
		d.Origin = origins[0]
		d.OnOrigin = slices.Contains(origins, requestOrigin(r))
	}
	return d, nil
}

// unlockCookie writes the browser's seamless_unlock cookie, valid for maxAge.
func (s *Service) unlockCookie(w http.ResponseWriter, value string, maxAge time.Duration) {
	http.SetCookie(w, &http.Cookie{
		Name:     unlockCookieName,
		Value:    value,
		Path:     "/console",
		HttpOnly: true,
		Secure:   s.cfg.SecureCookies,
		SameSite: http.SameSiteStrictMode,
		MaxAge:   max(1, int(maxAge/time.Second)),
	})
}

// browserToken returns this browser's unlock secret: its cookie when that
// already names a live unlock, else a fresh one. A cookie that names no live
// unlock is never adopted -- a page on another local port can set a cookie
// for this host, and its value would then be one the page knows.
func (s *Service) browserToken(r *http.Request) (string, bool, error) {
	if rc := reachOf(r.Context()); rc != nil && rc.token != "" {
		if c, err := r.Cookie(unlockCookieName); err == nil && store.HashConsoleToken(c.Value) == rc.token {
			return c.Value, false, nil
		}
	}
	token, err := store.NewConsoleToken()
	return token, true, err
}

// browserOnly refuses a caller that is not a browser: an unlock is per
// browser, and the bearer key and the socket are not one.
func (s *Service) browserOnly(w http.ResponseWriter, r *http.Request) bool {
	if s.classify(r) == credCookie {
		return true
	}
	writeJSON(w, http.StatusForbidden, map[string]string{"error": "a console unlock is per browser: open this page in the browser you sign in to the console with"})
	return false
}

// unlockBegin starts the passkey ceremony: a fresh challenge bound to the
// project and to this browser's cookie, which it sets when the browser holds
// no live unlock yet.
func (s *Service) unlockBegin(w http.ResponseWriter, r *http.Request) {
	slug, ok := s.unlockProject(w, r)
	if !ok || !s.browserOnly(w, r) {
		return
	}
	token, fresh, err := s.browserToken(r)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	begun, err := s.cfg.Approvals.BeginConsoleUnlock(r.Context(), slug, store.HashConsoleToken(token))
	if err != nil {
		s.unlockError(w, r, err)
		return
	}
	if fresh {
		s.unlockCookie(w, token, approval.ConsoleUnlockTTL+s.cfg.Approvals.ConsoleLeaseLength())
	}
	writeJSON(w, http.StatusOK, begun)
}

// unlockFinish verifies the assertion and makes the unlock live.
func (s *Service) unlockFinish(w http.ResponseWriter, r *http.Request) {
	slug, ok := s.unlockProject(w, r)
	if !ok || !s.browserOnly(w, r) {
		return
	}
	c, err := r.Cookie(unlockCookieName)
	if err != nil || c.Value == "" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this browser holds no unlock in progress; start again"})
		return
	}
	u, err := s.cfg.Approvals.FinishConsoleUnlock(r.Context(), r.PostForm.Get("id"), slug, store.HashConsoleToken(c.Value), []byte(r.PostForm.Get("response")))
	if err != nil {
		s.unlockError(w, r, err)
		return
	}
	s.keepUnlockCookie(w, r, c.Value)
	writeJSON(w, http.StatusOK, map[string]any{
		"unlocked": u.Project, "expires_at": u.ExpiresAt.UTC().Format(time.RFC3339), "open": "/console/projects/" + url.PathEscape(u.Project),
	})
}

// keepUnlockCookie re-issues the cookie to last as long as this browser's
// latest live unlock.
func (s *Service) keepUnlockCookie(w http.ResponseWriter, r *http.Request, value string) {
	now := time.Now()
	last := now.Add(s.cfg.Approvals.ConsoleLeaseLength())
	if rows, err := store.LiveConsoleUnlocks(r.Context(), s.writeDB(), store.HashConsoleToken(value), now); err == nil {
		for _, u := range rows {
			if u.ExpiresAt != nil && u.ExpiresAt.After(last) {
				last = *u.ExpiresAt
			}
		}
	}
	s.unlockCookie(w, value, last.Sub(now))
}

// unlockClaim claims a one-time link `seamlessd console-open --unlock` wrote.
func (s *Service) unlockClaim(w http.ResponseWriter, r *http.Request) {
	if s.approversOff(w, r) {
		return
	}
	if s.classify(r) != credCookie {
		s.badRequest(w, r, "an unlock link is for a browser: open it in the browser you sign in to the console with")
		return
	}
	secret := strings.TrimSpace(r.URL.Query().Get("t"))
	if secret == "" {
		s.badRequest(w, r, "this unlock link is incomplete: copy the whole link console-open printed")
		return
	}
	token, _, err := s.browserToken(r)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	u, err := store.ClaimConsoleUnlockLink(r.Context(), s.writeDB(), secret, store.HashConsoleToken(token), time.Now())
	if errors.Is(err, store.ErrConsoleUnlockNotFound) {
		s.notFound(w, r, "this unlock link was already used, or it expired: run console-open --unlock again")
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.keepUnlockCookie(w, r, token)
	if log := s.eventLog(r.Context()); log != nil {
		if _, err := log.Record(r.Context(), core.Event{Kind: approval.EventConsoleUnlocked, ItemID: u.ID, ProjectSlug: u.Project,
			Payload: map[string]any{"proof": approval.ProofDaemon, "seconds": int64(u.Lease / time.Second)}}); err != nil {
			s.logger.Warn("console: record console unlock", "error", err)
		}
	}
	// The answer is the unlock page itself, built from the row just claimed,
	// rather than a redirect: this request was judged before the cookie
	// existed, and a SameSite=Strict cookie set during a navigation another
	// site started is not sent on the redirect that follows it. The page's
	// links are same-site, so the next click carries it.
	if !slices.Contains(s.cfg.Views.Lockable(), u.Project) {
		s.notFound(w, r, "project "+u.Project+" is no longer locked: there is nothing to unlock")
		return
	}
	d, err := s.unlockDataFor(r, u.Project)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d.Unlocked, d.UnlockID, d.Until, d.Leased, d.Claimed = true, u.ID, u.ExpiresAt, false, true
	s.render(w, r, "unlock", pageData{Title: "Unlock " + d.Project.Name, Active: "projects", Data: d})
}

// ownerDecision refuses an agent's socket connection: revoking a lease and
// locking an unlock again are the owner's, from the browser or the TCP key.
func ownerDecision(w http.ResponseWriter, r *http.Request) bool {
	if access.PeerAuthenticated(r.Context()) {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "revoke from the console, or with the TCP key in the owner's terminal"})
		return false
	}
	return true
}

// ownerHands refuses, for next, a request from a process an agent started: the
// decision is the owner's, from their own terminal or the browser. It stops an
// agent's ordinary tool calls and is not a wall (access.FromAgent says why).
func ownerHands(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if refusedFromAgent(w, r) {
			return
		}
		next(w, r)
	}
}

// refusedFromAgent answers a request from a process an agent started, and
// reports that it did.
func refusedFromAgent(w http.ResponseWriter, r *http.Request) bool {
	if !access.FromAgent(r.Context()) {
		return false
	}
	writeJSON(w, http.StatusForbidden, map[string]string{"error": "this is the owner's decision, and this request came from a process an agent started: run it in your own terminal, or on the console"})
	return true
}

// afterAccessWrite answers an access-page write: JSON for a script, else the
// page the form named (the access page by default) with a notice.
func afterAccessWrite(w http.ResponseWriter, r *http.Request, payload map[string]any, notice string) {
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, payload)
		return
	}
	dest := "/console/access"
	if next := safeNext(strings.TrimSpace(r.PostFormValue("next"))); next != "/console/" {
		dest = next
	}
	sep := "?"
	if strings.Contains(dest, "?") {
		sep = "&"
	}
	http.Redirect(w, r, dest+sep+"notice="+url.QueryEscape(notice), http.StatusSeeOther)
}

// unlockEnd locks a console unlock again: any browser's, by id.
func (s *Service) unlockEnd(w http.ResponseWriter, r *http.Request) {
	if s.approversOff(w, r) || !ownerDecision(w, r) {
		return
	}
	u, err := store.EndConsoleUnlock(r.Context(), s.writeDB(), r.PathValue("id"), store.ConsoleUnlockEndLocked, time.Now())
	if errors.Is(err, store.ErrConsoleUnlockNotLive) {
		s.notFound(w, r, "that unlock already ended")
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	afterAccessWrite(w, r, map[string]any{"locked": u.ID, "project": u.Project}, u.Project+" is locked again in that browser.")
}

// leaseRevoke ends an agent tree's lease before its expiry.
func (s *Service) leaseRevoke(w http.ResponseWriter, r *http.Request) {
	if s.approversOff(w, r) || !ownerDecision(w, r) {
		return
	}
	ctx := r.Context()
	l, err := store.AccessLeaseByID(ctx, s.writeDB(), r.PathValue("id"))
	if err == nil {
		err = store.RevokeAccessLease(ctx, s.writeDB(), l.ID, store.LeaseRevokedOwner, time.Now())
	}
	if errors.Is(err, store.ErrLeaseNotLive) {
		s.notFound(w, r, "that lease already ended")
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if log := s.eventLog(ctx); log != nil {
		if _, err := log.Record(ctx, core.Event{Kind: core.EventAccessRevoked, SessionID: l.SessionID, ProjectSlug: l.Project,
			ItemID: l.ApprovalID, Payload: map[string]any{"reason": store.LeaseRevokedOwner, "leases": 1, "lease": l.ID}}); err != nil {
			s.logger.Warn("console: record lease revoke", "error", err)
		}
	}
	afterAccessWrite(w, r, map[string]any{"revoked": l.ID, "project": l.Project}, "Revoked the lease on "+l.Project+".")
}

// unlockError answers a ceremony step with a JSON error the page shows.
func (s *Service) unlockError(w http.ResponseWriter, r *http.Request, err error) {
	msg := ""
	switch {
	case errors.Is(err, store.ErrConsoleUnlockNotFound):
		msg = "this unlock expired, or it was begun in another browser; start again"
	case errors.Is(err, store.ErrConsoleUnlockBusy):
		msg = "too many unlocks are in progress; try again in a few minutes"
	case errors.Is(err, approval.ErrNoPasskey):
		msg = "no passkey is enrolled: enroll one on the Approvers page, or unlock in your own terminal"
	case errors.Is(err, passkey.ErrUnknownCredential):
		msg = "that passkey is not enrolled here"
	}
	if msg != "" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": msg})
		return
	}
	s.approverError(w, r, err)
}

// The kinds of item a locked page names (lockedItem, the unlock page's item=).
const (
	lockedItemPlan    = "plan"
	lockedItemSession = "session"
)

// lockedItem answers a browser page for an item -- a plan, a session -- that
// lives in project, a locked project this request does not read (owner
// decision 2026-11-08): the lock, named, with the way in, rather than a 404
// that would send the owner looking for something that exists. A full page
// goes to the project's unlock page, which says the item is in it and, once
// unlocked, links back (?item=, ?next=); a fragment (the peek, the reader)
// says so in place. Only the owner reaches the console, and only the item's
// kind and the project's slug are named: nothing from inside the project.
func (s *Service) lockedItem(w http.ResponseWriter, r *http.Request, project, item string) {
	unlock := "/console/unlock/" + url.PathEscape(project)
	if q := r.URL.Query(); q.Get("peek") == "1" || q.Get("reader") == "1" {
		w.Header().Set("Content-Type", "text/html; charset=utf-8")
		w.WriteHeader(http.StatusForbidden)
		esc := template.HTMLEscapeString
		fmt.Fprintf(w, `<article class="peek-entity" data-locked-item="%s"><div class="peek-head"><span class="badge">%s</span></div>`+
			`<h2 class="peek-title">Locked</h2><p class="peek-desc">This %s is in locked project %s.</p>`+
			`<p class="peek-note"><a href="%s">Unlock %s in the console</a> to read it.</p></article>`,
			esc(item), esc(project), esc(item), esc(project), esc(unlock), esc(project))
		return
	}
	http.Redirect(w, r, unlock+"?item="+url.QueryEscape(item)+"&next="+url.QueryEscape(r.URL.Path), http.StatusSeeOther)
}

// lockedProject answers a locked project's workspace for a request that does
// not read it: a browser goes to its unlock page (name, counts, the ways in);
// a script or a peek gets the refusal, naming the lock.
func (s *Service) lockedProject(w http.ResponseWriter, r *http.Request, slug string) {
	msg := "project " + slug + " is locked: unlock it in the console (" + "/console/unlock/" + slug + ") to read it"
	switch {
	case wantsJSON(r):
		writeJSON(w, http.StatusForbidden, map[string]any{"error": msg, "locked": true, "unlock": "/console/unlock/" + slug})
	case r.URL.Query().Get("peek") == "1":
		s.renderErrorPage(w, r, http.StatusForbidden, "Locked", msg)
	default:
		http.Redirect(w, r, "/console/unlock/"+url.PathEscape(slug), http.StatusSeeOther)
	}
}
