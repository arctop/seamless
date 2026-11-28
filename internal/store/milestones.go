// Momentum: the latched milestone ledger.
//
// A short, honest set of once-ever milestones -- counts a project actually
// crossed and firsts that actually happened -- minted as milestone.reached
// events via events.RecordOnce, whose kind+item_id guarded insert is the
// once-ever latch. The plan.shipped settlement (once per plan, on the task
// transition that closes its last step as done) is minted here too, through
// the same latch. Every payload states its exact claim verbatim and the real
// count behind it ("100 memories written in seamless", count 100), never a
// score-shaped invention. The checks piggyback on the event recorder's write
// path, where the counts change: there is no polling pass. They run on the
// write path of every watched event, so each reads its latch before it
// counts or inserts anything: a milestone already minted costs one indexed
// read, never a count over the event log or a write.
package store

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// EventMilestoneReached is the milestone ledger's event kind: a latched
// milestone, minted at most once per item_id (the milestone key) via
// events.RecordOnce. Defined here rather than in internal/core because the
// milestone vocabulary -- keys, thresholds, claims -- lives in this file and
// the ledger's consumers read them from here.
const EventMilestoneReached core.EventKind = "milestone.reached"

// The plan-settlement event kind is core.EventPlanShipped: checkPlanShipped
// below mints it on the task transition that completes a plan, and the
// recorder's post-insert hook feeds the minted event straight back through
// CheckMilestones, whose plan.shipped branch latches the first-plan-shipped
// milestone.

// MilestoneMemoryWrittenThresholds are the per-project memory-written counts
// that mint a milestone, ascending. Exported maturity-style so the ledger's
// consumers and this computation agree by construction.
var MilestoneMemoryWrittenThresholds = []int{100, 500, 1000}

// MilestoneRecallAnsweredThreshold is the per-project count of answered
// recalls -- recall-tool calls that returned at least one hit -- that mints a
// milestone. Kind-browse listings are passive exposure, not answered recalls,
// and do not count (closed-loop-utility-signal-contract).
const MilestoneRecallAnsweredThreshold = 1000

// OnceRecorder is the once-ever latch the milestone layer mints through --
// events.Recorder.RecordOnce, named as a role interface so this package does
// not import internal/events (whose in-package tests import store).
type OnceRecorder interface {
	RecordOnce(ctx context.Context, e core.Event) (id string, recorded bool, err error)
}

// CheckMilestones runs the milestone checks the just-landed event e can have
// moved, minting any newly crossed milestone through rec. The event recorder
// calls it after every successful insert, so each COUNT already includes e.
//
// Events with no project slug move no per-project count and are skipped
// outright, as are all kinds outside the small watched set -- in particular
// milestone.reached itself, which bounds the recorder's re-entrant call.
// plan.shipped is watched on purpose: the recorder feeds a settlement this
// very function just minted back through here to latch the first-plan-shipped
// milestone, and that second re-entry ends at milestone.reached.
func CheckMilestones(ctx context.Context, db *sql.DB, rec OnceRecorder, e core.Event) error {
	if e.ProjectSlug == "" {
		return nil
	}
	switch e.Kind {
	case core.EventSessionStarted, core.EventMemoryWritten, core.EventMemorySuperseded,
		core.EventInjected, core.EventTaskTransition, core.EventPlanShipped:
	default:
		return nil
	}

	var errs error
	switch e.Kind {
	case core.EventMemoryWritten:
		errs = errors.Join(errs, checkMemoryWrittenMilestones(ctx, db, rec, e))
	case core.EventInjected:
		errs = errors.Join(errs, checkRecallAnsweredMilestone(ctx, db, rec, e))
	case core.EventMemorySuperseded:
		errs = errors.Join(errs, mintFirstSupersession(ctx, rec, e))
	case core.EventTaskTransition:
		errs = errors.Join(errs, checkPlanShipped(ctx, db, rec, e))
	case core.EventPlanShipped:
		errs = errors.Join(errs, mintFirstPlanShipped(ctx, rec, e.SessionID, e.ProjectSlug,
			payloadString(e.Payload, "slug", "plan"), payloadInt(e.Payload, "steps")))
	}
	// The birthday check piggybacks on every watched kind: a project's next
	// activity after an anniversary mints it, with no polling pass.
	errs = errors.Join(errs, checkProjectBirthday(ctx, db, rec, e))
	return errs
}

