package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"reflect"
	"sort"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
)

const (
	// DefaultTrialQueryLimit is used when TrialFilter.Limit is left at zero.
	DefaultTrialQueryLimit = 20
	// MaxTrialQueryLimit bounds both the result slice and the metrics-filter
	// over-fetch query. The console's largest reader asks for 200 rows.
	MaxTrialQueryLimit    = 200
	trialMetricsOverfetch = 8
)

// ErrInvalidTrialQueryLimit marks a TrialFilter limit outside the store API.
var ErrInvalidTrialQueryLimit = errors.New("invalid trial query limit")

// trialCols is the SELECT list for the trials table, matching scanTrial.
const trialCols = `id, lab, title, changes, expected, actual, outcome, metrics,
	session_id, project_slug, favorite, created_at, account`

// TrialFilter parameterizes QueryTrials. Empty string fields are not filtered;
// MetricsEquals matches trials whose metrics contain each given key with an
// equal value (compared after JSON normalization). A zero Limit uses
// DefaultTrialQueryLimit; negative or oversized values are rejected.
type TrialFilter struct {
	Lab           string
	Outcome       string
	Project       string
	SessionID     string
	MetricsEquals map[string]any
	Limit         int
}

// CreateTrial inserts a trial row. The caller mints the ULID id and timestamp.
// The trial's account is the restricted account the request in ctx speaks for
// (access.AccountOf) unless the caller set one.
func CreateTrial(ctx context.Context, db *sql.DB, tr core.Trial) error {
	if tr.Account == "" {
		tr.Account = access.AccountOf(ctx)
	}
	metrics, err := marshalMetrics(tr.Metrics)
	if err != nil {
		return fmt.Errorf("store.CreateTrial: %w", err)
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO trials (id, lab, title, changes, expected, actual, outcome,
		                    metrics, session_id, project_slug, created_at, account)
		VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		tr.ID, tr.Lab, tr.Title, tr.Changes, tr.Expected, tr.Actual, string(tr.Outcome),
		metrics, tr.SessionID, tr.ProjectSlug, core.FormatTime(tr.CreatedAt), tr.Account)
	if err != nil {
		return fmt.Errorf("store.CreateTrial: %w", err)
	}
	if err := IndexTrialFTS(ctx, db, tr); err != nil {
		return fmt.Errorf("store.CreateTrial: %w", err)
	}
	return nil
}

// QueryTrials returns trials matching the filter, newest first. Lab/outcome/
// project are filtered in SQL; MetricsEquals is applied in Go (labs are small,
// and this matches the DB-first metrics design without a JSON1 dependency).
func QueryTrials(ctx context.Context, db *sql.DB, f TrialFilter) ([]core.Trial, error) {
	limit := f.Limit
	if limit == 0 {
		limit = DefaultTrialQueryLimit
	}
	if limit < 1 || limit > MaxTrialQueryLimit {
		return nil, fmt.Errorf("store.QueryTrials: %w: got %d, want 1-%d (or 0 for the default)",
			ErrInvalidTrialQueryLimit, f.Limit, MaxTrialQueryLimit)
	}
	query := `SELECT ` + trialCols + ` FROM trials WHERE 1=1`
	var args []any
	if f.Lab != "" {
		query += ` AND lab = ?`
		args = append(args, f.Lab)
	}
	if f.Outcome != "" {
		query += ` AND outcome = ?`
		args = append(args, f.Outcome)
	}
	if f.Project != "" {
		query += ` AND project_slug = ?`
		args = append(args, f.Project)
	}
	if f.SessionID != "" {
		query += ` AND session_id = ?`
		args = append(args, f.SessionID)
	}
	// Over-fetch when filtering metrics in Go so the limit still returns enough
	// matches; the metrics filter runs after the SQL cut.
	sqlLimit := limit
	if len(f.MetricsEquals) > 0 {
		sqlLimit = limit * trialMetricsOverfetch
	}
	query += ` ORDER BY created_at DESC, id DESC LIMIT ?`
	args = append(args, sqlLimit)

	rows, err := db.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, fmt.Errorf("store.QueryTrials: %w", err)
	}
	defer func() { _ = rows.Close() }()

	out := make([]core.Trial, 0, limit)
	for rows.Next() {
		tr, err := scanTrial(rows)
		if err != nil {
			return nil, fmt.Errorf("store.QueryTrials: %w", err)
		}
		if !metricsMatch(tr.Metrics, f.MetricsEquals) {
			continue
		}
		out = append(out, tr)
		if len(out) >= limit {
			break
		}
	}
	return out, rows.Err()
}

// TrialByID loads one trial. found=false means no such trial.
func TrialByID(ctx context.Context, db *sql.DB, id string) (core.Trial, bool, error) {
	rows, err := db.QueryContext(ctx, `SELECT `+trialCols+` FROM trials WHERE id = ?`, id)
	if err != nil {
		return core.Trial{}, false, fmt.Errorf("store.TrialByID: %w", err)
	}
	defer func() { _ = rows.Close() }()
	if !rows.Next() {
		if err := rows.Err(); err != nil {
			return core.Trial{}, false, fmt.Errorf("store.TrialByID: %w", err)
		}
		return core.Trial{}, false, nil
	}
	tr, err := scanTrial(rows)
	if err != nil {
		return core.Trial{}, false, fmt.Errorf("store.TrialByID: %w", err)
	}
	return tr, true, rows.Err()
}

