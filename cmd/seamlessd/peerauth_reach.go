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
// and doctor. A relay's restricted accounts (peer_auth.relays' restricted;
// plan networked-relay) take the same shapes and are checked by the same
// rules, each entry named by its relay. The decisions are memory
// restricted-account-reach-decisions:
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
// entry), the account, the relay whose entry it is ("" for an entry of
// peer_auth.restricted), and the sentence.
type reachFinding struct {
	status  checkStatus
	account access.Account
	relay   string
	text    string
}

// restrictedCheckName and relayRestrictedCheckName are doctor's row names for
// a restricted entry on this machine and for one on a relay's computer.
const (
	restrictedCheckName      = "peer_auth.restricted"
	relayRestrictedCheckName = "peer_auth.relays.restricted"
)

// reachCheckName is doctor's row name for an entry of relay ("" for this
// machine's).
func reachCheckName(relay string) string {
	if relay == "" {
		return restrictedCheckName
	}
	return relayRestrictedCheckName
}

// reachEntryKey names an entry the way its config key reads, for a finding:
// "peer_auth.restricted <account>", or "peer_auth.relays.<relay>.restricted
// <account there>".
func reachEntryKey(e access.RestrictedEntry) string {
	if e.Relay == "" {
		return "peer_auth.restricted " + e.Account.Name
	}
	return fmt.Sprintf("peer_auth.relays.%s.restricted %s", e.Relay, e.NameThere())
}

// reachEntryUID names an entry's uid for a finding: "uid N", and for a relay's
// account the computer it is a uid on.
func reachEntryUID(e access.RestrictedEntry) string {
	if e.Relay == "" {
		return fmt.Sprintf("uid %d", e.Account.UID)
	}
	return fmt.Sprintf("uid %d on %s", e.Account.UID, e.Relay)
}

// allRestrictedEntries is every restricted entry the policy holds: this
// machine's, by uid, then each mapped relay's, by the relay's name and the
// uid there.
func allRestrictedEntries(policy *access.Policy) []access.RestrictedEntry {
	out := policy.RestrictedEntries()
	for _, relay := range policy.Relays() {
		out = append(out, policy.RelayRestrictedEntries(relay)...)
	}
	return out
}

// restrictedReachFindings checks every restricted entry's exclude and only
// slugs, a relay's entries included, against the projects table and policy's
// locks.
func restrictedReachFindings(ctx context.Context, db *sql.DB, policy *access.Policy) ([]reachFinding, error) {
	var out []reachFinding
	locked := map[string]bool{}
	for _, slug := range policy.OwnerLocked() {
		locked[slug] = true
	}
	for _, e := range allRestrictedEntries(policy) {
		a, key := e.Account, reachEntryKey(e)
		add := func(status checkStatus, text string) { out = append(out, reachFinding{status, a, e.Relay, text}) }
		for _, slug := range e.Exclude {
			p, found, err := store.ProjectBySlug(ctx, db, slug)
			if err != nil {
				return nil, err
			}
			switch {
			case !found:
				add(statusFail, fmt.Sprintf("%s excludes project %q, which "+
					"does not exist: if that is a typo, the project it meant is NOT excluded, so serve refuses this account "+
					"(%s) until the entry is corrected; every other caller is served", key, slug, reachEntryUID(e)))
			case p.Isolation != core.IsolationOpen && p.Isolation != "":
				add(statusInfo, fmt.Sprintf("%s excludes project %q, which "+
					"is %s now: it stays excluded whatever its isolation becomes", key, slug, isolationWord(p.Isolation)))
			}
		}
		for _, slug := range e.Only {
			p, found, err := store.ProjectBySlug(ctx, db, slug)
			if err != nil {
				return nil, err
			}
			switch {
			case !found:
				add(statusWarn, fmt.Sprintf("%s names project %q under only, "+
					"which does not exist: the account reaches nothing there until the owner creates it", key, slug))
			case locked[slug]:
				add(statusWarn, fmt.Sprintf("%s names project %q under only, "+
					"which is locked: a lock closes a project to every restricted account, so this one does not reach it", key, slug))
			case len(e.Only) > 1 && p.Isolation.FencesOutbound():
				add(statusWarn, fmt.Sprintf("%s names project %q under only "+
					"beside other projects, and it is %s: a confidential or sealed project is reached only as an entry's one "+
					"project, so this account does not reach it -- give it an entry of its own (only: [%s]) on another account, "+
					"or open it", key, slug, isolationWord(p.Isolation), slug))
			}
		}
	}
	return out, nil
}