// milestoneLatched reports whether the milestone key has been minted: the read
// half of RecordOnce's guard, so a check whose milestone is already in the
// ledger stops before it counts anything or opens a write.
func milestoneLatched(ctx context.Context, db *sql.DB, key string) (bool, error) {
	var one int
	err := db.QueryRowContext(ctx,
		`SELECT 1 FROM events WHERE kind = ? AND item_id = ? LIMIT 1`,
		string(EventMilestoneReached), key).Scan(&one)
	if errors.Is(err, sql.ErrNoRows) {
		return false, nil
	}
	if err != nil {
		return false, fmt.Errorf("store.CheckMilestones: read latch %s: %w", key, err)
	}
	return true, nil
}

// memoryMilestoneKey is the latch key of a project's memory-written threshold.
func memoryMilestoneKey(threshold int, project string) string {
	return fmt.Sprintf("memories-%d:%s", threshold, project)
}

// checkMemoryWrittenMilestones counts the project's memory.written events
// (the triggering one included) and mints the highest threshold the count has
// reached. Checked on every write, a count meets each threshold on the write
// that reaches it, so each mints in its turn. A count first checked far past
// several of them -- writes recorded before the milestones were, or by a
// recorder that never armed them -- mints only the highest, with the real
// count in the payload ("1000 memories written in demo", count 1450), rather
// than a row now for each lower threshold passed long ago. Once the top
// threshold is minted, the check is one latch read.
func checkMemoryWrittenMilestones(ctx context.Context, db *sql.DB, rec OnceRecorder, e core.Event) error {
	top := MilestoneMemoryWrittenThresholds[len(MilestoneMemoryWrittenThresholds)-1]
	if done, err := milestoneLatched(ctx, db, memoryMilestoneKey(top, e.ProjectSlug)); err != nil || done {
		return err
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events WHERE kind = ? AND project_slug = ?`,
		string(core.EventMemoryWritten), e.ProjectSlug).Scan(&n); err != nil {
		return fmt.Errorf("store.CheckMilestones: count memory writes: %w", err)
	}
	reached := 0
	for _, threshold := range MilestoneMemoryWrittenThresholds {
		if n >= threshold {
			reached = threshold // thresholds ascend
		}
	}
	if reached == 0 {
		return nil
	}
	key := memoryMilestoneKey(reached, e.ProjectSlug)
	_, _, err := rec.RecordOnce(ctx, core.Event{
		Kind: EventMilestoneReached, SessionID: e.SessionID,
		ProjectSlug: e.ProjectSlug, ItemID: key,
		Payload: map[string]any{
			"claim":     fmt.Sprintf("%d memories written in %s", reached, e.ProjectSlug),
			"project":   e.ProjectSlug,
			"count":     n,
			"threshold": reached,
		},
	})
	if err != nil {
		return fmt.Errorf("store.CheckMilestones: mint %s: %w", key, err)
	}
	return nil
}

// checkRecallAnsweredMilestone counts the project's answered recalls --
// retrieval.injected events with source "recall", the recall-tool hits the
// closed-loop contract classifies as query-gated demand -- and mints the
// threshold once reached. Kind-browse hits (source "recall-browse") and every
// briefing or prompt injection stay out of the count; the aggregation is
// read-only over the event log and leaves retrieval stats untouched.
func checkRecallAnsweredMilestone(ctx context.Context, db *sql.DB, rec OnceRecorder, e core.Event) error {
	if payloadString(e.Payload, "source") != "recall" {
		return nil
	}
	key := fmt.Sprintf("recalls-%d:%s", MilestoneRecallAnsweredThreshold, e.ProjectSlug)
	if done, err := milestoneLatched(ctx, db, key); err != nil || done {
		return err
	}
	var n int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*) FROM events
		 WHERE kind = ? AND project_slug = ? AND json_extract(payload, '$.source') = 'recall'`,
		string(core.EventInjected), e.ProjectSlug).Scan(&n); err != nil {
		return fmt.Errorf("store.CheckMilestones: count answered recalls: %w", err)
	}
	if n < MilestoneRecallAnsweredThreshold {
		return nil
	}
	_, _, err := rec.RecordOnce(ctx, core.Event{
		Kind: EventMilestoneReached, SessionID: e.SessionID,
		ProjectSlug: e.ProjectSlug, ItemID: key,
		Payload: map[string]any{
			"claim":     fmt.Sprintf("%d recalls answered in %s", MilestoneRecallAnsweredThreshold, e.ProjectSlug),
			"project":   e.ProjectSlug,
			"count":     n,
			"threshold": MilestoneRecallAnsweredThreshold,
		},
	})
	if err != nil {
		return fmt.Errorf("store.CheckMilestones: mint %s: %w", key, err)
	}
	return nil
}

