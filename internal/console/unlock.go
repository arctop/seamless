package console

// Console unlocks: the owner opens a locked project in one
// browser's console for a limited time (approval.ConsoleLease, three hours).
//
// Two ways in, both begun on /console/unlock/<project> by the browser they
// open, and both ending in a live store.ConsoleUnlock row behind that
// browser's seamless_unlock cookie (HttpOnly, SameSite=Strict, Secure under
// TLS) and its browser key. A browser sends its cookies to every port of the
// host, so the row's hash covers the cookie and the key together
// (store.HashBrowserUnlock), and each request proves the key
// (browserproof; reach.go):
//
//   - a passkey: an assertion over a fresh challenge naming the project and
//     this browser (approval.BeginConsoleUnlock and FinishConsoleUnlock);
//   - the terminal: POST .../terminal records an unlock bound to this browser's
//     cookie and answers a pairing code, the page shows it with the command,
//     and `sudo -k -u <daemon account> seamlessd console-open --unlock <project>
//     --code <code>` approves it as the daemon's own account. The page asks GET
//     .../terminal?id= until it is live. Nothing the terminal shows is a
//     credential: the code only names the row, and no route lets a request
//     move a row to another browser's cookie.
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
	"github.com/arctop/seamless/internal/console/browserproof"
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
	// browser, so only a browser is offered one, passkey or terminal.
	Browser bool `json:"-"`
	// TerminalMinutes is how long a code the terminal path shows waits for
	// its approval.
	TerminalMinutes int `json:"-"`
	// Item is the kind of item a locked page sent the owner here for (a plan
	// or a session: lockedItem), Next its page, to open once unlocked; both
	// empty otherwise.
	Item string `json:"-"`
	Next string `json:"-"`
	// ForceLocked says the config file holds this lock; ConsoleLocked says a
	// lock set from the console does, and the page links to its removal.
	ForceLocked   bool `json:"force_locked"`
	ConsoleLocked bool `json:"console_locked"`
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

