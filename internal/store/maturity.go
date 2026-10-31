// Momentum: latched project maturity stages.
//
// Each project earns a stage -- seedling, sprouting, established, deep-rooted
// -- from real thresholds over age, memory count, event volume, and reach,
// following the utility-activation pattern: exported threshold constants (so
// the computation and the console's "why not yet" tooltip agree by
// construction) and a stored latch that never flaps. Stages never regress: a
// project that sheds memories keeps the stage it earned, and a crossing is
// minted exactly once.
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

// ProjectStage is a project's latched maturity stage.
type ProjectStage string

const (
	StageSeedling    ProjectStage = "seedling"
	StageSprouting   ProjectStage = "sprouting"
	StageEstablished ProjectStage = "established"
	StageDeepRooted  ProjectStage = "deep-rooted"
)

// Rank orders the stages for the never-regress comparison. Unknown reads as
// seedling, so a corrupt stored value can only under-report, never invent.
func (s ProjectStage) Rank() int {
	switch s {
	case StageSprouting:
		return 1
	case StageEstablished:
		return 2
	case StageDeepRooted:
		return 3
	default:
		return 0
	}
}

// Maturity thresholds. A stage requires EVERY listed bar; age counts from the
// project's registration (projects.created_at -- an unregistered slug stays a
// seedling), volume from the project's total event count, and deep-rooted
// additionally asks that knowledge demonstrably reaches the work (windowed
// reach at evaluation time; the latch keeps the stage once earned).
const (
	StageSproutingMinAgeDays  = 7
	StageSproutingMinMemories = 5
	StageSproutingMinEvents   = 25

	StageEstablishedMinAgeDays  = 30
	StageEstablishedMinMemories = 15
	StageEstablishedMinEvents   = 250

	StageDeepRootedMinAgeDays  = 90
	StageDeepRootedMinMemories = 40
	StageDeepRootedMinEvents   = 1000
	StageDeepRootedMinReachPct = 40
)

// StageInputs are one project's maturity facts at evaluation time.
type StageInputs struct {
	CreatedAt time.Time `json:"createdAt"` // registration; zero = unregistered
	Memories  int       `json:"memories"`  // active memories
	Events    int       `json:"events"`    // total event volume, all kinds
	ReachRate int       `json:"reachRate"` // windowed surfaced/active %, when HasReach
	HasReach  bool      `json:"hasReach"`
}

// AgeDays is the whole days since registration (0 for an unregistered slug).
func (in StageInputs) AgeDays(now time.Time) int {
	if in.CreatedAt.IsZero() {
		return 0
	}
	if d := int(now.Sub(in.CreatedAt).Hours() / 24); d > 0 {
		return d
	}
	return 0
}

// ComputeStage evaluates the highest stage these inputs satisfy right now,
// with no latch applied.
func ComputeStage(in StageInputs, now time.Time) ProjectStage {
	age := in.AgeDays(now)
	switch {
	case age >= StageDeepRootedMinAgeDays && in.Memories >= StageDeepRootedMinMemories &&
		in.Events >= StageDeepRootedMinEvents && in.HasReach && in.ReachRate >= StageDeepRootedMinReachPct:
		return StageDeepRooted
	case age >= StageEstablishedMinAgeDays && in.Memories >= StageEstablishedMinMemories &&
		in.Events >= StageEstablishedMinEvents:
		return StageEstablished
	case age >= StageSproutingMinAgeDays && in.Memories >= StageSproutingMinMemories &&
		in.Events >= StageSproutingMinEvents:
		return StageSprouting
	default:
		return StageSeedling
	}
}

// SettingProjectStages is the settings key holding the latched stage per
// project: {"projects":{"slug":{"stage":"sprouting","since":"..."}}}. Deleting
// the row is safe in the sense that nothing breaks -- but unlike a cache it IS
// the latch, so earned stages would be recomputed from current facts.
const SettingProjectStages = "momentum_project_stages"

// projectStageState is one project's stored latch.
type projectStageState struct {
	Stage ProjectStage `json:"stage"`
	Since time.Time    `json:"since"`
}

// projectStagesDoc is the stored shape behind SettingProjectStages.
type projectStagesDoc struct {
	Projects map[string]projectStageState `json:"projects"`
}

// ProjectStageInfo is one project's effective stage plus the inputs it was
// judged on, so the console can say why the next stage has not been reached.
type ProjectStageInfo struct {
	Stage  ProjectStage `json:"stage"`
	Since  time.Time    `json:"since,omitzero"`
	Inputs StageInputs  `json:"inputs"`
}

// StageCrossing is a latch advance minted by one evaluation.
type StageCrossing struct {
	Project string
	From    ProjectStage
	To      ProjectStage
}

