package update

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"maps"
	"os"
	"path/filepath"
	"runtime"
	"slices"
	"strings"
	"testing"
	"time"
	"unicode/utf8"

	"github.com/arctop/seamless/internal/core"
	"github.com/stretchr/testify/require"
)

// newAttemptID mints an attempt id the way the checker does.
func newAttemptID(t *testing.T) string {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	return id
}

// runningAttempt is an attempt in the middle of its install, heartbeat at now.
func runningAttempt(t *testing.T, now time.Time) Attempt {
	t.Helper()
	return Attempt{
		ID: newAttemptID(t), From: Version{0, 7, 2}, To: Version{0, 7, 3}, Why: WhyAuto,
		StartedAt: now.Add(-2 * time.Minute), HeartbeatAt: now, Stage: StageInstall,
		LogPath:    filepath.Join("data", "update", "attempt.log"),
		BackupPath: filepath.Join("data", "update", "backup"),
	}
}

// succeeded is a finished with a confirmed install at at.
func succeeded(a Attempt, at time.Time) Attempt {
	a.Stage, a.OK, a.FinishedAt, a.HeartbeatAt = StageDone, true, at, at
	return a
}

// failedAt is a finished with a failure in stage s at at.
func failedAt(a Attempt, s Stage, at time.Time) Attempt {
	a.Stage, a.FinishedAt, a.HeartbeatAt, a.Error = s, at, at, "it did not work"
	return a
}

var attemptNow = time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)

func TestAttemptPaths(t *testing.T) {
	dir := filepath.Join("srv", "seamless")
	require.Equal(t, filepath.Join(dir, "update", "attempt.json"), AttemptPath(dir))
	require.Equal(t, filepath.Join(dir, "update", "attempts.jsonl"), AttemptHistoryPath(dir))
	require.Equal(t, filepath.Join(dir, "update", "update.lock"), LockPath(dir))
}

func TestAttempt_RoundTrip(t *testing.T) {
	dir := t.TempDir()
	a := runningAttempt(t, attemptNow)
	require.NoError(t, WriteAttempt(dir, a))
	got, err := ReadAttempt(dir)
	require.NoError(t, err)
	require.Equal(t, a, got)

	// A heartbeat and then the outcome replace it in place.
	a.HeartbeatAt = attemptNow.Add(AttemptHeartbeatInterval)
	require.NoError(t, WriteAttempt(dir, a))
	fin := succeeded(a, attemptNow.Add(3*time.Minute))
	require.NoError(t, WriteAttempt(dir, fin))
	got, err = ReadAttempt(dir)
	require.NoError(t, err)
	require.Equal(t, fin, got)

	entries, err := os.ReadDir(StateDir(dir))
	require.NoError(t, err)
	require.Len(t, entries, 1, "no temp file left behind")

	if runtime.GOOS != "windows" {
		fi, err := os.Stat(AttemptPath(dir))
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
		di, err := os.Stat(StateDir(dir))
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o700), di.Mode().Perm())
	}
}

// The JSON names are what every release reads every other one's files by:
// renaming one breaks reading across versions, so they are pinned here.
func TestAttempt_FieldNamesAreFrozen(t *testing.T) {
	a := failedAt(runningAttempt(t, attemptNow), StageRollback, attemptNow)
	a.RolledBack = true
	raw, err := json.Marshal(a)
	require.NoError(t, err)
	var m map[string]any
	require.NoError(t, json.Unmarshal(raw, &m))
	require.Equal(t, []string{
		"backup_path", "error", "finished_at", "from", "heartbeat_at", "id", "log_path",
		"ok", "rolled_back", "stage", "started_at", "to", "why",
	}, slices.Sorted(maps.Keys(m)))
	require.Equal(t, "0.7.2", m["from"])
	require.Equal(t, "0.7.3", m["to"])
	require.Equal(t, "rollback", m["stage"])

	// A running attempt has no finished_at, error or paths to show.
	raw, err = json.Marshal(Attempt{ID: a.ID, From: a.From, To: a.To, Why: WhyAuto,
		StartedAt: attemptNow, HeartbeatAt: attemptNow, Stage: StageLock})
	require.NoError(t, err)
	m = nil
	require.NoError(t, json.Unmarshal(raw, &m))
	require.Equal(t, []string{"from", "heartbeat_at", "id", "ok", "rolled_back", "stage", "started_at", "to", "why"},
		slices.Sorted(maps.Keys(m)))

	// A refusal adds its one key, a fixed word; nothing else does.
	refused := failedAt(runningAttempt(t, attemptNow), StageGates, attemptNow)
	refused.Refusal = RefusalStaleBinary
	raw, err = json.Marshal(refused)
	require.NoError(t, err)
	m = nil
	require.NoError(t, json.Unmarshal(raw, &m))
	require.Equal(t, []string{
		"backup_path", "error", "finished_at", "from", "heartbeat_at", "id", "log_path",
		"ok", "refusal", "rolled_back", "stage", "started_at", "to", "why",
	}, slices.Sorted(maps.Keys(m)))
	require.Equal(t, "stale_binary", m["refusal"])
}

