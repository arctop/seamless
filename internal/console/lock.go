package console

// The console's lock: the owner turns a project's lock on, or takes off a lock
// set here, from a browser. Each change needs a passkey the owner has already
// enrolled (approval.BeginLockChange and FinishLockChange), because the bearer
// key is no proof of the owner. A project peer_auth.force_locked lists is shown
// as force locked and cannot be changed here.
//
// The locked projects are read once at startup, so a stored change applies when
// the daemon restarts, which it then does at once (Config.RequestRestart). Every
// route here is a 404 with peer_auth off.

import (
	"errors"
	"net/http"
	"slices"

	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/passkey"
	"github.com/arctop/seamless/internal/store"
)

// lockView is a project's lock as a console page shows it.
type lockView struct {
	Slug string `json:"project"`
	Name string `json:"name"`
	// Force says peer_auth.force_locked lists it; Console says a lock set here
	// holds it. Both can be true: then only the file can open it.
	Force   bool `json:"force_locked"`
	Console bool `json:"console_locked"`
	// Active says the running daemon enforces a lock on it.
	Active bool `json:"active"`
	// Fenced says it is confidential or sealed, which a lock requires.
	Fenced bool `json:"fenced"`
	// PasskeysOn, HasPasskey and OnOrigin decide whether a change can be made
	// here; Origin is where it can.
	PasskeysOn bool   `json:"passkeys_on"`
	HasPasskey bool   `json:"has_passkey"`
	OnOrigin   bool   `json:"-"`
	Origin     string `json:"-"`
	// Browser is false for a bearer or socket caller: a passkey ceremony needs
	// a browser.
	Browser bool `json:"-"`
	// CanRestart is false when this daemon cannot apply a change by itself.
	CanRestart bool `json:"-"`
	// Isolation is the project's fence: open, confidential or sealed.
	Isolation core.Isolation `json:"isolation"`
	// Rung is where the project stands on the lock page's ladder of Open, the
	// fence and Locked ("open", "confidential", "sealed" or "locked", as
	// stored: a change that waits for the restart already counts), and RungTo
	// where the change the page offers takes it; empty when it offers none (a
	// force lock, or no fence to lock on).
	Rung   string `json:"rung"`
	RungTo string `json:"rung_to,omitempty"`
}

// The lock page ladder's top rung; the others are the isolation states.
const rungLocked = "locked"

// rungs places a project on the lock page's ladder: where it stands and where
// the change offered takes it.
func rungs(force, console bool, isolation core.Isolation) (here, to string) {
	switch {
	case force:
		return rungLocked, ""
	case console:
		return rungLocked, string(isolation)
	case !isolation.FencesOutbound():
		return string(core.IsolationOpen), ""
	default:
		return string(isolation), rungLocked
	}
}

// Locked reports whether a lock holds the project, or will from the next start.
func (v *lockView) Locked() bool { return v != nil && (v.Force || v.Console) }

// doorRung is one rung of the lock page's ladder.
type doorRung struct {
	Key, Label, Sub, Icon string
	// Here marks where the project stands; To where the change offered
	// takes it.
	Here, To bool
}

// Ladder is the lock page's fence ladder, Open, the fence (Confidential, or
// Sealed for a sealed project) and Locked, with the project's rung and the
// change's marked.
func (v *lockView) Ladder() []doorRung {
	fence := doorRung{Key: string(core.IsolationConfidential), Label: "Confidential", Sub: "nothing leaves it", Icon: "shield"}
	if v.Isolation == core.IsolationSealed {
		fence = doorRung{Key: string(core.IsolationSealed), Label: "Sealed", Sub: "nothing in or out", Icon: "shield"}
	}
	out := []doorRung{
		{Key: string(core.IsolationOpen), Label: "Open", Sub: "shares with your open projects", Icon: "globe"},
		fence,
		{Key: rungLocked, Label: "Locked", Sub: "every agent asks you first", Icon: "lock"},
	}
	for i := range out {
		out[i].Here, out[i].To = out[i].Key == v.Rung, out[i].Key == v.RungTo
	}
	return out
}

