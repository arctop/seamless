package store

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"fmt"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// The corpus record (migration 029; write screening, part 2): the content hash
// Seamless last wrote for each memory and note file, the review's trust mark,
// and the drift quarantine. internal/screening owns the policy; this file is
// only the rows.

// How a corpus record's hash was recorded.
const (
	CorpusViaWrite  = "write"  // files.Manager wrote the file while the watch was armed
	CorpusViaReview = "review" // the review read the file
	CorpusViaResync = "resync" // the watch was re-armed and read the file
	CorpusViaAccept = "accept" // the owner accepted a drifted file as it stands
)

// Review statuses and who set them.
const (
	ReviewSafe    = "safe"
	ReviewFlagged = "flagged"

	ReviewByReview = "review" // the deterministic review pass
	ReviewByOwner  = "owner"  // the owner marked a flagged item safe
)

// Drift statuses and changes. DriftMissing is never stored: a file that is
// gone has no row to quarantine, so it is reported and its row dropped.
const (
	DriftDrifted = "drifted"
	DriftChanged = "changed" // the file differs from the hash Seamless recorded
	DriftUnknown = "unknown" // no Seamless write is on record for the file
	DriftMissing = "missing" // the file Seamless recorded is gone
)

// Settings keys of the corpus watch. They live in the settings table (the
// owner's settings, exempt from the peer_auth views) rather than a table of
// their own: each is one value.
const (
	// SettingCorpusArmed is "1" while the drift watch is armed. files.Manager
	// records its writes only then, so the record can never claim a write
	// that happened while nobody was recording.
	SettingCorpusArmed = "screening_corpus_armed"
	// SettingCorpusBaseline is when the first review ran (RFC 3339): set once,
	// on the first turn-on of the feature.
	SettingCorpusBaseline = "screening_corpus_baseline"
	// SettingCorpusReviewed is when the review last ran.
	SettingCorpusReviewed = "screening_corpus_reviewed"
	// SettingLockdown is the owner's lockdown switch: "1" while on.
	SettingLockdown = "screening_lockdown"
)

// ErrNoCorpusRecord is returned when a corpus record that must exist does not:
// the file was never recorded, or its record was dropped (it was removed).
var ErrNoCorpusRecord = errors.New("store: no corpus record for that file")

// CorpusRecord is one row of corpus_records.
type CorpusRecord struct {
	FilePath   string
	ItemKind   string
	ItemID     string
	Project    string
	Hash       string
	RecordedAt time.Time
	Via        string

	ReviewStatus   string
	ReviewDigest   string
	ReviewVerdicts []string
	ReviewedAt     *time.Time
	ReviewBy       string

	DriftStatus   string
	DriftChange   string
	DriftHash     string
	DriftBase     string
	DriftVerdicts []string
	DriftedAt     *time.Time
}

// Drifted reports whether the record is in drift quarantine.
func (r CorpusRecord) Drifted() bool { return r.DriftStatus == DriftDrifted }

const corpusCols = `file_path, item_kind, item_id, project_slug, hash, recorded_at, via,
	review_status, review_digest, review_verdicts, reviewed_at, review_by,
	drift_status, drift_change, drift_hash, drift_base, drift_verdicts, drifted_at`

// armedClause is the condition every write-path record statement carries, so
// a write recorded while the watch is disarmed is a no-op in the same
// statement rather than a check-then-act race.
const armedClause = `EXISTS (SELECT 1 FROM settings WHERE key = '` + SettingCorpusArmed + `' AND value = '1')`

// RecordCorpusWrite records the hash of a file files.Manager just wrote. It
// writes nothing while the watch is disarmed. The review and drift fields are
// left as they are: a Seamless write never clears a drift (only the owner's
// accept does) and never confirms a review (the digest no longer matches).
func RecordCorpusWrite(ctx context.Context, db *sql.DB, itemKind, itemID, project, path, hash string, at time.Time) error {
	_, err := db.ExecContext(ctx, `INSERT INTO corpus_records (file_path, item_kind, item_id, project_slug, hash, recorded_at, via)
		SELECT ?, ?, ?, ?, ?, ?, ? WHERE `+armedClause+`
		ON CONFLICT(file_path) DO UPDATE SET item_kind = excluded.item_kind, item_id = excluded.item_id,
			project_slug = excluded.project_slug, hash = excluded.hash, recorded_at = excluded.recorded_at,
			via = excluded.via`,
		path, itemKind, itemID, project, hash, core.FormatTime(at), CorpusViaWrite)
	if err != nil {
		return fmt.Errorf("store.RecordCorpusWrite: %w", err)
	}
	return nil
}