// The refusal words are what every release reads every other one's records
// by, as the stage and why words are: renaming one breaks reading across
// versions, so they are pinned here.
func TestRefusal_WordsAreFrozen(t *testing.T) {
	require.Equal(t, []string{"not_installer", "stale_binary", "self_check", "auto_off"},
		[]string{RefusalNotInstaller, RefusalStaleBinary, RefusalSelfCheck, RefusalAutoOff})
	for _, r := range refusals {
		require.True(t, KnownRefusal(r), r)
	}
	for _, r := range []string{"", "disk_full", "STALE_BINARY", "stale_binary "} {
		require.False(t, KnownRefusal(r), "%q", r)
	}
}

func TestReadAttempt_MissingIsNone(t *testing.T) {
	dir := t.TempDir()
	a, err := ReadAttempt(dir) // no update directory at all
	require.NoError(t, err)
	require.Equal(t, Attempt{}, a)
	require.False(t, a.Live(attemptNow))
	require.False(t, a.Finished())

	require.NoError(t, os.MkdirAll(StateDir(dir), 0o700)) // the directory, no attempt yet
	a, err = ReadAttempt(dir)
	require.NoError(t, err)
	require.Equal(t, Attempt{}, a)
}

func TestReadAttempt_AcrossVersions(t *testing.T) {
	const id = "01K7A0000000000000000000A1"
	start := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	for name, tc := range map[string]struct {
		body string
		want Attempt
	}{
		"a newer release's fields and stage": {
			body: `{"id":"` + id + `","from":"0.7.2","to":"0.8.0","why":"auto",` +
				`"started_at":"2026-10-09T12:00:00Z","heartbeat_at":"2026-10-09T12:01:00Z",` +
				`"ok":false,"rolled_back":false,"stage":"attest","pid":4242,"channel":{"name":"stable"}}`,
			want: Attempt{ID: id, From: Version{0, 7, 2}, To: Version{0, 8, 0}, Why: WhyAuto,
				StartedAt: start, HeartbeatAt: start.Add(time.Minute), Stage: "attest"},
		},
		"a newer release's why": {
			body: `{"id":"` + id + `","from":"0.8.0","to":"0.8.0","why":"repair","stage":"backup"}`,
			want: Attempt{ID: id, From: Version{0, 8, 0}, To: Version{0, 8, 0}, Why: "repair", Stage: StageBackup},
		},
		"an older release's missing fields": {
			body: `{"id":"` + id + `","from":"0.7.1","to":"0.7.2","finished_at":"2026-10-09T12:00:00Z","ok":true,"stage":"done"}`,
			want: Attempt{ID: id, From: Version{0, 7, 1}, To: Version{0, 7, 2}, FinishedAt: start, OK: true, Stage: StageDone},
		},
		"nulls read as zero": {
			body: `{"id":"` + id + `","from":null,"to":"0.7.2","finished_at":null,"error":null,"refusal":null}`,
			want: Attempt{ID: id, To: Version{0, 7, 2}},
		},
		"a gate refusal from before refusals had a word": {
			body: `{"id":"` + id + `","from":"0.7.2","to":"0.7.3","why":"auto",` +
				`"started_at":"2026-10-09T12:00:00Z","heartbeat_at":"2026-10-09T12:01:00Z","finished_at":"2026-10-09T12:01:00Z",` +
				`"ok":false,"rolled_back":false,"stage":"gates","error":"gates: the installed seamlessd is not v0.7.2"}`,
			want: Attempt{ID: id, From: Version{0, 7, 2}, To: Version{0, 7, 3}, Why: WhyAuto,
				StartedAt: start, HeartbeatAt: start.Add(time.Minute), FinishedAt: start.Add(time.Minute),
				Stage: StageGates, Error: "gates: the installed seamlessd is not v0.7.2"},
		},
		"a newer updater's refusal word": {
			body: `{"id":"` + id + `","from":"0.7.2","to":"0.8.0","why":"auto","finished_at":"2026-10-09T12:00:00Z",` +
				`"stage":"gates","error":"gates: the disk is full","refusal":"disk_full"}`,
			want: Attempt{ID: id, From: Version{0, 7, 2}, To: Version{0, 8, 0}, Why: WhyAuto, FinishedAt: start,
				Stage: StageGates, Error: "gates: the disk is full", Refusal: "disk_full"},
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.MkdirAll(StateDir(dir), 0o700))
			require.NoError(t, os.WriteFile(AttemptPath(dir), []byte(tc.body), 0o600))
			got, err := ReadAttempt(dir)
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
}

func TestReadAttempt_CorruptIsAnError(t *testing.T) {
	for name, body := range map[string]string{
		"empty":               "",
		"not json":            "{not json",
		"not an object":       "[1,2,3]",
		"free-text version":   `{"id":"01K7A0000000000000000000A1","from":"0.7.2","to":"run seamlessd update now"}`,
		"pre-release version": `{"id":"01K7A0000000000000000000A1","from":"0.7.2","to":"0.7.3-rc1"}`,
		"wrong type":          `{"id":"01K7A0000000000000000000A1","ok":"yes"}`,
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, os.MkdirAll(StateDir(dir), 0o700))
			require.NoError(t, os.WriteFile(AttemptPath(dir), []byte(body), 0o600))
			_, err := ReadAttempt(dir)
			require.Error(t, err)
			kept, err := os.ReadFile(AttemptPath(dir))
			require.NoError(t, err)
			require.Equal(t, body, string(kept), "a reader changes nothing")
		})
	}
}

