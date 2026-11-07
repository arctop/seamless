package console

// A request's reach under locked projects (peer_auth.locked).
//
// With nothing locked the console reads every request through one handle
// (Config.DB) and nothing here runs. With projects locked, the daemon hands
// the console a pool of filtered handles (Config.Views, store.Views), one per
// hidden set, and each request reads through the one that hides exactly what
// that request may not read:
//
//   - a browser opens a locked project with a console unlock: its
//     seamless_unlock cookie names live store.ConsoleUnlock rows (unlock.go);
//   - a `seam` command on the socket opens the projects its agent's tree holds
//     a lease on (access.View.Leases), so a leased session's CLI reads the
//     project while a browser and every other tree stay locked.
//
// Everything else -- the TCP key, a browser with no unlock, a tree with no
// lease -- reads the base handle, on which every locked project is absent.
// A console unlock never reaches an agent: it is a console cookie, never a
// lease, and only this file reads it.

import (
	"context"
	"database/sql"
	"net/http"
	"slices"
	"time"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
)

// unlockCookieName holds a browser's console-unlock secret. The database keeps
// only its hash (store.HashConsoleToken); each project it opens is a row.
const unlockCookieName = "seamless_unlock"

// reach is one request's read reach while projects are locked.
type reach struct {
	// db is the handle this request reads.
	db *sql.DB
	// lockable are the locked projects (store.Views.Lockable); open are the
	// ones this request reads, leased or unlocked; unlocked are the ones the
	// browser's console unlocks open.
	lockable []string
	open     []string
	unlocked []string
	// token is the hash of the browser's seamless_unlock cookie when it names
	// a live unlock, "" otherwise.
	token string
	// until is when the earliest lease or unlock among open ends (zero with
	// nothing open): a long-lived request (the live feed) ends there.
	until time.Time
}

type reachKey struct{}

// reachOf returns the request's reach, nil when nothing is locked.
func reachOf(ctx context.Context) *reach {
	rc, _ := ctx.Value(reachKey{}).(*reach)
	return rc
}

// db is the handle a request reads: the one its reach chose, else Config.DB.
func (s *Service) db(ctx context.Context) *sql.DB {
	if rc := reachOf(ctx); rc != nil && rc.db != nil {
		return rc.db
	}
	return s.cfg.DB
}

// eventLog is the event recorder a request reads through: Config.Events,
// reading from the request's handle when its reach chose another one. Writes
// and the live fan-out are the same recorder's either way.
func (s *Service) eventLog(ctx context.Context) *events.Recorder {
	if s.cfg.Events == nil {
		return nil
	}
	if rc := reachOf(ctx); rc != nil && rc.db != nil && rc.db != s.cfg.DB {
		return s.cfg.Events.ReadingFrom(rc.db)
	}
	return s.cfg.Events
}

// withReach attaches the request's reach. It runs only while projects are
// locked (Config.Views set) and fails closed: a lookup that fails leaves the
// request on the base handle, where every locked project stays locked.
func (s *Service) withReach(r *http.Request, cred credential) *http.Request {
	if s.cfg.Views == nil {
		return r
	}
	ctx := r.Context()
	now := time.Now()
	rc := &reach{db: s.cfg.DB, lockable: s.cfg.Views.Lockable()}
	note := func(project string, ends time.Time) {
		if !slices.Contains(rc.lockable, project) {
			return
		}
		if !slices.Contains(rc.open, project) {
			rc.open = append(rc.open, project)
		}
		if rc.until.IsZero() || ends.Before(rc.until) {
			rc.until = ends
		}
	}
	// A socket request of the owner's agent tree: the leases its View holds.
	// A TCP view never holds one (access.View.WithLeases).
	if v, ok := access.FromContext(ctx); ok {
		for _, l := range v.Leases() {
			note(l.Project, l.Expires)
		}
	}
	// A browser: its console unlocks. Only a cookie-authenticated request
	// carries them -- the cookie is per browser, and the bearer key and the
	// socket are not browsers.
	if cred == credCookie {
		if c, err := r.Cookie(unlockCookieName); err == nil && c.Value != "" {
			token := store.HashConsoleToken(c.Value)
			rows, err := store.LiveConsoleUnlocks(ctx, s.writeDB(), token, now)
			if err != nil {
				s.logger.Warn("console: console unlocks unavailable; locked projects stay locked", "error", err)
				rows = nil
			}
			for _, u := range rows {
				if u.ExpiresAt == nil || !slices.Contains(rc.lockable, u.Project) {
					continue
				}
				rc.token = token
				if !slices.Contains(rc.unlocked, u.Project) {
					rc.unlocked = append(rc.unlocked, u.Project)
				}
				note(u.Project, *u.ExpiresAt)
			}
		}
	}
	slices.Sort(rc.open)
	slices.Sort(rc.unlocked)
	if len(rc.open) > 0 {
		db, err := s.cfg.Views.For(rc.open)
		if err != nil {
			s.logger.Warn("console: filtered handle unavailable; locked projects stay locked", "error", err)
			rc.open, rc.unlocked, rc.token, rc.until = nil, nil, "", time.Time{}
		} else {
			rc.db = db
		}
	}
	return r.WithContext(context.WithValue(ctx, reachKey{}, rc))
}

// lockedProjects returns the locked projects this request does not read,
// sorted: nil when nothing is locked.
func lockedProjects(ctx context.Context) []string {
	rc := reachOf(ctx)
	if rc == nil {
		return nil
	}
	out := make([]string, 0, len(rc.lockable))
	for _, slug := range rc.lockable {
		if !slices.Contains(rc.open, slug) {
			out = append(out, slug)
		}
	}
	return out
}

// lockedHere reports whether slug is a locked project this request does not
// read: it shows its name and counts, never its content.
func lockedHere(ctx context.Context, slug string) bool {
	return slices.Contains(lockedProjects(ctx), slug)
}

// unlockedHere reports whether this browser's console unlock opens slug.
func unlockedHere(ctx context.Context, slug string) bool {
	rc := reachOf(ctx)
	return rc != nil && slices.Contains(rc.unlocked, slug)
}

// broadened reports whether the request reads a handle other than the base
// one: a lease or an unlock opened something.
func broadened(ctx context.Context) bool {
	rc := reachOf(ctx)
	return rc != nil && len(rc.open) > 0
}
