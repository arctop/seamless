package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// SettingUtilityActivation is the settings key holding the per-project
// utility-ranking activation state: which projects' briefings order their
// memory index by the utility blend. The gardener latches projects in as their
// demand data matures (see gardener.evaluateUtilityActivation); the owner can
// force a project on or off from the console. The bounded recall/prompt boosts
// are always on and are not gated here -- this state guards only the
// consequential surface, briefing index re-ordering.
const SettingUtilityActivation = "utility_activation"

// UtilityProjectState is one project's activation record. ReadyAt is the latch:
// once the gardener sets it, the project stays active (no flapping). Forced is
// the owner override: "on" and "off" both win over the latch; "" defers to it.
type UtilityProjectState struct {
	ReadyAt *time.Time `json:"ready_at,omitempty"`
	Forced  string     `json:"forced,omitempty"`
}

// UtilityActivation is the stored activation map, keyed by project slug
// (project "" -- the global scope -- is a valid key).
type UtilityActivation struct {
	Projects map[string]UtilityProjectState `json:"projects"`
}

// Active reports whether utility ranking applies to a project's briefing under
// the given mode ("auto", "on", or "off" -- config.Briefing.UtilityMode).
func (a UtilityActivation) Active(project, mode string) bool {
	switch mode {
	case "on":
		return true
	case "off":
		return false
	}
	st := a.Projects[project]
	switch st.Forced {
	case "on":
		return true
	case "off":
		return false
	}
	return st.ReadyAt != nil
}

// GetUtilityActivation loads the activation state; an absent row is an empty
// (nothing active) state, not an error.
func GetUtilityActivation(ctx context.Context, db *sql.DB) (UtilityActivation, error) {
	out := UtilityActivation{Projects: map[string]UtilityProjectState{}}
	raw, found, err := GetSetting(ctx, db, SettingUtilityActivation)
	if err != nil {
		return out, err
	}
	if !found || strings.TrimSpace(raw) == "" {
		return out, nil
	}
	if err := json.Unmarshal([]byte(raw), &out); err != nil {
		return out, fmt.Errorf("store.GetUtilityActivation: decode: %w", err)
	}
	if out.Projects == nil {
		out.Projects = map[string]UtilityProjectState{}
	}
	return out, nil
}

// SetUtilityActivation persists the activation state.
func SetUtilityActivation(ctx context.Context, db *sql.DB, a UtilityActivation) error {
	raw, err := json.Marshal(a)
	if err != nil {
		return fmt.Errorf("store.SetUtilityActivation: %w", err)
	}
	return SetSetting(ctx, db, SettingUtilityActivation, string(raw))
}

// Utility-activation readiness thresholds. A project's briefing switches to
// utility-blended ordering (in "auto" mode) only once its demand history is
// deep enough to rank on: old enough that decay has meaning, busy enough that
// the scores are not one session's noise, and broad enough that ordering by
// them changes more than a couple of lines. All three must hold. Exported so
// the gardener (which latches) and the console (which shows progress toward
// them) agree by construction.
const (
	UtilityReadyMinAgeDays  = 14                  // first demand event at least this old
	UtilityReadyMinEvents   = 20                  // session-deduped demand events in the window
	UtilityReadyMinMemories = 10                  // distinct memories that demand touched
	UtilityReadyWindow      = 30 * 24 * time.Hour // trailing window for the two counts above
)

// UtilityProjectDemand summarizes a project's query-gated demand history for
// the readiness decision: when demand first appeared, and how much of it -- in
// session-deduped events and distinct memories -- the trailing window holds.
type UtilityProjectDemand struct {
	Earliest       time.Time // first query-gated demand event ever (zero = none)
	RecentEvents   int       // session-deduped demand events within the window
	RecentMemories int       // distinct memories those recent events surfaced
}

// Ready reports whether this demand history satisfies every readiness
// threshold as of now.
func (d UtilityProjectDemand) Ready(now time.Time) bool {
	if d.Earliest.IsZero() || now.Sub(d.Earliest) < UtilityReadyMinAgeDays*24*time.Hour {
		return false
	}
	return d.RecentEvents >= UtilityReadyMinEvents && d.RecentMemories >= UtilityReadyMinMemories
}

// UtilityDemandByProject walks the query-gated demand events (recall hits,
// prompt-recall matches, explicit reads -- the same classes utility scores)
// grouped by the event's project slug. Passive briefing injections do not
// count, mirroring the score. The window bounds the Recent* counters; Earliest
// is all-time.
func UtilityDemandByProject(ctx context.Context, db *sql.DB, now time.Time, window time.Duration) (map[string]UtilityProjectDemand, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT ts, kind, session_id, project_slug, item_id, payload FROM events
		WHERE kind IN (?, ?)
		ORDER BY ts ASC, id ASC`,
		string(core.EventInjected), string(core.EventMemoryRead))
	if err != nil {
		return nil, fmt.Errorf("store.UtilityDemandByProject: query events: %w", err)
	}
	defer func() { _ = rows.Close() }()

	cutoff := now.Add(-window)
	out := map[string]UtilityProjectDemand{}
	seenEvents := map[string]struct{}{} // project + session + class, window-scoped
	seenMems := map[string]struct{}{}   // project + item id, window-scoped
	for rows.Next() {
		var tsStr, kind, sessionID, project, itemID, payload string
		if err := rows.Scan(&tsStr, &kind, &sessionID, &project, &itemID, &payload); err != nil {
			return nil, fmt.Errorf("store.UtilityDemandByProject: scan: %w", err)
		}
		ts, err := core.ParseTime(tsStr)
		if err != nil {
			return nil, fmt.Errorf("store.UtilityDemandByProject: parse ts: %w", err)
		}

		var class, session string
		var ids []string
		switch core.EventKind(kind) {
		case core.EventInjected:
			p, ok := decodeInjectedPayload(payload)
			if !ok {
				continue
			}
			if _, class = injectedUtilityWeight(p); class == "" {
				continue // passive exposure, not demand
			}
			session = injectedSessionKey(sessionID, p)
			ids = injectedItemIDs(itemID, p)
		case core.EventMemoryRead:
			if itemID == "" {
				continue
			}
			class, session, ids = "read", sessionID, []string{itemID}
		}

		d := out[project]
		if d.Earliest.IsZero() || ts.Before(d.Earliest) {
			d.Earliest = ts
		}
		if !ts.Before(cutoff) {
			eventKey := project + "\x00" + session + "\x00" + class
			if session == "" {
				eventKey = "" // unattributable sessions count individually
			}
			if _, dup := seenEvents[eventKey]; eventKey == "" || !dup {
				d.RecentEvents++
				if eventKey != "" {
					seenEvents[eventKey] = struct{}{}
				}
			}
			for _, id := range ids {
				memKey := project + "\x00" + id
				if _, dup := seenMems[memKey]; !dup {
					seenMems[memKey] = struct{}{}
					d.RecentMemories++
				}
			}
		}
		out[project] = d
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.UtilityDemandByProject: rows: %w", err)
	}
	return out, nil
}