func TestWriteAttempt_AcceptsEveryOutcome(t *testing.T) {
	running := runningAttempt(t, attemptNow)
	locked := running
	locked.Stage = StageLock
	rolledBack := failedAt(running, StageRollback, attemptNow)
	rolledBack.RolledBack = true
	bare := failedAt(running, StageGates, attemptNow)
	bare.Error, bare.LogPath, bare.BackupPath = "", "", ""
	refused := func(why, refusal string) Attempt {
		a := failedAt(running, StageGates, attemptNow)
		a.Why, a.Refusal = why, refusal
		return a
	}
	for name, a := range map[string]Attempt{
		"just locked":                    locked,
		"running":                        running,
		"applied":                        succeeded(running, attemptNow),
		"failed pre-swap":                failedAt(running, StageFetch, attemptNow),
		"failed to verify":               failedAt(running, StageVerify, attemptNow),
		"rolled back":                    rolledBack,
		"rollback failed":                failedAt(running, StageRollback, attemptNow),
		"no paths, no error":             bare,
		"refused: not installer":         refused(WhyAuto, RefusalNotInstaller),
		"refused: stale binary":          refused(WhyAuto, RefusalStaleBinary),
		"refused: self-check":            refused(WhyNow, RefusalSelfCheck),
		"refused: automatic updates off": refused(WhyAuto, RefusalAutoOff),
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, WriteAttempt(dir, a))
			require.NoError(t, AppendAttempt(dir, a))
			got, err := ReadAttempt(dir)
			require.NoError(t, err)
			require.Equal(t, a, got)
			hist, skipped, err := ReadAttemptHistory(dir)
			require.NoError(t, err)
			require.Zero(t, skipped)
			require.Equal(t, []Attempt{a}, hist)
		})
	}
}

