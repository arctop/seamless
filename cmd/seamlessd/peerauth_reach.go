package main

import (
	"context"
	"database/sql"
	"fmt"
	"log/slog"
	"strings"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// A restricted entry's shape (peer_auth.restricted's exclude and only; plan
// agent-accounts-and-releases, step B) against the projects table, for serve
// and doctor. The decisions are memory restricted-account-reach-decisions:
//
//   - an exclude slug that names no project disables that entry alone (B-D7):
//     a typo there leaves the project it meant reachable, so the account is
//     refused at the door while every other caller is served -- refusing to
//     start would also stop the daemon's own update restarts;
//   - an only slug that names no project, is locked, or is confidential or
//     sealed beside other projects is a warning: only fails closed, so the
//     account just reaches less than its entry says;
//   - an exclude slug whose project is not open now is information: it is
//     excluded in every state.
//
// projectProblems stays the locks' alone, so the restore validator that shares
// it (backups.go) is unchanged.

// reachFinding is one entry's finding: its status (statusFail disables the
// entry), the account, and the sentence.
type reachFinding struct {
	status  checkStatus
	account access.Account
	text    string
}

// restrictedReachFindings checks every restricted entry's exclude and only
// slugs against the projects table and policy's locks.
func restrictedReachFindings(ctx context.Context, db *sql.DB, policy *access.Policy) ([]reachFinding, error) {
	var out []reachFinding
	locked := map[string]bool{}
	for _, slug := range policy.OwnerLocked() {
		locked[slug] = true
	}
	for _, e := range policy.RestrictedEntries() {
		a := e.Account
		for _, slug := range e.Exclude {
			p, found, err := store.ProjectBySlug(ctx, db, slug)
			if err != nil {
				return nil, err
			}
			switch {
			case !found:
				out = append(out, reachFinding{statusFail, a, fmt.Sprintf("peer_auth.restricted %s excludes project %q, which "+
					"does not exist: if that is a typo, the project it meant is NOT excluded, so serve refuses this account "+
					"(uid %d) until the entry is corrected; every other caller is served", a.Name, slug, a.UID)})
			case p.Isolation != core.IsolationOpen && p.Isolation != "":
				out = append(out, reachFinding{statusInfo, a, fmt.Sprintf("peer_auth.restricted %s excludes project %q, which "+
					"is %s now: it stays excluded whatever its isolation becomes", a.Name, slug, isolationWord(p.Isolation))})
			}
		}
		for _, slug := range e.Only {
			p, found, err := store.ProjectBySlug(ctx, db, slug)
			if err != nil {
				return nil, err
			}
			switch {
			case !found:
				out = append(out, reachFinding{statusWarn, a, fmt.Sprintf("peer_auth.restricted %s names project %q under only, "+
					"which does not exist: the account reaches nothing there until the owner creates it", a.Name, slug)})
			case locked[slug]:
				out = append(out, reachFinding{statusWarn, a, fmt.Sprintf("peer_auth.restricted %s names project %q under only, "+
					"which is locked: a lock closes a project to every restricted account, so this one does not reach it", a.Name, slug)})
			case len(e.Only) > 1 && p.Isolation.FencesOutbound():
				out = append(out, reachFinding{statusWarn, a, fmt.Sprintf("peer_auth.restricted %s names project %q under only "+
					"beside other projects, and it is %s: a confidential or sealed project is reached only as an entry's one "+
					"project, so this account does not reach it -- give it an entry of its own (only: [%s]) on another account, "+
					"or open it", a.Name, slug, isolationWord(p.Isolation), slug)})
			}
		}
	}
	return out, nil
}

// applyRestrictedReach is serve's half: every finding is logged, and the
// entries an exclude typo disabled are refused from here on.
func applyRestrictedReach(ctx context.Context, db *sql.DB, policy *access.Policy, logger *slog.Logger) (*access.Policy, error) {
	findings, err := restrictedReachFindings(ctx, db, policy)
	if err != nil {
		return nil, err
	}
	var disabled []uint32
	for _, f := range findings {
		switch f.status {
		case statusFail:
			logger.Error("peer_auth: " + f.text)
			disabled = append(disabled, f.account.UID)
		case statusWarn:
			logger.Warn("peer_auth: " + f.text)
		default:
			logger.Info("peer_auth: " + f.text)
		}
	}
	if len(disabled) == 0 {
		return policy, nil
	}
	return policy.WithDisabled(disabled), nil
}

// restrictedRowTail is what every restricted account's doctor row ends with.
const restrictedRowTail = "its writes are labeled with its name, and its constraints, conventions, protocols and stages " +
	"wait for the owner's promotion"

// restrictedRow is doctor's row for one restricted entry (decision B-D8). A
// plain entry's row is the one doctor printed before shapes existed, word for
// word; a shaped entry's names its shape and, when the database is at hand,
// what it reaches now.
func restrictedRow(ctx context.Context, db *sql.DB, policy *access.Policy, e access.RestrictedEntry) (check, error) {
	a := e.Account
	if !e.Shaped() {
		return check{statusOK, "peer_auth.restricted", fmt.Sprintf("%s (uid %d): restricted -- the socket only; "+
			"reads and writes every open project that is not locked, reads global; "+restrictedRowTail, a.Name, a.UID)}, nil
	}
	var rule string
	switch {
	case len(e.Exclude) > 0:
		rule = fmt.Sprintf("exclude [%s] -- the socket only; reads and writes every open project that is not locked "+
			"but those, reads global", strings.Join(e.Exclude, ", "))
	case len(e.Only) == 1:
		rule = fmt.Sprintf("only [%s] -- the socket only; reads and writes that project while it is open, confidential "+
			"or sealed and not locked, and reads global unless it is sealed", e.Only[0])
	default:
		rule = fmt.Sprintf("only [%s] -- the socket only; reads and writes those of them that are open and not locked, "+
			"reads global", strings.Join(e.Only, ", "))
	}
	now := ""
	if db != nil {
		states, err := store.ReachProjects(ctx, db)
		if err != nil {
			return check{}, err
		}
		reach := policy.ReachOf(a.UID, states.Open, states.Fenced)
		described := make([]string, 0, len(reach))
		for _, slug := range reach {
			p, _, err := store.ProjectBySlug(ctx, db, slug)
			if err != nil {
				return check{}, err
			}
			described = append(described, fmt.Sprintf("%s (%s)", slug, isolationWord(p.Isolation)))
		}
		switch {
		case len(e.Exclude) > 0 && len(reach) == 1:
			now = " (now 1 project)"
		case len(e.Exclude) > 0:
			now = fmt.Sprintf(" (now %d projects)", len(reach))
		case len(described) == 0:
			now = " (now none)"
		default:
			now = " (now " + strings.Join(described, ", ") + ")"
		}
	}
	return check{statusOK, "peer_auth.restricted", fmt.Sprintf("%s (uid %d): restricted, %s%s; %s", a.Name, a.UID, rule, now,
		restrictedRowTail)}, nil
}