// unlockCommand is the terminal's half of a terminal unlock: console-open as
// the daemon's own account, through sudo -k so it asks every time, approving
// the code this browser's page shows.
func (s *Service) unlockCommand(project, code string) string {
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
	return fmt.Sprintf("sudo -k -u %s %s%s console-open --unlock %s --code %s", account, env, seamlessd, shellWord(project), shellWord(code))
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
	d := unlockData{
		Project: tallies[0], Lease: leaseWord(s.cfg.Approvals.ConsoleLeaseLength()), PasskeysOn: s.cfg.Approvals.PasskeysOn(),
		Browser: s.classify(r) == credCookie, TerminalMinutes: int(approval.ConsoleTerminalUnlockTTL / time.Minute),
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
	d.ForceLocked = slices.Contains(s.cfg.ForceLocked, slug)
	if d.ConsoleLocked, err = store.HasConsoleLock(ctx, s.writeDB(), slug); err != nil {
		return unlockData{}, err
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

// browserKeyOf returns the key this request's proof verifies under, or writes
// the refusal and reports false. Every unlock route needs it: the unlock's
// hash covers it (store.HashBrowserUnlock), and a page on another port of this
// host receives the cookie but can never make the proof. The console's page
// sets the key up before it begins (static/browserkey.js), and its service
// worker signs the requests.
func (s *Service) browserKeyOf(w http.ResponseWriter, r *http.Request) (string, bool) {
	rc := reachOf(r.Context())
	if rc != nil && rc.key != "" {
		return rc.key, true
	}
	msg := "this browser sent no proof of its unlock key: reload this page and start again"
	if rc != nil && errors.Is(rc.keyErr, browserproof.ErrClock) {
		msg = fmt.Sprintf("this browser's clock is more than %s off the daemon's: correct it, then start again", leaseWord(browserproof.Window))
	}
	writeJSON(w, http.StatusConflict, map[string]string{"error": msg})
	return "", false
}

// browserToken returns this browser's unlock secret, and the hash an unlock
// stores of it with key: its cookie when cookie and key already name a live
// unlock, else a fresh one. A cookie that names no live unlock is never
// adopted -- a page on another local port can set a cookie for this host, and
// its value would then be one the page knows.
func (s *Service) browserToken(r *http.Request, key string) (string, string, bool, error) {
	if rc := reachOf(r.Context()); rc != nil && rc.token != "" {
		if c := unlockCookieValue(r); c != "" && store.HashBrowserUnlock(c, key) == rc.token {
			return c, rc.token, false, nil
		}
	}
	token, err := store.NewConsoleToken()
	return token, store.HashBrowserUnlock(token, key), true, err
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
	key, ok := s.browserKeyOf(w, r)
	if !ok {
		return
	}
	token, hash, fresh, err := s.browserToken(r, key)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	begun, err := s.cfg.Approvals.BeginConsoleUnlock(r.Context(), slug, hash)
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
	cookie := unlockCookieValue(r)
	if cookie == "" {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "this browser holds no unlock in progress; start again"})
		return
	}
	key, ok := s.browserKeyOf(w, r)
	if !ok {
		return
	}
	hash := store.HashBrowserUnlock(cookie, key)
	u, err := s.cfg.Approvals.FinishConsoleUnlock(r.Context(), r.PostForm.Get("id"), slug, hash, []byte(r.PostForm.Get("response")))
	if err != nil {
		s.unlockError(w, r, err)
		return
	}
	s.keepUnlockCookie(w, r, cookie, hash)
	writeJSON(w, http.StatusOK, map[string]any{
		"unlocked": u.Project, "expires_at": u.ExpiresAt.UTC().Format(time.RFC3339), "open": "/console/projects/" + url.PathEscape(u.Project),
	})
}

// keepUnlockCookie re-issues the cookie value to last as long as this
// browser's latest live unlock, the rows under hash.
func (s *Service) keepUnlockCookie(w http.ResponseWriter, r *http.Request, value, hash string) {
	now := time.Now()
	last := now.Add(s.cfg.Approvals.ConsoleLeaseLength())
	if rows, err := store.LiveConsoleUnlocks(r.Context(), s.writeDB(), hash, now); err == nil {
		for _, u := range rows {
			if u.ExpiresAt != nil && u.ExpiresAt.After(last) {
				last = *u.ExpiresAt
			}
		}
	}
	s.unlockCookie(w, value, last.Sub(now))
}

// The states of a terminal unlock, as the page waiting on it reads them.
const (
	terminalPending = "pending" // its code waits for the terminal
	terminalLive    = "live"    // approved: this browser reads the project
	terminalExpired = "expired" // its code's deadline passed unapproved, or it is not this browser's
	terminalEnded   = "ended"   // locked again, or reset, before or after it went live
)

// unlockTerminalBegin starts a terminal unlock: a row bound to this browser's
// cookie (set here when the browser holds no live unlock yet), and the pairing
// code and the command the page shows. Its gates are the passkey begin's: a
// locked project, a browser (never the TCP key or the socket), the console's
// origin check (auth, which also keeps a restricted account out), and a
// pending cap, the terminal unlocks' own, so a key holder filling it leaves
// the passkey path open.
func (s *Service) unlockTerminalBegin(w http.ResponseWriter, r *http.Request) {
	slug, ok := s.unlockProject(w, r)
	if !ok || !s.browserOnly(w, r) {
		return
	}
	key, ok := s.browserKeyOf(w, r)
	if !ok {
		return
	}
	token, hash, fresh, err := s.browserToken(r, key)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	begun, err := s.cfg.Approvals.BeginTerminalUnlock(r.Context(), slug, hash)
	if err != nil {
		s.unlockError(w, r, err)
		return
	}
	if fresh {
		s.unlockCookie(w, token, approval.ConsoleTerminalUnlockTTL+s.cfg.Approvals.ConsoleLeaseLength())
	}
	writeJSON(w, http.StatusOK, map[string]any{
		"id": begun.ID, "code": begun.Code, "command": s.unlockCommand(slug, begun.Code),
		"pending_until": begun.PendingUntil.UTC().Format(time.RFC3339), "minutes": int(approval.ConsoleTerminalUnlockTTL / time.Minute),
	})
}

// unlockTerminalStatus answers the page waiting on a terminal approval with
// the state of the unlock it began (?id=). It reads the row through this
// browser's cookie and key, so another browser, the TCP key and the socket get the
// answer an id that never existed gets. Once live it re-issues the cookie to
// last as long as the unlock, as a passkey's finish does.
func (s *Service) unlockTerminalStatus(w http.ResponseWriter, r *http.Request) {
	slug, ok := s.unlockProject(w, r)
	if !ok || !s.browserOnly(w, r) {
		return
	}
	gone := func() {
		writeJSON(w, http.StatusNotFound, map[string]string{"state": terminalExpired,
			"error": "no unlock begun in this browser is waiting: start again for a new code"})
	}
	cookie := unlockCookieValue(r)
	if cookie == "" {
		gone()
		return
	}
	key, ok := s.browserKeyOf(w, r)
	if !ok {
		return
	}
	hash := store.HashBrowserUnlock(cookie, key)
	u, err := store.ConsoleUnlockFor(r.Context(), s.writeDB(), r.URL.Query().Get("id"), slug, hash)
	if errors.Is(err, store.ErrConsoleUnlockNotFound) {
		gone()
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	now := time.Now()
	switch {
	case u.Status == store.ConsoleUnlockPending && u.PendingUntil.After(now):
		writeJSON(w, http.StatusOK, map[string]any{"state": terminalPending, "pending_until": u.PendingUntil.UTC().Format(time.RFC3339)})
	case u.Status == store.ConsoleUnlockLive && u.ExpiresAt != nil && u.ExpiresAt.After(now):
		s.keepUnlockCookie(w, r, cookie, hash)
		writeJSON(w, http.StatusOK, map[string]any{"state": terminalLive, "unlocked": u.Project,
			"expires_at": u.ExpiresAt.UTC().Format(time.RFC3339), "open": "/console/projects/" + url.PathEscape(u.Project)})
	case u.Status == store.ConsoleUnlockPending:
		writeJSON(w, http.StatusOK, map[string]string{"state": terminalExpired})
	default:
		writeJSON(w, http.StatusOK, map[string]string{"state": terminalEnded})
	}
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
		msg = "no passkey is enrolled: enroll one on the Approvers page, or approve this unlock from a terminal"
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