func TestWriteAttempt_RefusesWhatThisReleaseWouldNotWrite(t *testing.T) {
	for name, mutate := range map[string]func(a *Attempt){
		"no id":                     func(a *Attempt) { a.ID = "" },
		"id that is a path":         func(a *Attempt) { a.ID = "../../../../../../etc/passw" },
		"lower-case id":             func(a *Attempt) { a.ID = "01k7a0000000000000000000a1" },
		"no from":                   func(a *Attempt) { a.From = Version{} },
		"no to":                     func(a *Attempt) { a.To = Version{} },
		"an auto downgrade":         func(a *Attempt) { a.From, a.To = a.To, a.From },
		"an auto reinstall":         func(a *Attempt) { a.To = a.From },
		"no why":                    func(a *Attempt) { a.Why = "" },
		"an unknown why":            func(a *Attempt) { a.Why = "because" },
		"no stage":                  func(a *Attempt) { a.Stage = "" },
		"an unknown stage":          func(a *Attempt) { a.Stage = "attest" },
		"no started_at":             func(a *Attempt) { a.StartedAt = time.Time{} },
		"no heartbeat":              func(a *Attempt) { a.HeartbeatAt = time.Time{} },
		"ok before done":            func(a *Attempt) { a.OK = true },
		"done but not ok":           func(a *Attempt) { *a = failedAt(*a, StageDone, attemptNow) },
		"ok but not finished":       func(a *Attempt) { a.Stage, a.OK = StageDone, true },
		"ok and rolled back":        func(a *Attempt) { *a = succeeded(*a, attemptNow); a.RolledBack = true },
		"rolled back outside stage": func(a *Attempt) { a.RolledBack = true },
		"an unknown refusal": func(a *Attempt) {
			*a = failedAt(*a, StageGates, attemptNow)
			a.Refusal = "disk_full"
		},
		"a refusal outside the gates": func(a *Attempt) {
			*a = failedAt(*a, StageFetch, attemptNow)
			a.Refusal = RefusalStaleBinary
		},
		"a refusal before it finished": func(a *Attempt) { a.Stage, a.Refusal = StageGates, RefusalStaleBinary },
		"a refusal on an attended update": func(a *Attempt) {
			*a = failedAt(*a, StageGates, attemptNow)
			a.Why, a.Refusal = WhyManual, RefusalSelfCheck
		},
	} {
		t.Run(name, func(t *testing.T) {
			dir := t.TempDir()
			a := runningAttempt(t, attemptNow)
			mutate(&a)
			require.ErrorIs(t, WriteAttempt(dir, a), ErrInvalidAttempt)
			require.ErrorIs(t, AppendAttempt(dir, a), ErrInvalidAttempt)
			_, err := os.Stat(StateDir(dir))
			require.True(t, os.IsNotExist(err), "nothing is written")
		})
	}
}

func TestAttempt_ErrorIsCappedOnWrite(t *testing.T) {
	dir := t.TempDir()
	a := failedAt(runningAttempt(t, attemptNow), StageInstall, attemptNow)
	a.Error = strings.Repeat("the installer exited 1. ", 400)
	require.NoError(t, WriteAttempt(dir, a))
	require.NoError(t, AppendAttempt(dir, a))

	got, err := ReadAttempt(dir)
	require.NoError(t, err)
	require.LessOrEqual(t, utf8.RuneCountInString(got.Error), attemptErrorMax)
	require.True(t, strings.HasPrefix(got.Error, "the installer exited 1."))
	hist, _, err := ReadAttemptHistory(dir)
	require.NoError(t, err)
	require.Len(t, hist, 1)
	require.Equal(t, got.Error, hist[0].Error, "both files carry the same record")
}

func TestAttempt_Live(t *testing.T) {
	running := runningAttempt(t, attemptNow)
	beat := func(at time.Time) Attempt { a := running; a.HeartbeatAt = at; return a }
	for name, tc := range map[string]struct {
		a    Attempt
		live bool
	}{
		"fresh":                            {beat(attemptNow), true},
		"one interval old":                 {beat(attemptNow.Add(-AttemptHeartbeatInterval)), true},
		"just short of stale":              {beat(attemptNow.Add(-AttemptHeartbeatStale + time.Second)), true},
		"stale":                            {beat(attemptNow.Add(-AttemptHeartbeatStale)), false},
		"long dead":                        {beat(attemptNow.Add(-48 * time.Hour)), false},
		"a moment ahead":                   {beat(attemptNow.Add(time.Second)), true},
		"far ahead: the clock went back":   {beat(attemptNow.Add(AttemptHeartbeatStale)), false},
		"applied":                          {succeeded(running, attemptNow), false},
		"failed, heartbeat still fresh":    {failedAt(running, StageFetch, attemptNow), false},
		"no heartbeat (an older writer's)": {beat(time.Time{}), false},
		"none":                             {Attempt{}, false},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.live, tc.a.Live(attemptNow))
		})
	}
}