// fenceWord is the project's fence in a sentence: "confidential" or "sealed".
func (v *lockView) fenceWord() string {
	if v.Isolation == core.IsolationSealed {
		return string(core.IsolationSealed)
	}
	return string(core.IsolationConfidential)
}

// Question is the lock page's title: the change it offers, asked, or the
// force lock it cannot change, said.
func (v *lockView) Question() string {
	switch {
	case v.Force:
		return v.Name + " is locked"
	case v.Console:
		return "Remove the lock on " + v.Name + "?"
	default:
		return "Lock " + v.Name + "?"
	}
}

// Lead is the one line under the title.
func (v *lockView) Lead() string {
	switch {
	case v.Force:
		return "Locked in the daemon's config file."
	case v.Console:
		return "Locked from this console. Its agents would reach it again without asking you."
	case !v.Fenced:
		return "It is open, so there is no fence for a lock to sit on yet."
	default:
		return "Close it to everyone until you say so."
	}
}

// WhatAgents and WhatConsole are what the change offered does to the
// project's own agents and to this console.
func (v *lockView) WhatAgents() string {
	if v.Console {
		return "Its own agents reach it again, inside its " + v.fenceWord() + " fence."
	}
	return "Its own agents ask you before they read or write it."
}

func (v *lockView) WhatConsole() string {
	if v.Console {
		return "This console shows its content again without an unlock."
	}
	return "This console shows only its name and counts until you unlock it in a browser."
}

// lockStep is one step of a lock change once the passkey approved it, as the
// kit's "steps" partial draws it; the page's script moves them on.
type lockStep struct{ Label, State string }

// Steps are a lock change's steps after the passkey, all still to come.
func (v *lockView) Steps() []lockStep {
	last := "Locked, and this page refreshes"
	if v.Console {
		last = "Lock removed, and this page refreshes"
	}
	return []lockStep{{"Approved with your passkey", "todo"}, {"Restarting Seamless", "todo"}, {last, "todo"}}
}

// Action is the change this page offers: remove for a lock set here, else lock.
func (v *lockView) Action() approval.LockAction {
	if v.Console {
		return approval.LockActionRemove
	}
	return approval.LockActionAdd
}

// Pending reports a stored change the running daemon does not enforce yet.
func (v *lockView) Pending() bool { return !v.Force && v.Console != v.Active }

// lockViewFor assembles slug's lock for this request. It is nil with peer_auth
// off, so a default install renders nothing.
func (s *Service) lockViewFor(r *http.Request, slug string, isolation core.Isolation) (*lockView, error) {
	if s.cfg.Approvals == nil {
		return nil, nil
	}
	ctx := r.Context()
	console, err := store.HasConsoleLock(ctx, s.writeDB(), slug)
	if err != nil {
		return nil, err
	}
	creds, err := store.ApproverCredentials(ctx, s.writeDB(), store.ApproverPasskey)
	if err != nil {
		return nil, err
	}
	v := &lockView{
		Slug: slug, Name: slug, Force: slices.Contains(s.cfg.ForceLocked, slug), Console: console,
		Active: s.cfg.Views != nil && slices.Contains(s.cfg.Views.Lockable(), slug),
		Fenced: isolation.FencesOutbound(), PasskeysOn: s.cfg.Approvals.PasskeysOn(), HasPasskey: len(creds) > 0,
		Browser: s.classify(r) == credCookie, CanRestart: s.cfg.RequestRestart != nil, Isolation: isolation,
	}
	v.Rung, v.RungTo = rungs(v.Force, v.Console, isolation)
	if origins := s.cfg.Approvals.Origins(); len(origins) > 0 {
		v.Origin = origins[0]
		v.OnOrigin = slices.Contains(origins, requestOrigin(r))
	}
	return v, nil
}