// mintFirstSupersession latches the project's first supersession: the first
// time its knowledge visibly turned over rather than just accumulated. The
// triggering memory.superseded event is the whole evidence, so no count query
// runs -- the RecordOnce latch is the entire check.
func mintFirstSupersession(ctx context.Context, rec OnceRecorder, e core.Event) error {
	key := "first-supersession:" + e.ProjectSlug
	_, _, err := rec.RecordOnce(ctx, core.Event{
		Kind: EventMilestoneReached, SessionID: e.SessionID,
		ProjectSlug: e.ProjectSlug, ItemID: key,
		Payload: map[string]any{
			"claim":   fmt.Sprintf("first supersession in %s", e.ProjectSlug),
			"project": e.ProjectSlug,
			"count":   1,
			"memory":  e.ItemID,
		},
	})
	if err != nil {
		return fmt.Errorf("store.CheckMilestones: mint %s: %w", key, err)
	}
	return nil
}

// checkPlanShipped mints the plan.shipped settlement on the task transition
// that completes a plan: a step closing as done leaves every step in the plan
// closed (done or dropped -- the planRollups completeness test, "one step from
// shipped" reaching zero). Judged from the tasks table, never inferred: the
// transition's task row names the plan and project, and the grouped count is
// the claim's evidence. The RecordOnce latch on (plan.shipped, slug) makes the
// settlement once-ever per plan, so repeated rollup recomputes cannot re-mint;
// the recorder's post-insert hook then feeds the minted event back through
// CheckMilestones, whose plan.shipped branch latches the first-plan-shipped
// milestone. A plan whose final open step is dropped completes without
// shipping and mints nothing.
func checkPlanShipped(ctx context.Context, db *sql.DB, rec OnceRecorder, e core.Event) error {
	if payloadString(e.Payload, "to") != string(core.TaskDone) {
		return nil
	}
	var project, plan string
	err := db.QueryRowContext(ctx,
		`SELECT project_slug, plan_slug FROM tasks WHERE id = ?`, e.ItemID).Scan(&project, &plan)
	if errors.Is(err, sql.ErrNoRows) {
		return nil // the task is gone; nothing to judge
	}
	if err != nil {
		return fmt.Errorf("store.CheckMilestones: load task: %w", err)
	}
	if project == "" || plan == "" {
		return nil // not a plan step
	}
	var total, closed int
	if err := db.QueryRowContext(ctx,
		`SELECT COUNT(*), COALESCE(SUM(CASE WHEN status IN (?, ?) THEN 1 ELSE 0 END), 0)
		 FROM tasks WHERE project_slug = ? AND plan_slug = ?`,
		string(core.TaskDone), string(core.TaskDropped), project, plan).Scan(&total, &closed); err != nil {
		return fmt.Errorf("store.CheckMilestones: plan rollup: %w", err)
	}
	if total == 0 || closed < total {
		return nil // steps remain open or in flight
	}
	_, _, err = rec.RecordOnce(ctx, core.Event{
		Kind: core.EventPlanShipped, SessionID: e.SessionID,
		ProjectSlug: project, ItemID: plan,
		Payload: map[string]any{"plan": plan, "project": project, "steps": total},
	})
	if err != nil {
		return fmt.Errorf("store.CheckMilestones: mint plan.shipped %s: %w", plan, err)
	}
	return nil
}