// What the checker sees of one spawn through attempt.json alone, from the
// spawn to an interruption, and of the next spawn to its outcome.
func TestAttempt_TheCheckersView(t *testing.T) {
	dir := t.TempDir()
	now := attemptNow
	spawned := newAttemptID(t)

	// Spawned, not yet started: nothing names it.
	a, err := ReadAttempt(dir)
	require.NoError(t, err)
	require.NotEqual(t, spawned, a.ID)

	// Started: it holds the lock, then heartbeats through a long install.
	started := Attempt{ID: spawned, From: Version{0, 7, 2}, To: Version{0, 7, 3}, Why: WhyAuto,
		StartedAt: now.Add(time.Minute), HeartbeatAt: now.Add(time.Minute), Stage: StageLock}
	require.NoError(t, WriteAttempt(dir, started))
	started.Stage, started.HeartbeatAt = StageInstall, now.Add(4*time.Minute)
	require.NoError(t, WriteAttempt(dir, started))
	a, err = ReadAttempt(dir)
	require.NoError(t, err)
	require.Equal(t, spawned, a.ID)
	require.True(t, a.Live(now.Add(8*time.Minute)), "four minutes after its last heartbeat")

	// Killed mid-install: no outcome, and the heartbeat goes stale.
	later := now.Add(4*time.Minute + AttemptHeartbeatStale)
	require.False(t, a.Live(later))
	require.False(t, a.Finished(), "interrupted: not live, not finished")

	// The next spawn replaces it and finishes rolled back.
	next := runningAttempt(t, later)
	next = failedAt(next, StageRollback, later.Add(time.Minute))
	next.RolledBack = true
	require.NoError(t, WriteAttempt(dir, next))
	a, err = ReadAttempt(dir)
	require.NoError(t, err)
	require.Equal(t, next.ID, a.ID)
	require.True(t, a.Finished())
	require.False(t, a.OK)
	require.True(t, a.RolledBack)
	require.False(t, a.Stage.PreSwap())
}

func TestStage_ValidAndPreSwap(t *testing.T) {
	for _, tc := range []struct {
		s              Stage
		valid, preSwap bool
	}{
		{StageLock, true, true},
		{StageGates, true, true},
		{StageFetch, true, true},
		{StageVerify, true, true},
		{StagePreflight, true, true},
		{StageBackup, true, true},
		{StageInstall, true, false},
		{StageConfirm, true, false},
		{StageRollback, true, false},
		{StageDone, true, false},
		{"attest", false, false}, // a newer release's: nothing vouches for the install
		{"", false, false},
	} {
		t.Run(string(tc.s), func(t *testing.T) {
			require.Equal(t, tc.valid, tc.s.Valid())
			require.Equal(t, tc.preSwap, tc.s.PreSwap())
		})
	}
	require.Len(t, Stages, 10)
	seen := map[Stage]bool{}
	for _, s := range Stages {
		require.False(t, seen[s], "listed twice: %s", s)
		seen[s] = true
	}
}

func TestAttemptHistory_AppendAndRead(t *testing.T) {
	dir := t.TempDir()
	var want []Attempt
	for i := range 3 {
		at := attemptNow.Add(time.Duration(i) * time.Hour)
		a := failedAt(runningAttempt(t, at), StageFetch, at)
		require.NoError(t, AppendAttempt(dir, a))
		want = append(want, a)
	}
	got, skipped, err := ReadAttemptHistory(dir)
	require.NoError(t, err)
	require.Zero(t, skipped)
	require.Equal(t, want, got)

	raw, err := os.ReadFile(AttemptHistoryPath(dir))
	require.NoError(t, err)
	require.Equal(t, 3, bytes.Count(raw, []byte("\n")), "one line per record")

	if runtime.GOOS != "windows" {
		fi, err := os.Stat(AttemptHistoryPath(dir))
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
		di, err := os.Stat(StateDir(dir))
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o700), di.Mode().Perm())
	}
}

func TestAttemptHistory_MissingIsEmpty(t *testing.T) {
	got, skipped, err := ReadAttemptHistory(t.TempDir())
	require.NoError(t, err)
	require.Zero(t, skipped)
	require.Empty(t, got)
}