// RecordCorpusRemove drops the record of a file files.Manager just removed.
// It removes nothing while the watch is disarmed (a re-arm resyncs anyway).
func RecordCorpusRemove(ctx context.Context, db *sql.DB, path string) error {
	if _, err := db.ExecContext(ctx, `DELETE FROM corpus_records WHERE file_path = ? AND `+armedClause, path); err != nil {
		return fmt.Errorf("store.RecordCorpusRemove: %w", err)
	}
	return nil
}

// CorpusRecordByPath returns one file's record.
func CorpusRecordByPath(ctx context.Context, db *sql.DB, path string) (CorpusRecord, bool, error) {
	list, err := queryCorpus(ctx, db, `SELECT `+corpusCols+` FROM corpus_records WHERE file_path = ?`, path)
	if err != nil {
		return CorpusRecord{}, false, fmt.Errorf("store.CorpusRecordByPath: %w", err)
	}
	if len(list) == 0 {
		return CorpusRecord{}, false, nil
	}
	return list[0], true, nil
}

// CorpusRecords returns every record, ordered by path.
func CorpusRecords(ctx context.Context, db *sql.DB) ([]CorpusRecord, error) {
	list, err := queryCorpus(ctx, db, `SELECT `+corpusCols+` FROM corpus_records ORDER BY file_path`)
	if err != nil {
		return nil, fmt.Errorf("store.CorpusRecords: %w", err)
	}
	return list, nil
}

// FlaggedCorpusRecords returns the records the review flagged, newest review
// first.
func FlaggedCorpusRecords(ctx context.Context, db *sql.DB) ([]CorpusRecord, error) {
	list, err := queryCorpus(ctx, db, `SELECT `+corpusCols+` FROM corpus_records
		WHERE review_status = ? ORDER BY reviewed_at DESC, file_path`, ReviewFlagged)
	if err != nil {
		return nil, fmt.Errorf("store.FlaggedCorpusRecords: %w", err)
	}
	return list, nil
}

// DriftedCorpusRecords returns the records in drift quarantine, newest first.
func DriftedCorpusRecords(ctx context.Context, db *sql.DB) ([]CorpusRecord, error) {
	list, err := queryCorpus(ctx, db, `SELECT `+corpusCols+` FROM corpus_records
		WHERE drift_status = ? ORDER BY drifted_at DESC, file_path`, DriftDrifted)
	if err != nil {
		return nil, fmt.Errorf("store.DriftedCorpusRecords: %w", err)
	}
	return list, nil
}

// CorpusRecordsByItem returns the record of each of ids that has one, keyed
// by item id. A memory that sits at two paths (a planted copy) keeps the
// drifted one, so a quarantine is never hidden by its twin.
func CorpusRecordsByItem(ctx context.Context, db *sql.DB, ids []string) (map[string]CorpusRecord, error) {
	out := make(map[string]CorpusRecord)
	for chunk := range chunked(ids, sqlInChunk) {
		list, err := queryCorpus(ctx, db, `SELECT `+corpusCols+` FROM corpus_records
			WHERE item_id IN (`+placeholders(len(chunk))+`)`, anySlice(chunk)...)
		if err != nil {
			return nil, fmt.Errorf("store.CorpusRecordsByItem: %w", err)
		}
		for _, r := range list {
			if prev, ok := out[r.ItemID]; ok && prev.Drifted() {
				continue
			}
			out[r.ItemID] = r
		}
	}
	return out, nil
}