// mintFirstPlanShipped latches the project's first shipped plan, from the
// plan.shipped branch above: the settlement event's own once-ever latch is per
// plan, while this milestone's first-plan-shipped:<project> key is per
// project, so a second shipped plan finds the milestone latch set.
func mintFirstPlanShipped(ctx context.Context, rec OnceRecorder, sessionID, project, plan string, steps int) error {
	claim := fmt.Sprintf("first plan shipped in %s", project)
	if plan != "" {
		claim = fmt.Sprintf("first plan shipped in %s: %s", project, plan)
	}
	payload := map[string]any{"claim": claim, "project": project, "count": 1}
	if plan != "" {
		payload["plan"] = plan
	}
	if steps > 0 {
		payload["steps"] = steps
	}
	key := "first-plan-shipped:" + project
	_, _, err := rec.RecordOnce(ctx, core.Event{
		Kind: EventMilestoneReached, SessionID: sessionID,
		ProjectSlug: project, ItemID: key, Payload: payload,
	})
	if err != nil {
		return fmt.Errorf("store.CheckMilestones: mint %s: %w", key, err)
	}
	return nil
}

// checkProjectBirthday mints a registered project's yearly anniversary on its
// first watched activity at or past the date -- the item key carries the year
// ordinal, so RecordOnce latches each year exactly once, and a dormant year
// simply never mints. An unregistered slug has no registration date and earns
// no birthday (the maturity precedent). Age is judged against the triggering
// event's timestamp with AddDate, never day arithmetic. Every watched event
// of a project past its first year asks, so the year's latch is read before
// anything is written.
func checkProjectBirthday(ctx context.Context, db *sql.DB, rec OnceRecorder, e core.Event) error {
	var createdStr string
	err := db.QueryRowContext(ctx,
		`SELECT created_at FROM projects WHERE slug = ?`, e.ProjectSlug).Scan(&createdStr)
	if errors.Is(err, sql.ErrNoRows) {
		return nil
	}
	if err != nil {
		return fmt.Errorf("store.CheckMilestones: load project: %w", err)
	}
	created, err := core.ParseTime(createdStr)
	if err != nil {
		return fmt.Errorf("store.CheckMilestones: parse project created_at: %w", err)
	}
	now := e.TS
	if now.IsZero() {
		now = time.Now().UTC()
	}
	years := 0
	for !created.AddDate(years+1, 0, 0).After(now) {
		years++
	}
	if years < 1 {
		return nil
	}
	key := fmt.Sprintf("birthday-%d:%s", years, e.ProjectSlug)
	if done, lerr := milestoneLatched(ctx, db, key); lerr != nil || done {
		return lerr
	}
	claim := fmt.Sprintf("%d years of %s", years, e.ProjectSlug)
	if years == 1 {
		claim = "one year of " + e.ProjectSlug
	}
	_, _, err = rec.RecordOnce(ctx, core.Event{
		Kind: EventMilestoneReached, SessionID: e.SessionID,
		ProjectSlug: e.ProjectSlug, ItemID: key,
		Payload: map[string]any{
			"claim":      claim,
			"project":    e.ProjectSlug,
			"years":      years,
			"registered": core.FormatTime(created),
		},
	})
	if err != nil {
		return fmt.Errorf("store.CheckMilestones: mint %s: %w", key, err)
	}
	return nil
}

// payloadString returns the first of the named payload fields holding a
// non-empty string, or "".
func payloadString(p map[string]any, keys ...string) string {
	for _, k := range keys {
		if s, ok := p[k].(string); ok && s != "" {
			return s
		}
	}
	return ""
}

// payloadInt returns the named payload field as an int, or 0 when absent. A
// freshly minted event carries the in-process int; one read back from the DB
// carries JSON's float64. Both are the same number.
func payloadInt(p map[string]any, key string) int {
	switch v := p[key].(type) {
	case int:
		return v
	case int64:
		return int(v)
	case float64:
		return int(v)
	}
	return 0
}