func TestAttemptHistory_SkipsWhatDoesNotDecode(t *testing.T) {
	dir := t.TempDir()
	require.NoError(t, os.MkdirAll(StateDir(dir), 0o700))
	older := `{"id":"01K7A0000000000000000000A1","from":"0.7.1","to":"0.7.2","stage":"done","ok":true,` +
		`"finished_at":"2026-10-01T12:00:00Z"}`
	newer := `{"id":"01K7A0000000000000000000A2","from":"0.7.2","to":"0.8.0","why":"auto",` +
		`"started_at":"2026-10-02T12:00:00Z","heartbeat_at":"2026-10-02T12:05:00Z","finished_at":"2026-10-02T12:05:00Z",` +
		`"ok":false,"rolled_back":false,"stage":"attest","error":"the attestation did not match",` +
		`"pid":4242,"attestation":{"log":"rekor"}}`
	body := older + "\r\n" + // written on a line of its own with a stray CR
		"\n" + // blank: ignored, not counted
		"{not json\n" +
		`{"id":"01K7A0000000000000000000A3","from":"0.7.2","to":"latest"}` + "\n" + // not X.Y.Z
		`{"from":"0.7.2","to":"0.7.3","stage":"done","ok":true}` + "\n" + // no id
		`[1,2,3]` + "\n" +
		newer + "\n" +
		`{"id":"01K7A0000000000000000000A4","from":"0.7.2","to":"0.8` // torn by a crash mid-append
	require.NoError(t, os.WriteFile(AttemptHistoryPath(dir), []byte(body), 0o600))

	got, skipped, err := ReadAttemptHistory(dir)
	require.NoError(t, err)
	require.Equal(t, 5, skipped)
	require.Len(t, got, 2)
	require.Equal(t, "01K7A0000000000000000000A1", got[0].ID)
	require.True(t, got[0].OK)
	require.Empty(t, got[0].Why, "a field an older release did not write reads as zero")
	require.Equal(t, "01K7A0000000000000000000A2", got[1].ID)
	require.Equal(t, Version{0, 8, 0}, got[1].To)
	require.Equal(t, Stage("attest"), got[1].Stage)
	require.False(t, got[1].Stage.PreSwap())

	// The next append starts on a line of its own, so the torn line stays one
	// skipped line and the new record reads whole.
	a := failedAt(runningAttempt(t, attemptNow), StageGates, attemptNow)
	require.NoError(t, AppendAttempt(dir, a))
	got, skipped, err = ReadAttemptHistory(dir)
	require.NoError(t, err)
	require.Equal(t, 5, skipped)
	require.Len(t, got, 3)
	require.Equal(t, a, got[2])
}

// historyLine is an older release's history line, about 350 bytes.
func historyLine(i int) string {
	return fmt.Sprintf(`{"id":"01K7A%021d","from":"0.7.1","to":"0.7.2","why":"auto","stage":"fetch",`+
		`"started_at":"2026-10-01T12:00:00Z","heartbeat_at":"2026-10-01T12:00:00Z","finished_at":"2026-10-01T12:00:00Z",`+
		`"ok":false,"rolled_back":false,"error":"%s"}`, i, strings.Repeat("x", 120))
}

func seedHistory(t *testing.T, dir string, lines []string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(StateDir(dir), 0o700))
	require.NoError(t, os.WriteFile(AttemptHistoryPath(dir), []byte(strings.Join(lines, "\n")+"\n"), 0o600))
}

func TestAttemptHistory_CompactionKeepsTheNewestLinesVerbatim(t *testing.T) {
	dir := t.TempDir()
	var lines []string
	for i := 0; len(strings.Join(lines, "\n")) < historyCompactAt; i++ {
		lines = append(lines, historyLine(i))
	}
	// The newest seeded line is a newer release's, with a field this one does
	// not know: compacting must not drop it.
	lines[len(lines)-1] = strings.TrimSuffix(lines[len(lines)-1], "}") + `,"attestation":{"log":"rekor"}}`
	seedHistory(t, dir, lines)

	a := failedAt(runningAttempt(t, attemptNow), StageFetch, attemptNow)
	require.NoError(t, AppendAttempt(dir, a))

	raw, err := os.ReadFile(AttemptHistoryPath(dir))
	require.NoError(t, err)
	kept := strings.Split(strings.TrimSuffix(string(raw), "\n"), "\n")
	require.Len(t, kept, historyKeep)
	require.Equal(t, lines[len(lines)-historyKeep+1:], kept[:historyKeep-1], "kept byte for byte")
	require.Contains(t, kept[historyKeep-2], `"attestation":{"log":"rekor"}`)

	got, skipped, err := ReadAttemptHistory(dir)
	require.NoError(t, err)
	require.Zero(t, skipped)
	require.Len(t, got, historyKeep)
	require.Equal(t, a, got[historyKeep-1])

	entries, err := os.ReadDir(StateDir(dir))
	require.NoError(t, err)
	require.Len(t, entries, 1, "no temp file left behind")
	if runtime.GOOS != "windows" {
		fi, err := os.Stat(AttemptHistoryPath(dir))
		require.NoError(t, err)
		require.Equal(t, os.FileMode(0o600), fi.Mode().Perm())
	}
}