// applyRestrictedReach is serve's half: every finding is logged, and the
// entries an exclude typo disabled -- this machine's, and each relay's -- are
// refused from here on.
func applyRestrictedReach(ctx context.Context, db *sql.DB, policy *access.Policy, logger *slog.Logger) (*access.Policy, error) {
	findings, err := restrictedReachFindings(ctx, db, policy)
	if err != nil {
		return nil, err
	}
	var disabled []uint32
	relayDisabled := map[string][]uint32{}
	for _, f := range findings {
		switch f.status {
		case statusFail:
			logger.Error("peer_auth: " + f.text)
			if f.relay == "" {
				disabled = append(disabled, f.account.UID)
			} else {
				relayDisabled[f.relay] = append(relayDisabled[f.relay], f.account.UID)
			}
		case statusWarn:
			logger.Warn("peer_auth: " + f.text)
		default:
			logger.Info("peer_auth: " + f.text)
		}
	}
	if len(disabled) > 0 {
		policy = policy.WithDisabled(disabled)
	}
	for relay, uids := range relayDisabled {
		policy = policy.WithRelayDisabled(relay, uids)
	}
	return policy, nil
}

// restrictedRowTail is what every restricted account's doctor row ends with.
const restrictedRowTail = "its writes are labeled with its name, and its constraints, conventions, protocols and stages " +
	"wait for the owner's promotion"

// restrictedRow is doctor's row for one restricted entry of this machine
// (decision B-D8). A plain entry's row is the one doctor printed before
// shapes existed, word for word; a shaped entry's names its shape and, when
// the database is at hand, what it reaches now (shapedReachRow).
func restrictedRow(ctx context.Context, db *sql.DB, policy *access.Policy, e access.RestrictedEntry) (check, error) {
	a := e.Account
	if !e.Shaped() {
		return check{statusOK, restrictedCheckName, fmt.Sprintf("%s (uid %d): restricted -- the socket only; "+
			"reads and writes every open project that is not locked, reads global; "+restrictedRowTail, a.Name, a.UID)}, nil
	}
	return shapedReachRow(ctx, db, policy, e)
}

// relayRestrictedRows is doctor's rows for the relays' restricted entries
// that shape their reach: one row each, as restrictedRow's shaped row. A
// plain entry there has no row of its own: the relay's mapping row names it
// (relayRestrictedWords), word for word as before shapes existed.
func relayRestrictedRows(ctx context.Context, db *sql.DB, policy *access.Policy) ([]check, error) {
	var out []check
	for _, relay := range policy.Relays() {
		for _, e := range policy.RelayRestrictedEntries(relay) {
			if !e.Shaped() {
				continue
			}
			row, err := shapedReachRow(ctx, db, policy, e)
			if err != nil {
				return out, err
			}
			out = append(out, row)
		}
	}
	return out, nil
}

// shapedReachRow is doctor's row for one shaped restricted entry, on this
// machine or on a relay's computer: the account, its shape and, when the
// database is at hand, what it reaches now.
func shapedReachRow(ctx context.Context, db *sql.DB, policy *access.Policy, e access.RestrictedEntry) (check, error) {
	a := e.Account
	who, via := fmt.Sprintf("%s (uid %d)", a.Name, a.UID), "the socket only"
	if e.Relay != "" {
		who, via = fmt.Sprintf("%s (uid %d there)", a.Name, a.UID), "through relay "+e.Relay+" only"
	}
	var rule string
	switch {
	case len(e.Exclude) > 0:
		rule = fmt.Sprintf("exclude [%s] -- %s; reads and writes every open project that is not locked "+
			"but those, reads global", strings.Join(e.Exclude, ", "), via)
	case len(e.Only) == 1:
		rule = fmt.Sprintf("only [%s] -- %s; reads and writes that project while it is open, confidential "+
			"or sealed and not locked, and reads global unless it is sealed", e.Only[0], via)
	default:
		rule = fmt.Sprintf("only [%s] -- %s; reads and writes those of them that are open and not locked, "+
			"reads global", strings.Join(e.Only, ", "), via)
	}
	now := ""
	if db != nil {
		states, err := store.ReachProjects(ctx, db)
		if err != nil {
			return check{}, err
		}
		var reach []string
		if e.Relay == "" {
			reach = policy.ReachOf(a.UID, states.Open, states.Fenced)
		} else {
			reach = policy.RelayReachOf(e.Relay, a.UID, states.Open, states.Fenced)
		}
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
	return check{statusOK, reachCheckName(e.Relay), fmt.Sprintf("%s: restricted, %s%s; %s", who, rule, now, restrictedRowTail)}, nil
}
