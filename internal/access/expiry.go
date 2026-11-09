package access

import (
	"fmt"
	"slices"
	"strings"
	"time"
)

// Lease expiry, as every surface words it (the 2026-11-04 rule, step 8): from
// ExpiryWarning before a lease ends, every response to the session holding it
// says when access ends and how to ask again. Nothing extends a lease
// silently -- a refresh is a new approval -- so the warning is the agent's
// only notice before the project locks again.

// ExpiryWarning is how long before a lease's end the warnings start.
const ExpiryWarning = 30 * time.Minute

// Expiring returns the leases that end within ExpiryWarning of now (and have
// not ended yet), sorted by project, at most one per project (the latest).
func Expiring(leases []Lease, now time.Time) []Lease {
	byProject := map[string]Lease{}
	for _, l := range leases {
		left := l.Expires.Sub(now)
		if left <= 0 || left > ExpiryWarning {
			continue
		}
		if prev, ok := byProject[l.Project]; ok && !l.Expires.After(prev.Expires) {
			continue
		}
		byProject[l.Project] = l
	}
	// A project with a second lease that ends later is not expiring: the
	// later lease carries on (a refresh approved before the old one ended).
	for _, l := range leases {
		if prev, ok := byProject[l.Project]; ok && l.Expires.After(prev.Expires) {
			delete(byProject, l.Project)
		}
	}
	out := make([]Lease, 0, len(byProject))
	for _, l := range byProject {
		out = append(out, l)
	}
	slices.SortFunc(out, func(a, b Lease) int { return strings.Compare(a.Project, b.Project) })
	return out
}

// ExpiryNotice is the warning for every lease in leases that ends within
// ExpiryWarning of now: one line per project with the absolute end, the
// minutes left, and how to ask again. "" when none does.
func ExpiryNotice(leases []Lease, now time.Time) string {
	expiring := Expiring(leases, now)
	if len(expiring) == 0 {
		return ""
	}
	lines := make([]string, 0, len(expiring))
	for _, l := range expiring {
		lines = append(lines, fmt.Sprintf("Access to %s ends at %s (%s left). Nothing extends it silently: to keep "+
			"working there, run `seam access request %s` with your own shell tool to ask the owner again (a new approval).",
			l.Project, clock(l.Expires), minutesLeft(l.Expires.Sub(now)), l.Project))
	}
	return strings.Join(lines, "\n")
}

// minutesLeft renders a short remaining duration in whole minutes.
func minutesLeft(d time.Duration) string {
	switch m := int(d / time.Minute); {
	case m < 1:
		return "less than a minute"
	case m == 1:
		return "1 minute"
	default:
		return fmt.Sprintf("%d minutes", m)
	}
}

// clock renders an instant as the daemon's local wall clock, naming no zone
// (owner decision 2026-11-08): each surface shows its viewer's own clock --
// the terminal's for the CLI, the browser's for the console -- and the owner
// and the agents share this machine, so its clock is theirs.
func clock(t time.Time) string { return t.Local().Format("15:04") }

// clockDay is clock with the day, for an instant that may lie days back.
func clockDay(t time.Time) string { return t.Local().Format("15:04 on Jan 2") }

// Clock renders t as the warnings and refusals do: local time, no zone.
func Clock(t time.Time) string { return clock(t) }