// EvaluateProjectStages computes the stage for every named project on the
// board, latches forward (never back), persists any advances, and returns the
// effective stages plus the crossings this evaluation minted (the caller
// records those as events). The global "" scope is not a project and earns no
// stage. A momentum-only surface: callers gate on the feature, so a disabled
// install neither computes nor latches.
func EvaluateProjectStages(ctx context.Context, db *sql.DB, rows []ProjectBoardRow, now time.Time) (map[string]ProjectStageInfo, []StageCrossing, error) {
	return EvaluateProjectStagesWith(ctx, db, db, rows, now)
}

// EvaluateProjectStagesWith is EvaluateProjectStages reading through db and
// writing the latch document through cacheDB (see CaptureStreakWith).
func EvaluateProjectStagesWith(ctx context.Context, db, cacheDB *sql.DB, rows []ProjectBoardRow, now time.Time) (map[string]ProjectStageInfo, []StageCrossing, error) {
	doc, err := loadProjectStages(ctx, db)
	if err != nil {
		return nil, nil, err
	}
	created, err := projectCreatedAt(ctx, db)
	if err != nil {
		return nil, nil, err
	}
	events, err := eventCountByProject(ctx, db)
	if err != nil {
		return nil, nil, err
	}

	out := make(map[string]ProjectStageInfo, len(rows))
	var crossings []StageCrossing
	changed := false
	for _, row := range rows {
		if row.Project == "" {
			continue
		}
		in := StageInputs{
			CreatedAt: created[row.Project],
			Memories:  row.Memories,
			Events:    events[row.Project],
			ReachRate: row.ReachRate,
			HasReach:  row.Active > 0,
		}
		latched := doc.Projects[row.Project]
		effective := latched.Stage
		if computed := ComputeStage(in, now); computed.Rank() > latched.Stage.Rank() {
			from := latched.Stage
			if from == "" {
				from = StageSeedling
			}
			effective = computed
			doc.Projects[row.Project] = projectStageState{Stage: computed, Since: now.UTC()}
			changed = true
			// Reaching seedling is not a crossing -- every project is born there.
			if computed != StageSeedling {
				crossings = append(crossings, StageCrossing{Project: row.Project, From: from, To: computed})
			}
		}
		if effective == "" {
			effective = StageSeedling
		}
		out[row.Project] = ProjectStageInfo{Stage: effective, Since: doc.Projects[row.Project].Since, Inputs: in}
	}
	if changed {
		raw, err := json.Marshal(doc)
		if err != nil {
			return nil, nil, fmt.Errorf("store.EvaluateProjectStages: encode: %w", err)
		}
		if err := SetSetting(ctx, cacheDB, SettingProjectStages, string(raw)); err != nil {
			return nil, nil, err
		}
	}
	return out, crossings, nil
}

// loadProjectStages reads the latch document; an absent or corrupt row is an
// empty latch (stages recompute and re-latch), never a failed page.
func loadProjectStages(ctx context.Context, db *sql.DB) (projectStagesDoc, error) {
	doc := projectStagesDoc{Projects: map[string]projectStageState{}}
	raw, found, err := GetSetting(ctx, db, SettingProjectStages)
	if err != nil {
		return doc, fmt.Errorf("store.EvaluateProjectStages: %w", err)
	}
	if !found || strings.TrimSpace(raw) == "" {
		return doc, nil
	}
	if err := json.Unmarshal([]byte(raw), &doc); err != nil || doc.Projects == nil {
		return projectStagesDoc{Projects: map[string]projectStageState{}}, nil
	}
	return doc, nil
}

// projectCreatedAt maps registered project slugs to their registration time.
func projectCreatedAt(ctx context.Context, db *sql.DB) (map[string]time.Time, error) {
	rows, err := db.QueryContext(ctx, `SELECT slug, created_at FROM projects`)
	if err != nil {
		return nil, fmt.Errorf("store.projectCreatedAt: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]time.Time{}
	for rows.Next() {
		var slug, createdAt string
		if err := rows.Scan(&slug, &createdAt); err != nil {
			return nil, fmt.Errorf("store.projectCreatedAt: scan: %w", err)
		}
		ts, err := core.ParseTime(createdAt)
		if err != nil {
			return nil, fmt.Errorf("store.projectCreatedAt: parse: %w", err)
		}
		out[slug] = ts
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.projectCreatedAt: %w", err)
	}
	return out, nil
}

// eventCountByProject counts all events per project slug -- the volume input.
func eventCountByProject(ctx context.Context, db *sql.DB) (map[string]int, error) {
	rows, err := db.QueryContext(ctx, `SELECT project_slug, COUNT(*) FROM events GROUP BY project_slug`)
	if err != nil {
		return nil, fmt.Errorf("store.eventCountByProject: %w", err)
	}
	defer func() { _ = rows.Close() }()
	out := map[string]int{}
	for rows.Next() {
		var slug string
		var n int
		if err := rows.Scan(&slug, &n); err != nil {
			return nil, fmt.Errorf("store.eventCountByProject: scan: %w", err)
		}
		out[slug] = n
	}
	if err := rows.Err(); err != nil {
		return nil, fmt.Errorf("store.eventCountByProject: %w", err)
	}
	return out, nil
}