func TestAttemptHistory_NoCompactionBelowTheBound(t *testing.T) {
	dir := t.TempDir()
	var lines []string
	for i := range historyKeep + 20 {
		lines = append(lines, fmt.Sprintf(`{"id":"01K7A%021d","from":"0.7.1","to":"0.7.2"}`, i))
	}
	seedHistory(t, dir, lines)
	require.NoError(t, AppendAttempt(dir, failedAt(runningAttempt(t, attemptNow), StageGates, attemptNow)))
	got, skipped, err := ReadAttemptHistory(dir)
	require.NoError(t, err)
	require.Zero(t, skipped)
	require.Len(t, got, historyKeep+21, "a small file keeps every line")
}

func TestAttemptHistory_ReadsOnlyTheTail(t *testing.T) {
	dir := t.TempDir()
	var lines []string
	for i := 0; len(strings.Join(lines, "\n")) < 2*historyReadMax; i++ {
		lines = append(lines, historyLine(i))
	}
	seedHistory(t, dir, lines)

	got, skipped, err := ReadAttemptHistory(dir)
	require.NoError(t, err)
	require.Zero(t, skipped, "the line the window cuts is dropped, not counted")
	require.NotEmpty(t, got)
	require.Less(t, len(got), len(lines))
	for i, a := range got {
		require.Equal(t, fmt.Sprintf("01K7A%021d", len(lines)-len(got)+i), a.ID, "the newest lines, in order")
	}
}

func TestReadTail(t *testing.T) {
	path := filepath.Join(t.TempDir(), "history")
	require.NoError(t, os.WriteFile(path, []byte("aaaa\nbbbb\ncccc\n"), 0o600)) // 15 bytes
	for name, tc := range map[string]struct {
		limit int64
		want  string
	}{
		"the file fits":                   {15, "aaaa\nbbbb\ncccc\n"},
		"a larger limit":                  {100, "aaaa\nbbbb\ncccc\n"},
		"the window cuts the first line":  {14, "bbbb\ncccc\n"},
		"the window starts on a boundary": {10, "bbbb\ncccc\n"},
		"the window cuts a later line":    {8, "cccc\n"},
		"the window is inside one line":   {3, ""},
	} {
		t.Run(name, func(t *testing.T) {
			got, err := readTail(path, tc.limit)
			require.NoError(t, err)
			require.Equal(t, tc.want, string(got))
		})
	}

	_, err := readTail(filepath.Join(t.TempDir(), "missing"), 10)
	require.ErrorIs(t, err, os.ErrNotExist)
}

func TestValidAttemptID(t *testing.T) {
	for range 50 {
		id := newAttemptID(t)
		require.True(t, ValidAttemptID(id), id)
	}
	for name, tc := range map[string]struct {
		id string
		ok bool
	}{
		"a ULID":                   {"01K7A0000000000000000000A1", true},
		"the largest":              {"7" + strings.Repeat("Z", 25), true},
		"empty":                    {"", false},
		"one short":                {strings.Repeat("0", 25), false},
		"one long":                 {strings.Repeat("0", 27), false},
		"past 128 bits":            {"8" + strings.Repeat("0", 25), false},
		"lower case":               {"01k7a0000000000000000000a1", false},
		"an I, never in Crockford": {strings.Repeat("0", 25) + "I", false},
		"an L":                     {strings.Repeat("0", 25) + "L", false},
		"an O":                     {strings.Repeat("0", 25) + "O", false},
		"a U":                      {strings.Repeat("0", 25) + "U", false},
		"a path":                   {"../../../../../../etc/pass", false},
		"a space":                  {strings.Repeat("0", 25) + " ", false},
		"a quote":                  {strings.Repeat("0", 25) + `"`, false},
		"a multi-byte rune":        {strings.Repeat("0", 23) + "é0", false},
	} {
		t.Run(name, func(t *testing.T) {
			require.Equal(t, tc.ok, ValidAttemptID(tc.id))
		})
	}
}

