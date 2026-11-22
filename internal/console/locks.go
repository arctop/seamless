package console

// The Locks page (plan repo-locks): every repo lock -- a lock on a
// repository's refs that agents declare once and hold while they land changes
// -- with its holder (and the restricted account it belongs to, if any), since
// and until, its queue (how many sessions wait, and which is next), the
// repository identities that resolve to it, and the projects its declaration
// lists; the declarations restricted accounts filed, waiting for the owner;
// and the owner's three decisions, force-release, approve and discard.
//
// A lock belongs to no project, so the page reads every lock whatever this
// request's handle hides (store.viewExempt); what the handle hides it hides
// here too: a listed project it does not show is not listed, and a holder in
// such a project is "a session this console does not show".

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// lkRow is one lock as the page shows it.
type lkRow struct {
	ID     string   `json:"id"`
	Label  string   `json:"label"`
	Repo   string   `json:"repo"`
	Name   string   `json:"name"`
	Refs   []string `json:"refs"`
	State  string   `json:"state"`
	Held   bool     `json:"held"`
	Lapsed bool     `json:"lapsed,omitempty"`
	Holder string   `json:"holder,omitempty"`
	// HolderName and HolderProject are the holder's session as this
	// request's handle shows it; HolderHidden is a holder it does not show.
	HolderName    string `json:"holder_session,omitempty"`
	HolderProject string `json:"holder_project,omitempty"`
	HolderHidden  bool   `json:"holder_hidden,omitempty"`
	// HolderAccount is the restricted account the holding session belongs
	// to, "" for the owner's own sessions.
	HolderAccount string `json:"holder_account,omitempty"`
	// Waiting is how many sessions wait for the lock; NextWaiter is the head
	// of its queue, named only where this request's handle shows its session
	// (NextWaiterHidden otherwise).
	Waiting           int                  `json:"waiting"`
	NextWaiter        string               `json:"next_waiter,omitempty"`
	NextWaiterName    string               `json:"next_waiter_session,omitempty"`
	NextWaiterAccount string               `json:"next_waiter_account,omitempty"`
	NextWaiterHidden  bool                 `json:"next_waiter_hidden,omitempty"`
	Since             *time.Time           `json:"since,omitempty"`
	Until             *time.Time           `json:"until,omitempty"`
	Aliases           []core.RepoLockAlias `json:"aliases"`
	Projects          []string             `json:"projects"`
	Account           string               `json:"account,omitempty"`
	CreatedBy         string               `json:"created_by,omitempty"`
	CreatedByName     string               `json:"created_by_session,omitempty"`
	CreatedAt         time.Time            `json:"created_at"`
}

// lkPageData is the Locks page's payload, and its JSON.
type lkPageData struct {
	Locks     []lkRow `json:"locks"`
	Requested []lkRow `json:"requested"`
	// Focus is the lock a detail URL named (/console/locks/{id}), marked on
	// the page.
	Focus string `json:"focus,omitempty"`
}

// lkSessionName is a session's name and project as this request's handle
// shows them; ok is false for one it does not show (or none at all).
func (s *Service) lkSessionName(ctx context.Context, id string) (name, project string, ok bool, err error) {
	sess, ok, err := s.lkSession(ctx, id)
	return sess.Name, sess.ProjectSlug, ok, err
}

// lkSession is a session as this request's handle shows it; ok is false for
// one it does not show (or none at all).
func (s *Service) lkSession(ctx context.Context, id string) (core.Session, bool, error) {
	if id == "" {
		return core.Session{}, false, nil
	}
	return store.SessionByID(ctx, s.db(ctx), id)
}

// lkRowFor renders one lock for the page.
func (s *Service) lkRowFor(ctx context.Context, l core.RepoLock, now time.Time) (lkRow, error) {
	row := lkRow{
		ID: l.ID, Label: l.Label(), Repo: l.Repo(), Name: l.Name, Refs: l.Refs, State: string(l.State),
		Held: l.HoldLive(now), Aliases: l.Aliases, Projects: l.Projects, Account: l.Account,
		CreatedBy: l.CreatedBy, CreatedAt: l.CreatedAt, Waiting: len(l.Waiters),
	}
	if l.Holder != "" {
		row.Holder, row.Since, row.Until, row.Lapsed = l.Holder, l.HeldSince, l.LeaseExpiresAt, !row.Held
		sess, ok, err := s.lkSession(ctx, l.Holder)
		if err != nil {
			return lkRow{}, err
		}
		row.HolderName, row.HolderProject, row.HolderAccount, row.HolderHidden = sess.Name, sess.ProjectSlug, sess.Account, !ok
	}
	if next := l.NextWaiter(); next != "" {
		sess, ok, err := s.lkSession(ctx, next)
		if err != nil {
			return lkRow{}, err
		}
		if ok {
			row.NextWaiter, row.NextWaiterName, row.NextWaiterAccount = next, sess.Name, sess.Account
		}
		row.NextWaiterHidden = !ok
	}
	if name, _, ok, err := s.lkSessionName(ctx, l.CreatedBy); err != nil {
		return lkRow{}, err
	} else if ok {
		row.CreatedByName = name
	}
	return row, nil
}

// lkData reads the page through this request's handle.
func (s *Service) lkData(ctx context.Context) (lkPageData, error) {
	locks, err := store.ListRepoLocks(ctx, s.db(ctx))
	if err != nil {
		return lkPageData{}, err
	}
	now := time.Now().UTC()
	d := lkPageData{Locks: []lkRow{}, Requested: []lkRow{}}
	for _, l := range locks {
		row, err := s.lkRowFor(ctx, l, now)
		if err != nil {
			return lkPageData{}, err
		}
		if l.State == core.RepoLockRequested {
			d.Requested = append(d.Requested, row)
		} else {
			d.Locks = append(d.Locks, row)
		}
	}
	return d, nil
}