// lockPage shows one project's lock and, when it can be changed here, the
// passkey button. A locked project is read through the main handle: this page
// shows its name and the lock, never its content.
func (s *Service) lockPage(w http.ResponseWriter, r *http.Request) {
	if s.lockRoutesOff(w, r, lockOffWords) {
		return
	}
	slug := r.PathValue("project")
	p, found, err := store.ProjectBySlug(r.Context(), s.writeDB(), slug)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !found {
		s.notFound(w, r, "No project with slug "+slug+".")
		return
	}
	v, err := s.lockViewFor(r, slug, isolationOf(p))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if p.Name != "" {
		v.Name = p.Name
	}
	s.render(w, r, "lock", pageData{Title: "Lock " + v.Name, Active: "projects", Data: v})
}

// lockChangeRequest reads what both ceremony routes share. It writes the answer
// and reports false when the request cannot go on.
func (s *Service) lockChangeRequest(w http.ResponseWriter, r *http.Request) (string, approval.LockAction, bool) {
	if s.lockRoutesOff(w, r, lockOffWords) {
		return "", "", false
	}
	if s.classify(r) != credCookie {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "a lock change needs a passkey: open this page in the browser you sign in to the console with"})
		return "", "", false
	}
	action, err := approval.ParseLockAction(r.PostForm.Get("action"))
	if err != nil {
		writeJSON(w, http.StatusBadRequest, map[string]string{"error": err.Error()})
		return "", "", false
	}
	return r.PathValue("project"), action, true
}

// lockBegin starts the passkey ceremony for one change.
func (s *Service) lockBegin(w http.ResponseWriter, r *http.Request) {
	slug, action, ok := s.lockChangeRequest(w, r)
	if !ok {
		return
	}
	begun, err := s.cfg.Approvals.BeginLockChange(r.Context(), slug, action)
	if err != nil {
		s.lockError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, begun)
}

// lockFinish verifies the assertion, stores the change and restarts the daemon
// to apply it. The answer is written first: the restart's clean shutdown lets a
// running handler finish.
func (s *Service) lockFinish(w http.ResponseWriter, r *http.Request) {
	slug, action, ok := s.lockChangeRequest(w, r)
	if !ok {
		return
	}
	if _, err := s.cfg.Approvals.FinishLockChange(r.Context(), r.PostForm.Get("id"), slug, action, []byte(r.PostForm.Get("response"))); err != nil {
		s.lockError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, map[string]any{"project": slug, "action": string(action), "restarting": s.cfg.RequestRestart != nil})
	if s.cfg.RequestRestart != nil {
		s.cfg.RequestRestart()
	}
}

// lockError answers a refused change in the owner's words.
func (s *Service) lockError(w http.ResponseWriter, r *http.Request, err error) {
	msg, status := "", http.StatusConflict
	switch {
	case errors.Is(err, approval.ErrForceLocked):
		msg = "this project is force locked in the daemon's config file: only the file can change it"
	case errors.Is(err, approval.ErrNoLockChange):
		msg = "this change expired or was already used; start again"
	case errors.Is(err, approval.ErrNoPasskey):
		msg = "no passkey is enrolled: add one on the Approvers page first"
	case errors.Is(err, store.ErrConsoleLockExists):
		msg = "this project is already locked from the console"
	case errors.Is(err, store.ErrConsoleLockNotFound):
		msg = "this project has no lock set from the console"
	case errors.Is(err, store.ErrConsoleLockUnfenced):
		msg = "make this project confidential or sealed first: a lock needs an isolation fence"
	case errors.Is(err, store.ErrProjectNotFound):
		msg, status = "no such project", http.StatusNotFound
	case errors.Is(err, passkey.ErrUnknownCredential):
		msg = "that passkey is not enrolled here"
	}
	if msg != "" {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	s.approverError(w, r, err)
}