func TestSpawnRequest_Validate(t *testing.T) {
	good := SpawnRequest{AttemptID: newAttemptID(t), From: Version{0, 7, 2}, To: Version{0, 7, 3}, Why: WhyAuto}
	require.NoError(t, good.Validate())

	// The owner may pin an older release or reinstall the running one by hand.
	manual := good
	manual.Why = WhyManual
	manual.From, manual.To = good.To, good.From
	require.NoError(t, manual.Validate(), "a manual downgrade")
	manual.To = manual.From
	require.NoError(t, manual.Validate(), "a manual reinstall")

	// Update now is the daemon's too, and moves forward only, like auto.
	now := good
	now.Why = WhyNow
	require.NoError(t, now.Validate())

	for name, mutate := range map[string]func(r *SpawnRequest){
		"an Update now downgrade": func(r *SpawnRequest) { r.Why = WhyNow; r.From, r.To = r.To, r.From },
		"an Update now reinstall": func(r *SpawnRequest) { r.Why = WhyNow; r.To = r.From },
		"no attempt id":           func(r *SpawnRequest) { r.AttemptID = "" },
		"an argument":             func(r *SpawnRequest) { r.AttemptID = strings.Repeat("0", 15) + " --url=evil" },
		"no from":                 func(r *SpawnRequest) { r.From = Version{} },
		"no to":                   func(r *SpawnRequest) { r.To = Version{} },
		"no why":                  func(r *SpawnRequest) { r.Why = "" },
		"an unknown why":          func(r *SpawnRequest) { r.Why = "cron" },
		"an auto downgrade":       func(r *SpawnRequest) { r.From, r.To = r.To, r.From },
		"an auto reinstall":       func(r *SpawnRequest) { r.To = r.From },
	} {
		t.Run(name, func(t *testing.T) {
			r := good
			mutate(&r)
			require.ErrorIs(t, r.Validate(), ErrInvalidAttempt)
		})
	}
}

// fakeSpawner is a Spawner as the checker sees one: it refuses a request
// that fails Validate before anything else, and records the rest.
type fakeSpawner struct {
	reqs []SpawnRequest
	err  error
}

var _ Spawner = (*fakeSpawner)(nil)

func (f *fakeSpawner) Spawn(_ context.Context, req SpawnRequest) error {
	if err := req.Validate(); err != nil {
		return err
	}
	if f.err != nil {
		return f.err
	}
	f.reqs = append(f.reqs, req)
	return nil
}

func TestSpawner_Contract(t *testing.T) {
	f := &fakeSpawner{}
	var s Spawner = f
	req := SpawnRequest{AttemptID: newAttemptID(t), From: Version{0, 7, 2}, To: Version{0, 7, 3}, Why: WhyAuto}
	require.NoError(t, s.Spawn(context.Background(), req))
	require.Equal(t, []SpawnRequest{req}, f.reqs)

	bad := req
	bad.To = Version{0, 7, 1}
	require.ErrorIs(t, s.Spawn(context.Background(), bad), ErrInvalidAttempt)
	require.Len(t, f.reqs, 1, "an invalid request starts nothing")

	f.err = errors.New("launchctl: service not found")
	require.EqualError(t, s.Spawn(context.Background(), req), "launchctl: service not found")
}

func TestAttempt_SignatureIsAKnownWordAtVerify(t *testing.T) {
	base := Attempt{
		ID: "01K7A0000000000000000000A1", From: ver("0.7.2"), To: ver("0.7.3"), Why: WhyManual,
		StartedAt: attemptNow, HeartbeatAt: attemptNow, FinishedAt: attemptNow, Stage: StageVerify,
	}
	for _, w := range []string{SignatureMissing, SignatureUnlisted, SignatureInvalid} {
		a := base
		a.Signature = w
		require.NoError(t, a.check(), w)
		a.Why = WhyAuto
		require.NoError(t, a.check(), "attended or not")
	}
	for name, edit := range map[string]func(*Attempt){
		"an unknown word":      func(a *Attempt) { a.Signature = "expired" },
		"at another stage":     func(a *Attempt) { a.Signature, a.Stage = SignatureMissing, StageFetch },
		"on an unfinished one": func(a *Attempt) { a.Signature, a.FinishedAt = SignatureMissing, time.Time{} },
	} {
		a := base
		edit(&a)
		require.ErrorIs(t, a.check(), ErrInvalidAttempt, name)
	}
	require.True(t, KnownRefusal(RefusalReleaseSigners))
	require.NotEmpty(t, RefusalWords(RefusalReleaseSigners))
	require.NotEmpty(t, RefusalAction(RefusalReleaseSigners))
}