// locksPage is GET /console/locks.
func (s *Service) locksPage(w http.ResponseWriter, r *http.Request) {
	d, err := s.lkData(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, d)
		return
	}
	s.render(w, r, "locks", pageData{Title: "Locks", Active: "locks", Data: d})
}

// lockDetail is GET /console/locks/{id}: one lock's row for a JSON caller, the
// page with that lock marked for a browser.
func (s *Service) lockDetail(w http.ResponseWriter, r *http.Request) {
	l, ok := s.lkTarget(w, r)
	if !ok {
		return
	}
	if wantsJSON(r) {
		row, err := s.lkRowFor(r.Context(), l, time.Now().UTC())
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		writeJSON(w, http.StatusOK, row)
		return
	}
	d, err := s.lkData(r.Context())
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	d.Focus = l.ID
	s.render(w, r, "locks", pageData{Title: "LOCK " + l.Label(), Active: "locks", Data: d})
}

// lkTarget reads the lock a decision names through this request's handle, and
// answers the request itself when there is none.
func (s *Service) lkTarget(w http.ResponseWriter, r *http.Request) (core.RepoLock, bool) {
	id := r.PathValue("id")
	l, ok, err := store.RepoLockByID(r.Context(), s.db(r.Context()), id)
	if err != nil {
		s.serverError(w, r, err)
		return core.RepoLock{}, false
	}
	if !ok {
		s.notFound(w, r, "No lock with id "+id+".")
		return core.RepoLock{}, false
	}
	return l, true
}

// lkDecided records one of the owner's decisions and answers it: the lock as
// it now stands for a JSON caller, else back to the page with a notice.
func (s *Service) lkDecided(w http.ResponseWriter, r *http.Request, l core.RepoLock, action, session, notice string, extra map[string]any) {
	ctx := r.Context()
	if s.cfg.Events != nil {
		if _, err := s.eventLog(ctx).Record(ctx, core.Event{
			Kind: core.EventRepoLockChanged, SessionID: session, ItemID: l.ID,
			Payload: map[string]any{"action": action, "name": l.Name, "by": "console"},
		}); err != nil {
			s.logger.Warn("console: record lock decision", "action", action, "error", err)
		}
	}
	if wantsJSON(r) {
		row, err := s.lkRowFor(ctx, l, time.Now().UTC())
		if err != nil {
			s.serverError(w, r, err)
			return
		}
		out := map[string]any{"lock": row}
		for k, v := range extra {
			out[k] = v
		}
		writeJSON(w, http.StatusOK, out)
		return
	}
	// A form that names a next (the Overview's inline Approve) goes back there
	// with the notice; without one, Locks, as always (ownerNext).
	http.Redirect(w, r, ownerNext(r.PostFormValue("next"), "/console/locks?notice="+url.QueryEscape(notice), "notice", notice), http.StatusSeeOther)
}

// lockRelease is POST /console/locks/{id}/release: the owner's force-release
// of whatever hold the lock carries.
func (s *Service) lockRelease(w http.ResponseWriter, r *http.Request) {
	l, ok := s.lkTarget(w, r)
	if !ok {
		return
	}
	got, err := store.ForceReleaseRepoLock(r.Context(), s.writeDB(), l.ID, time.Now().UTC())
	switch {
	case errors.Is(err, store.ErrRepoLockFree):
		s.formError(w, r, http.StatusConflict, "LOCK "+l.Label()+" is not held: there is nothing to release")
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	extra := map[string]any{}
	if name, _, shown, err := s.lkSessionName(r.Context(), got.PriorHolder); err == nil && shown {
		extra["prior_holder"], extra["prior_holder_session"] = got.PriorHolder, name
	}
	// The event names the freed session, so a holder in a project this
	// console hides keeps the event hidden with it.
	s.lkDecided(w, r, got.Lock, core.RepoLockActionForceReleased, got.PriorHolder,
		"Released LOCK "+got.Lock.Label()+".", extra)
}

// lockApprove is POST /console/locks/{id}/approve: a restricted account's
// declaration becomes an active lock.
func (s *Service) lockApprove(w http.ResponseWriter, r *http.Request) {
	l, ok := s.lkTarget(w, r)
	if !ok {
		return
	}
	got, err := store.ApproveRepoLock(r.Context(), s.writeDB(), l.ID, time.Now().UTC())
	switch {
	case errors.Is(err, store.ErrRepoLockNotRequested):
		s.formError(w, r, http.StatusConflict, "LOCK "+l.Label()+" is already active")
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	s.lkDecided(w, r, got, core.RepoLockActionApproved, "", "Approved LOCK "+got.Label()+": agents may hold it now.", nil)
}

// lockDiscard is POST /console/locks/{id}/discard: a restricted account's
// declaration is deleted, and the repository may be declared again.
func (s *Service) lockDiscard(w http.ResponseWriter, r *http.Request) {
	l, ok := s.lkTarget(w, r)
	if !ok {
		return
	}
	got, err := store.DiscardRepoLock(r.Context(), s.writeDB(), l.ID)
	switch {
	case errors.Is(err, store.ErrRepoLockNotRequested):
		s.formError(w, r, http.StatusConflict, "LOCK "+l.Label()+" is active: only a declaration waiting for you is discarded")
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	got.Aliases, got.Projects = nil, nil
	s.lkDecided(w, r, got, core.RepoLockActionDiscarded, "", "Discarded the declaration of LOCK "+l.Label()+".", nil)
}