// DriftedItemIDs returns the ids of every item in drift quarantine.
func DriftedItemIDs(ctx context.Context, db *sql.DB) (map[string]bool, error) {
	out := make(map[string]bool)
	err := eachRow(ctx, db, `SELECT item_id FROM corpus_records WHERE drift_status = ? AND item_id != ''`,
		[]any{DriftDrifted}, func(rows *sql.Rows) error {
			var id string
			if err := rows.Scan(&id); err != nil {
				return err
			}
			out[id] = true
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("store.DriftedItemIDs: %w", err)
	}
	return out, nil
}

// SetCorpusHash records a file's current hash (the review or a resync read
// it), creating the row when there is none. Review and drift fields are left
// as they are.
func SetCorpusHash(ctx context.Context, db *sql.DB, path, itemKind, itemID, project, hash, via string, at time.Time) error {
	_, err := db.ExecContext(ctx, `INSERT INTO corpus_records (file_path, item_kind, item_id, project_slug, hash, recorded_at, via)
		VALUES (?, ?, ?, ?, ?, ?, ?)
		ON CONFLICT(file_path) DO UPDATE SET item_kind = excluded.item_kind, item_id = excluded.item_id,
			project_slug = excluded.project_slug, hash = excluded.hash, recorded_at = excluded.recorded_at,
			via = excluded.via`,
		path, itemKind, itemID, project, hash, core.FormatTime(at), via)
	if err != nil {
		return fmt.Errorf("store.SetCorpusHash: %w", err)
	}
	return nil
}

// SetCorpusReview stores the review's verdict on a file. The row must exist
// (the review records the hash first).
func SetCorpusReview(ctx context.Context, db *sql.DB, path, status, digest string, verdicts []string, by string, at time.Time) error {
	if status != ReviewSafe && status != ReviewFlagged {
		return fmt.Errorf("store.SetCorpusReview: invalid status %q: valid values are %s, %s", status, ReviewSafe, ReviewFlagged)
	}
	v, err := json.Marshal(nonNilStrings(verdicts))
	if err != nil {
		return fmt.Errorf("store.SetCorpusReview: marshal verdicts: %w", err)
	}
	res, err := db.ExecContext(ctx, `UPDATE corpus_records SET review_status = ?, review_digest = ?,
		review_verdicts = ?, reviewed_at = ?, review_by = ? WHERE file_path = ?`,
		status, digest, string(v), core.FormatTime(at), by, path)
	if err != nil {
		return fmt.Errorf("store.SetCorpusReview: %w", err)
	}
	return requireOneRow(res, "store.SetCorpusReview")
}

// SetCorpusDrift puts a file in drift quarantine, or refreshes the quarantine
// of one whose bytes changed again. drift_base keeps the hash the file was
// recorded at when the drift was first found. A file with no row yet (one no
// Seamless write is on record for) gets one, with an empty hash.
func SetCorpusDrift(ctx context.Context, db *sql.DB, path, itemKind, itemID, project, change, driftHash string,
	verdicts []string, at time.Time) error {
	v, err := json.Marshal(nonNilStrings(verdicts))
	if err != nil {
		return fmt.Errorf("store.SetCorpusDrift: marshal verdicts: %w", err)
	}
	ts := core.FormatTime(at)
	_, err = db.ExecContext(ctx, `INSERT INTO corpus_records (file_path, item_kind, item_id, project_slug, hash,
			recorded_at, via, drift_status, drift_change, drift_hash, drift_base, drift_verdicts, drifted_at)
		VALUES (?, ?, ?, ?, '', ?, '', ?, ?, ?, '', ?, ?)
		ON CONFLICT(file_path) DO UPDATE SET item_kind = excluded.item_kind, item_id = excluded.item_id,
			project_slug = excluded.project_slug,
			drift_base = CASE WHEN corpus_records.drift_status = ? THEN corpus_records.drift_base ELSE corpus_records.hash END,
			drift_status = excluded.drift_status, drift_change = excluded.drift_change,
			drift_hash = excluded.drift_hash, drift_verdicts = excluded.drift_verdicts,
			drifted_at = excluded.drifted_at`,
		path, itemKind, itemID, project, ts, DriftDrifted, change, driftHash, string(v), ts, DriftDrifted)
	if err != nil {
		return fmt.Errorf("store.SetCorpusDrift: %w", err)
	}
	return nil
}

// ClearCorpusDrift lifts a file's drift quarantine and records hash as the
// file's hash: the owner accepted it as it stands (via accept), or the file
// went back to the bytes it was recorded at.
func ClearCorpusDrift(ctx context.Context, db *sql.DB, path, hash, via string, at time.Time) error {
	res, err := db.ExecContext(ctx, `UPDATE corpus_records SET hash = ?, recorded_at = ?, via = ?,
		drift_status = '', drift_change = '', drift_hash = '', drift_base = '', drift_verdicts = '[]', drifted_at = ''
		WHERE file_path = ?`, hash, core.FormatTime(at), via, path)
	if err != nil {
		return fmt.Errorf("store.ClearCorpusDrift: %w", err)
	}
	return requireOneRow(res, "store.ClearCorpusDrift")
}

// DeleteCorpusRecord drops one file's record.
func DeleteCorpusRecord(ctx context.Context, db *sql.DB, path string) error {
	if _, err := db.ExecContext(ctx, `DELETE FROM corpus_records WHERE file_path = ?`, path); err != nil {
		return fmt.Errorf("store.DeleteCorpusRecord: %w", err)
	}
	return nil
}

// CorpusCounts summarizes the record for the console.
type CorpusCounts struct {
	Items      int
	Safe       int
	Flagged    int
	Unreviewed int
	Drifted    int
}

// CountCorpus counts the record's rows by review and drift status.
func CountCorpus(ctx context.Context, db *sql.DB) (CorpusCounts, error) {
	var c CorpusCounts
	err := db.QueryRowContext(ctx, `SELECT COUNT(*),
		COALESCE(SUM(review_status = 'safe'), 0), COALESCE(SUM(review_status = 'flagged'), 0),
		COALESCE(SUM(review_status = ''), 0), COALESCE(SUM(drift_status = 'drifted'), 0)
		FROM corpus_records`).Scan(&c.Items, &c.Safe, &c.Flagged, &c.Unreviewed, &c.Drifted)
	if err != nil {
		return CorpusCounts{}, fmt.Errorf("store.CountCorpus: %w", err)
	}
	return c, nil
}

// CorpusArmed reports whether the drift watch is armed.
func CorpusArmed(ctx context.Context, db *sql.DB) (bool, error) {
	v, ok, err := GetSetting(ctx, db, SettingCorpusArmed)
	if err != nil {
		return false, err
	}
	return ok && v == "1", nil
}

// SetCorpusArmed arms or disarms the drift watch.
func SetCorpusArmed(ctx context.Context, db *sql.DB, armed bool) error {
	if armed {
		return SetSetting(ctx, db, SettingCorpusArmed, "1")
	}
	return DeleteSetting(ctx, db, SettingCorpusArmed)
}

// SettingTime reads a settings key holding an RFC 3339 time. found is false
// when unset.
func SettingTime(ctx context.Context, db *sql.DB, key string) (time.Time, bool, error) {
	v, ok, err := GetSetting(ctx, db, key)
	if err != nil || !ok {
		return time.Time{}, false, err
	}
	t, err := core.ParseTime(v)
	if err != nil {
		return time.Time{}, false, fmt.Errorf("store.SettingTime: %s: %w", key, err)
	}
	return t, true, nil
}

// SetSettingTime stores an RFC 3339 time under a settings key.
func SetSettingTime(ctx context.Context, db *sql.DB, key string, at time.Time) error {
	return SetSetting(ctx, db, key, core.FormatTime(at))
}

// LockdownOn reports whether the owner's lockdown switch is on. It is only
// half of the answer: internal/screening also reads the configured flag file.
func LockdownOn(ctx context.Context, db *sql.DB) (bool, error) {
	v, ok, err := GetSetting(ctx, db, SettingLockdown)
	if err != nil {
		return false, err
	}
	return ok && v == "1", nil
}

// SetLockdown sets or clears the owner's lockdown switch.
func SetLockdown(ctx context.Context, db *sql.DB, on bool) error {
	if on {
		return SetSetting(ctx, db, SettingLockdown, "1")
	}
	return DeleteSetting(ctx, db, SettingLockdown)
}

// InjectedSession is one session that received an item through an injection
// (a briefing, a prompt-matched recall, a recall tool answer).
type InjectedSession struct {
	SessionID string
	Name      string
	Project   string
	Times     int
	First     time.Time
	Last      time.Time
}

// InjectedSessions answers the forensic question "which sessions received
// item id": every retrieval.injected event naming it, in the item_id column
// or the payload's item_ids array, grouped by session, most recent first.
// retrieval.injected is a domain event and no retention pass deletes it, so
// the answer reaches back to the first injection on record. An injection that
// ran in no session is grouped under "".
func InjectedSessions(ctx context.Context, db *sql.DB, itemID string, limit int) ([]InjectedSession, error) {
	if itemID == "" {
		return nil, nil
	}
	if limit <= 0 {
		limit = 50
	}
	var out []InjectedSession
	err := eachRow(ctx, db, `SELECT x.session_id, COALESCE(s.name, ''), COALESCE(s.project_slug, ''),
			COUNT(*), MIN(x.ts), MAX(x.ts)
		FROM (
			SELECT e.id, e.session_id, e.ts FROM events e
			WHERE e.kind = ? AND (e.item_id = ? OR EXISTS (
				SELECT 1 FROM json_each(CASE WHEN json_valid(e.payload) THEN e.payload ELSE '{}' END, '$.item_ids') j
				WHERE j.value = ?))
		) x LEFT JOIN sessions s ON s.id = x.session_id
		GROUP BY x.session_id ORDER BY MAX(x.ts) DESC LIMIT ?`,
		[]any{string(core.EventInjected), itemID, itemID, limit}, func(rows *sql.Rows) error {
			var r InjectedSession
			var first, last string
			if err := rows.Scan(&r.SessionID, &r.Name, &r.Project, &r.Times, &first, &last); err != nil {
				return err
			}
			var err error
			if r.First, err = core.ParseTime(first); err != nil {
				return err
			}
			if r.Last, err = core.ParseTime(last); err != nil {
				return err
			}
			out = append(out, r)
			return nil
		})
	if err != nil {
		return nil, fmt.Errorf("store.InjectedSessions: %w", err)
	}
	return out, nil
}

func requireOneRow(res sql.Result, op string) error {
	n, err := res.RowsAffected()
	if err != nil {
		return fmt.Errorf("%s: rows affected: %w", op, err)
	}
	if n == 0 {
		return fmt.Errorf("%s: %w", op, ErrNoCorpusRecord)
	}
	return nil
}

func queryCorpus(ctx context.Context, db *sql.DB, query string, args ...any) ([]CorpusRecord, error) {
	var out []CorpusRecord
	err := eachRow(ctx, db, query, args, func(rows *sql.Rows) error {
		var r CorpusRecord
		var recorded, reviewVerdicts, reviewedAt, driftVerdicts, driftedAt string
		if err := rows.Scan(&r.FilePath, &r.ItemKind, &r.ItemID, &r.Project, &r.Hash, &recorded, &r.Via,
			&r.ReviewStatus, &r.ReviewDigest, &reviewVerdicts, &reviewedAt, &r.ReviewBy,
			&r.DriftStatus, &r.DriftChange, &r.DriftHash, &r.DriftBase, &driftVerdicts, &driftedAt); err != nil {
			return err
		}
		t, err := core.ParseTime(recorded)
		if err != nil {
			return fmt.Errorf("corpus record %s: recorded_at: %w", r.FilePath, err)
		}
		r.RecordedAt = t
		if r.ReviewedAt, err = optionalTime(reviewedAt); err != nil {
			return fmt.Errorf("corpus record %s: reviewed_at: %w", r.FilePath, err)
		}
		if r.DriftedAt, err = optionalTime(driftedAt); err != nil {
			return fmt.Errorf("corpus record %s: drifted_at: %w", r.FilePath, err)
		}
		if err := json.Unmarshal([]byte(reviewVerdicts), &r.ReviewVerdicts); err != nil {
			return fmt.Errorf("corpus record %s: review_verdicts: %w", r.FilePath, err)
		}
		if err := json.Unmarshal([]byte(driftVerdicts), &r.DriftVerdicts); err != nil {
			return fmt.Errorf("corpus record %s: drift_verdicts: %w", r.FilePath, err)
		}
		out = append(out, r)
		return nil
	})
	return out, err
}

func optionalTime(s string) (*time.Time, error) {
	if s == "" {
		return nil, nil
	}
	t, err := core.ParseTime(s)
	if err != nil {
		return nil, err
	}
	return &t, nil
}