// LabSummary aggregates one lab's trials for the console: outcome tallies over
// the conventional values (anything else -- including an empty outcome -- lands
// in Other), the distinct projects and sessions its trials touched, and the
// first/last trial stamps. A lab exists only as the label its trials carry, so
// this is the lab's whole identity.
type LabSummary struct {
	Lab          string    `json:"lab"`
	Trials       int       `json:"trials"`
	Pass         int       `json:"pass"`
	Fail         int       `json:"fail"`
	Partial      int       `json:"partial"`
	Inconclusive int       `json:"inconclusive"`
	Other        int       `json:"other"`
	Projects     []string  `json:"projects"` // distinct project slugs ("" = global), sorted
	Sessions     int       `json:"sessions"` // distinct recording sessions
	FirstAt      time.Time `json:"firstAt"`
	LastAt       time.Time `json:"lastAt"`
}

// ListLabs returns every lab with its aggregate summary, most recently active
// first (ties broken by name). Labs are few -- one per line of investigation --
// so this is a single GROUP BY over trials with no pagination.
func ListLabs(ctx context.Context, db *sql.DB) ([]LabSummary, error) {
	rows, err := db.QueryContext(ctx, `
		SELECT lab, COUNT(*),
			SUM(CASE WHEN outcome = 'pass' THEN 1 ELSE 0 END),
			SUM(CASE WHEN outcome = 'fail' THEN 1 ELSE 0 END),
			SUM(CASE WHEN outcome = 'partial' THEN 1 ELSE 0 END),
			SUM(CASE WHEN outcome = 'inconclusive' THEN 1 ELSE 0 END),
			COUNT(DISTINCT CASE WHEN session_id <> '' THEN session_id END),
			GROUP_CONCAT(DISTINCT project_slug),
			MIN(created_at), MAX(created_at)
		FROM trials GROUP BY lab
		ORDER BY MAX(created_at) DESC, lab ASC`)
	if err != nil {
		return nil, fmt.Errorf("store.ListLabs: %w", err)
	}
	defer func() { _ = rows.Close() }()

	var out []LabSummary
	for rows.Next() {
		var (
			l               LabSummary
			projects        sql.NullString
			firstAt, lastAt string
		)
		if err := rows.Scan(&l.Lab, &l.Trials, &l.Pass, &l.Fail, &l.Partial,
			&l.Inconclusive, &l.Sessions, &projects, &firstAt, &lastAt); err != nil {
			return nil, fmt.Errorf("store.ListLabs: scan: %w", err)
		}
		l.Other = l.Trials - l.Pass - l.Fail - l.Partial - l.Inconclusive
		// GROUP_CONCAT joins with "," and slugs cannot contain one
		// (validate.Name); "" survives the split as the global scope.
		l.Projects = strings.Split(projects.String, ",")
		sort.Strings(l.Projects)
		if l.FirstAt, err = core.ParseTime(firstAt); err != nil {
			return nil, fmt.Errorf("store.ListLabs: first_at: %w", err)
		}
		if l.LastAt, err = core.ParseTime(lastAt); err != nil {
			return nil, fmt.Errorf("store.ListLabs: last_at: %w", err)
		}
		out = append(out, l)
	}
	return out, rows.Err()
}

// metricsMatch reports whether metrics contains every key in want with an equal
// value. An empty want matches everything. Values are compared after JSON
// normalization so 497 (int literal) equals 497.0 (float).
func metricsMatch(metrics, want map[string]any) bool {
	for k, wv := range want {
		mv, ok := metrics[k]
		if !ok || !jsonEqual(mv, wv) {
			return false
		}
	}
	return true
}

// jsonEqual compares two decoded-JSON values by re-encoding, so numeric literals
// of different Go types (int vs float64) that render identically compare equal.
func jsonEqual(a, b any) bool {
	ba, err1 := json.Marshal(a)
	bb, err2 := json.Marshal(b)
	if err1 != nil || err2 != nil {
		return reflect.DeepEqual(a, b)
	}
	return string(ba) == string(bb)
}

func scanTrial(rows *sql.Rows) (core.Trial, error) {
	var (
		tr      core.Trial
		outcome string
		metrics string
		created string
	)
	if err := rows.Scan(&tr.ID, &tr.Lab, &tr.Title, &tr.Changes, &tr.Expected,
		&tr.Actual, &outcome, &metrics, &tr.SessionID, &tr.ProjectSlug, &tr.Favorite, &created, &tr.Account); err != nil {
		return core.Trial{}, err
	}
	tr.Outcome = core.TrialOutcome(outcome)
	if metrics != "" && metrics != "{}" {
		if err := json.Unmarshal([]byte(metrics), &tr.Metrics); err != nil {
			return core.Trial{}, fmt.Errorf("metrics: %w", err)
		}
	}
	var err error
	if tr.CreatedAt, err = core.ParseTime(created); err != nil {
		return core.Trial{}, fmt.Errorf("created_at: %w", err)
	}
	return tr, nil
}

// marshalMetrics serializes a metrics map to a JSON object string ("{}" when
// empty), matching the trials.metrics column default.
func marshalMetrics(m map[string]any) (string, error) {
	if len(m) == 0 {
		return "{}", nil
	}
	b, err := json.Marshal(m)
	if err != nil {
		return "", fmt.Errorf("marshal metrics: %w", err)
	}
	return string(b), nil
}
