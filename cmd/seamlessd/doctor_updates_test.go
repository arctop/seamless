package main

import (
	"bytes"
	"context"
	"crypto/tls"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"testing"
	"time"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
	"github.com/arctop/seamless/internal/update"
	"github.com/stretchr/testify/require"
)

// releaseRecord is the state a release daemon on v0.7.2 writes.
func releaseRecord(now time.Time, mutate func(s *update.State)) update.State {
	v := update.Version{Major: 0, Minor: 7, Patch: 2}
	s := update.State{
		CheckedAt:   now.Add(-2 * time.Hour),
		NextCheckAt: now.Add(4 * time.Hour),
		Releases:    []update.Release{{Version: v, PublishedAt: now.Add(-72 * time.Hour)}},
		Running: update.Running{
			Version: v, Distribution: update.DistributionRelease, Kind: update.KindInstaller,
			Instance: "01INST", PID: 7, StartedAt: now.Add(-3 * time.Hour),
		},
		LastRunVersion: &v,
	}
	if mutate != nil {
		mutate(&s)
	}
	return s
}

func TestUpdatesCheck(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	no := false
	newer := func(s *update.State) {
		s.Releases = append(s.Releases, update.Release{Version: update.Version{Major: 0, Minor: 7, Patch: 3}, PublishedAt: now.Add(-time.Hour)})
	}
	tests := []struct {
		name       string
		state      *update.State // nil: no state file
		corrupt    bool
		check      *bool
		wantStatus checkStatus
		want       string
	}{
		{"source build with nothing recorded", nil, false, nil, statusInfo, "builds from source"},
		{"release daemon, up to date", ptr(releaseRecord(now, nil)), false, nil, statusOK, "up to date at v0.7.2 (checked 2h ago"},
		{"release daemon, behind", ptr(releaseRecord(now, newer)), false, nil, statusWarn,
			"v0.7.3 is available (running v0.7.2; checked 2h ago) -- update with: seamlessd update"},
		{"homebrew daemon, behind", ptr(releaseRecord(now, func(s *update.State) {
			newer(s)
			s.Running.Kind = update.KindHomebrew
		})), false, nil, statusWarn, "brew upgrade --cask arctop/tap/seamless"},
		{"checks off in the config", ptr(releaseRecord(now, newer)), false, &no, statusInfo, "update.check: false"},
		{"a fresh error streak informs", ptr(releaseRecord(now, func(s *update.State) {
			s.CheckError = &update.CheckError{Kind: update.CheckErrorUnavailable, Message: "dial tcp: timeout", Since: now.Add(-time.Hour), Count: 2}
		})), false, nil, statusInfo, "failed 2 time(s)"},
		{"a week of errors warns", ptr(releaseRecord(now, func(s *update.State) {
			s.CheckError = &update.CheckError{Kind: update.CheckErrorUnavailable, Message: "proxy refused", Since: now.Add(-8 * 24 * time.Hour), Count: 30}
		})), false, nil, statusWarn, "HTTPS_PROXY"},
		{"never checked yet", ptr(releaseRecord(now, func(s *update.State) { s.CheckedAt = time.Time{} })), false, nil, statusInfo, "no release check recorded yet"},
		{"corrupt record", nil, true, nil, statusWarn, "cannot read"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			if tt.state != nil {
				require.NoError(t, update.SaveState(dir, *tt.state))
			}
			if tt.corrupt {
				require.NoError(t, os.MkdirAll(update.StateDir(dir), 0o700))
				require.NoError(t, os.WriteFile(update.StatePath(dir), []byte("{bad"), 0o600))
			}
			cfg := config.Defaults()
			cfg.DataDir = dir
			cfg.Update.Check = tt.check
			got := updatesCheck(context.Background(), cfg, nil, now)
			require.Equal(t, "updates", got.name)
			require.Equal(t, tt.wantStatus, got.status, got.detail)
			require.Contains(t, got.detail, tt.want)
		})
	}
}

// TestUpdatesCheck_ConsoleOverrideIsRead: the doctor row merges the console's
// stored override exactly as the daemon does.
func TestUpdatesCheck_ConsoleOverrideIsRead(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	require.NoError(t, update.SaveState(dir, releaseRecord(now, nil)))
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	defer func() { _ = db.Close() }()
	off := false
	require.NoError(t, store.SetUpdateOverride(context.Background(), db, config.UpdateOverride{Check: &off}))

	cfg := config.Defaults()
	cfg.DataDir = dir
	got := updatesCheck(context.Background(), cfg, db, now)
	require.Equal(t, statusInfo, got.status)
	require.Contains(t, got.detail, "turned off in the console")
}

func TestUpdateCheckRows(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	require.NoError(t, update.SaveState(dir, releaseRecord(now, nil)))
	cfg := config.Defaults()
	cfg.DataDir = dir
	v := loadUpdateView(context.Background(), cfg, nil)

	var buf bytes.Buffer
	updateCheckRows(&buf, v, now)
	out := buf.String()
	require.Contains(t, out, "notify")
	require.Contains(t, out, "2h ago by the daemon; next in 4h")
}

func TestUpdateCheckRows_ClientHasNoRecord(t *testing.T) {
	cfg := config.Defaults()
	cfg.Role = "client"
	cfg.AdvertisedURL = "https://server.example:8081"
	cfg.DataDir = t.TempDir()
	v := loadUpdateView(context.Background(), cfg, nil)
	require.Equal(t, update.KindSource, v.install.Kind, "a test binary is a source build")

	var buf bytes.Buffer
	updateCheckRows(&buf, v, time.Now())
	out := buf.String()
	require.Contains(t, out, "never by the daemon")
	for _, row := range []string{"target", "pending", "update", "last", "hold", "paused", "blocked", "backoff", "drill"} {
		require.NotContains(t, out, "  "+row+" ", "a client has no daemon record to show")
	}
}

func TestHumanDuration(t *testing.T) {
	require.Equal(t, "45s", humanDuration(45*time.Second))
	require.Equal(t, "12m", humanDuration(12*time.Minute))
	require.Equal(t, "47h", humanDuration(47*time.Hour))
	require.Equal(t, "3d", humanDuration(80*time.Hour))
}

// A time still ahead rounds up, so a soak with 8h58m left does not read "in
// 8h".
func TestWhenText(t *testing.T) {
	now := time.Date(2026, 10, 10, 19, 49, 0, 0, time.UTC)
	require.Equal(t, "now", whenText(time.Time{}, now))
	require.Equal(t, "now", whenText(now.Add(-time.Minute), now))
	require.Equal(t, "in 1m", whenText(now.Add(30*time.Second), now))
	require.Equal(t, "in 8h 58m", whenText(now.Add(8*time.Hour+58*time.Minute), now))
	require.Equal(t, "in 3d 4h", whenText(now.Add(76*time.Hour), now))
}

func ptr[T any](v T) *T { return &v }

// Versions the automatic-update fixtures name.
var (
	v071 = update.Version{Major: 0, Minor: 7, Patch: 1}
	v072 = update.Version{Major: 0, Minor: 7, Patch: 2}
	v073 = update.Version{Major: 0, Minor: 7, Patch: 3}
	v074 = update.Version{Major: 0, Minor: 7, Patch: 4}
)

// autoRecord is a release daemon on v0.7.2 that updates itself: the installer
// made it and an updater is wired. Its release list carries checksums
// bundles.
func autoRecord(now time.Time, mutate func(s *update.State)) update.State {
	return releaseRecord(now, func(s *update.State) {
		s.Running.CanApply = true
		s.ServerDate = s.CheckedAt
		s.Releases[0].ChecksumsBundle = true
		if mutate != nil {
			mutate(s)
		}
	})
}

// signed is a release published at at that carries the checksums bundle.
func signed(v update.Version, at time.Time) update.Release {
	return update.Release{Version: v, PublishedAt: at, ChecksumsBundle: true}
}

// finishedRecord is a finished attempt.json from v0.7.2 to to.
func finishedRecord(t *testing.T, now time.Time, to update.Version, why string, edit func(a *update.Attempt)) update.Attempt {
	t.Helper()
	a := update.Attempt{
		ID: testAttemptID(t), From: v072, To: to, Why: why,
		StartedAt: now.Add(-15 * time.Minute), HeartbeatAt: now.Add(-10 * time.Minute), FinishedAt: now.Add(-10 * time.Minute),
		LogPath: "/var/seamless/update/logs/x.log",
	}
	if edit != nil {
		edit(&a)
	}
	return a
}

// failedRollbackError is the record a failed rollback leaves: the recovery
// steps, the backup path among them (update_engine.go's rollbackFailed).
const failedRollbackError = "v0.7.3 did not come up (no answer), and the rollback to v0.7.2 was not confirmed (no answer); " +
	"the daemon may be down: check it with seamlessd status, reinstall v0.7.2 with SEAMLESS_VERSION=0.7.2 and the installer one-liner, " +
	"and to bring the data back as it was, stop the daemon, move the data dir aside to /data.broken and run seamlessd import --from /data.broken/backups/pre-update-v0.7.2-20261009T114500Z.tar.gz"

// gateRefusal is the gates stage's refusal when the seamlessd on disk is not
// the release the daemon runs: T on disk, F running.
const gateRefusal = "gates: the installed seamlessd is not v0.7.2, the release this attempt updates from; restart the service so it runs the installed release"

// TestUpdatesCheck_Automatic: the updates row of a daemon that updates
// itself, and of one whose record says it cannot, from state.json and
// attempt.json alone.
func TestUpdatesCheck_Automatic(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	at := func(t time.Time) string { return t.Local().Format(timeLayout) }
	no := false
	tests := []struct {
		name  string
		state update.State
		// attempt is attempt.json, and spawn whether the daemon's record
		// names it as the spawn it waits on.
		attempt func(t *testing.T) *update.Attempt
		spawn   bool
		auto    *bool
		status  checkStatus
		want    []string
		not     []string
	}{
		{
			name:   "up to date",
			state:  autoRecord(now, nil),
			status: statusOK, want: []string{"automatic; up to date at v0.7.2 (checked 2h ago; installer install)"},
		},
		{
			name: "a soaked target with its pending deadline",
			state: autoRecord(now, func(s *update.State) {
				s.Releases = append(s.Releases, signed(v073, now.Add(-48*time.Hour)))
				s.Pending = &update.Pending{Running: v072, Since: now.Add(-2 * time.Hour)}
			}),
			status: statusInfo,
			want: []string{"automatic, running v0.7.2 (checked 2h ago); installs v0.7.3 once no agent session is live, or at a lull in requests from " +
				at(now.Add(22*time.Hour)) + " (in 22h)"},
		},
		{
			name:   "a soaked target the daemon has not started the clock for",
			state:  autoRecord(now, func(s *update.State) { s.Releases = append(s.Releases, signed(v073, now.Add(-48*time.Hour))) }),
			status: statusInfo, want: []string{"installs v0.7.3 once no agent session is live, or at a lull in requests after waiting 24h"},
		},
		{
			name: "past its deadline",
			state: autoRecord(now, func(s *update.State) {
				s.Releases = append(s.Releases, signed(v073, now.Add(-48*time.Hour)))
				s.Pending = &update.Pending{Running: v072, Since: now.Add(-30 * time.Hour)}
			}),
			status: statusInfo, want: []string{"now that its deadline (" + at(now.Add(-6*time.Hour)) + ") has passed"},
		},
		{
			name:   "soaking",
			state:  autoRecord(now, func(s *update.State) { s.Releases = append(s.Releases, signed(v073, now.Add(-2*time.Hour))) }),
			status: statusInfo, want: []string{"installs v0.7.3 once it is 24h old (at " + at(now.Add(22*time.Hour)) + ", in 22h)"},
		},
		{
			// This machine's clock runs two days ahead: the daemon checked a
			// minute ago by it, and GitHub's Date said two days earlier. The
			// soak runs on GitHub's clock, so it is not over.
			name: "soaking, on a machine clock two days ahead",
			state: autoRecord(now, func(s *update.State) {
				s.CheckedAt = now.Add(-time.Minute)
				s.ServerDate = now.Add(-48*time.Hour - time.Minute)
				s.Releases = append(s.Releases, signed(v073, now.Add(-50*time.Hour)))
			}),
			status: statusInfo, want: []string{"installs v0.7.3 once it is 24h old (at " + at(now.Add(-26*time.Hour)) + ", in 22h)"},
			not: []string{"once no agent session is live"},
		},
		{
			name: "a pending deadline, on a machine clock two days ahead",
			state: autoRecord(now, func(s *update.State) {
				s.CheckedAt = now.Add(-time.Minute)
				s.ServerDate = now.Add(-48*time.Hour - time.Minute)
				s.Releases = append(s.Releases, signed(v073, now.Add(-96*time.Hour)))
				s.Pending = &update.Pending{Running: v072, Since: now.Add(-49 * time.Hour)}
			}),
			status: statusInfo, want: []string{"or at a lull in requests from " + at(now.Add(-25*time.Hour)) + " (in 23h)"},
		},
		{
			name: "a soaked target, and a newer one soaking",
			state: autoRecord(now, func(s *update.State) {
				s.Releases = append(s.Releases, signed(v073, now.Add(-48*time.Hour)), signed(v074, now.Add(-time.Hour)))
			}),
			status: statusInfo,
			want:   []string{"installs v0.7.3 once", "; then v0.7.4, once it is 24h old (at " + at(now.Add(23*time.Hour)) + ")"},
		},
		{
			name: "the newest release blocked is the owner's to install",
			state: autoRecord(now, func(s *update.State) {
				s.Releases = append(s.Releases, signed(v073, now.Add(-48*time.Hour)))
				s.Blocks = []update.Block{{Version: v073, Reason: update.BlockRolledBack, At: now.Add(-time.Hour)}}
			}),
			status: statusWarn,
			want: []string{"v0.7.3 is available (running v0.7.2; checked 2h ago), but automatic updates skip it: the update to it rolled back" +
				" -- update with: seamlessd update"},
		},
		{
			name: "a release without the checksums bundle is the owner's to install",
			state: autoRecord(now, func(s *update.State) {
				s.Releases = append(s.Releases, update.Release{Version: v073, PublishedAt: now.Add(-48 * time.Hour)})
			}),
			status: statusWarn, want: []string{"but automatic updates skip it: it carries no signed checksums bundle"},
		},
		{
			name: "a held release is the owner's own pin",
			state: autoRecord(now, func(s *update.State) {
				s.Releases = append(s.Releases, signed(v073, now.Add(-48*time.Hour)))
				s.Hold = &update.Hold{Through: v073, From: v073, At: now.Add(-time.Hour)}
			}),
			status: statusInfo,
			want:   []string{"v0.7.3 is held back: this install went back from v0.7.3, so automatic updates skip releases up to v0.7.3 until they are resumed in the console"},
		},
		{
			name: "an older target, the newest blocked",
			state: autoRecord(now, func(s *update.State) {
				s.Releases = append(s.Releases, signed(v073, now.Add(-48*time.Hour)), signed(v074, now.Add(-48*time.Hour)))
				s.Blocks = []update.Block{{Version: v074, Reason: update.BlockVerify, At: now.Add(-time.Hour)}}
			}),
			status: statusWarn,
			want:   []string{"installs v0.7.3 once", "; v0.7.4 is skipped: it did not pass verification -- install it by hand with: seamlessd update"},
		},
		{
			name: "a backoff, and the gate refusal it follows, worded as the owner's action",
			state: autoRecord(now, func(s *update.State) {
				s.Releases = append(s.Releases, signed(v073, now.Add(-48*time.Hour)))
				s.Backoff = &update.Backoff{Until: now.Add(time.Hour), Count: 2, Version: v073, Reason: update.OutcomeFailed}
				s.LastAttempt = &update.AttemptResult{
					ID: "01ATTEMPT", From: v072, To: v073, Why: update.WhyAuto, Outcome: update.OutcomeFailed,
					Stage: update.StageGates, Error: gateRefusal, FinishedAt: now.Add(-20 * time.Minute), FoldedAt: now.Add(-19 * time.Minute),
				}
			}),
			status: statusInfo,
			want: []string{
				"the next attempt waits out a backoff until " + at(now.Add(time.Hour)) + " (in 1h), after 2 failures in a row on v0.7.3",
				"; the last attempt (v0.7.2 -> v0.7.3, automatic, 20m ago) failed at gates, leaving the install as it was: " +
					"the installed seamlessd is not v0.7.2, the release this attempt updates from; restart the service so it runs the installed release",
			},
			not: []string{"gates: gates:", ": gates: the installed"},
		},
		{
			name: "a stale-binary refusal names the restart as the owner's action",
			state: autoRecord(now, func(s *update.State) {
				s.Releases = append(s.Releases, signed(v073, now.Add(-48*time.Hour)))
				s.Backoff = &update.Backoff{Until: now.Add(time.Hour), Count: 2, Version: v073, Reason: update.OutcomeFailed}
				s.LastAttempt = &update.AttemptResult{
					ID: "01ATTEMPT", From: v072, To: v073, Why: update.WhyAuto, Outcome: update.OutcomeFailed,
					Stage: update.StageGates, Error: gateRefusal, Refusal: update.RefusalStaleBinary,
					FinishedAt: now.Add(-20 * time.Minute), FoldedAt: now.Add(-19 * time.Minute),
				}
			}),
			status: statusWarn,
			want: []string{
				"; the last attempt (v0.7.2 -> v0.7.3, automatic, 20m ago) failed at gates, leaving the install as it was: " +
					"the installed seamlessd is not v0.7.2, the release this attempt updates from; restart the service so it runs the installed release" +
					" -- restart the service so it runs the installed release: seamlessd restart",
			},
		},
		{
			name: "a refusal word from a newer updater reads as a plain failure",
			state: autoRecord(now, func(s *update.State) {
				s.Releases = append(s.Releases, signed(v073, now.Add(-48*time.Hour)))
				s.Backoff = &update.Backoff{Until: now.Add(time.Hour), Count: 1, Version: v073, Reason: update.OutcomeFailed}
				s.LastAttempt = &update.AttemptResult{
					ID: "01ATTEMPT", From: v072, To: v073, Why: update.WhyAuto, Outcome: update.OutcomeFailed,
					Stage: update.StageGates, Error: "gates: the disk is full", Refusal: "disk_full",
					FinishedAt: now.Add(-20 * time.Minute), FoldedAt: now.Add(-19 * time.Minute),
				}
			}),
			status: statusInfo,
			want:   []string{"; the last attempt (v0.7.2 -> v0.7.3, automatic, 20m ago) failed at gates, leaving the install as it was: the disk is full"},
			not:    []string{" -- ", "disk_full", "seamlessd restart"},
		},
		{
			name: "a refusal on the way from another version names no action",
			state: autoRecord(now, func(s *update.State) {
				s.Releases = append(s.Releases, signed(v073, now.Add(-48*time.Hour)))
				s.LastAttempt = &update.AttemptResult{
					ID: "01ATTEMPT", From: v071, To: v073, Why: update.WhyAuto, Outcome: update.OutcomeFailed,
					Stage: update.StageGates, Error: "gates: the installed seamlessd is not v0.7.1", Refusal: update.RefusalStaleBinary,
					FinishedAt: now.Add(-time.Hour), FoldedAt: now.Add(-time.Hour),
				}
			}),
			status: statusInfo, not: []string{"last attempt", "seamlessd restart"},
		},
		{
			name: "a refusal the daemon has not settled yet, read from attempt.json",
			state: autoRecord(now, func(s *update.State) {
				s.Releases = append(s.Releases, signed(v073, now.Add(-48*time.Hour)))
			}),
			attempt: func(t *testing.T) *update.Attempt {
				a := finishedRecord(t, now, v073, update.WhyAuto, func(a *update.Attempt) {
					a.Stage, a.Error, a.Refusal = update.StageGates, "gates: not an installer-managed install (unknown): moved", update.RefusalNotInstaller
				})
				return &a
			},
			status: statusWarn,
			want: []string{
				"failed at gates, leaving the install as it was: not an installer-managed install (unknown): moved (not yet settled by the daemon)" +
					" -- restart the service so it re-reads how it was installed: seamlessd restart",
			},
		},
		{
			name: "a refusal's action comes first, the row's own after it",
			state: autoRecord(now, func(s *update.State) {
				s.Releases = append(s.Releases, signed(v073, now.Add(-48*time.Hour)))
				s.Blocks = []update.Block{{Version: v073, Reason: update.BlockVerify, At: now.Add(-2 * time.Hour)}}
				s.LastAttempt = &update.AttemptResult{
					ID: "01ATTEMPT", From: v072, To: v073, Why: update.WhyNow, Outcome: update.OutcomeFailed,
					Stage: update.StageGates, Error: "gates: the updater must run outside the service's process tree", Refusal: update.RefusalSelfCheck,
					FinishedAt: now.Add(-time.Hour), FoldedAt: now.Add(-time.Hour),
				}
			}),
			status: statusWarn,
			want:   []string{" -- update by hand from a terminal: seamlessd update; update with: seamlessd update"},
		},
		{
			name: "automatic updates turned off before it began: words only, no action",
			state: autoRecord(now, func(s *update.State) {
				s.Releases = append(s.Releases, signed(v073, now.Add(-48*time.Hour)))
				s.LastAttempt = &update.AttemptResult{
					ID: "01ATTEMPT", From: v072, To: v073, Why: update.WhyAuto, Outcome: update.OutcomeFailed,
					Stage: update.StageGates, Error: "gates: automatic updates are off (set by console)", Refusal: update.RefusalAutoOff,
					FinishedAt: now.Add(-time.Hour), FoldedAt: now.Add(-time.Hour),
				}
			}),
			status: statusInfo,
			want:   []string{"failed at gates, leaving the install as it was: automatic updates are off (set by console)"},
			not:    []string{" -- "},
		},
		{
			name: "applied with warnings names install-hooks",
			state: autoRecord(now, func(s *update.State) {
				s.LastAttempt = &update.AttemptResult{
					ID: "01ATTEMPT", From: v071, To: v072, Why: update.WhyAuto, Outcome: update.OutcomeApplied, Stage: update.StageDone,
					Warnings: true, Error: "applied with warnings: v0.7.2 is serving, but the installer exited with exit status 1",
					FinishedAt: now.Add(-time.Hour), FoldedAt: now.Add(-time.Hour),
				}
			}),
			status: statusInfo,
			want:   []string{"up to date at v0.7.2", "; the update to v0.7.2 applied with warnings: finish wiring the clients with seamlessd install-hooks"},
		},
		{
			name: "a stale failure from another version says nothing",
			state: autoRecord(now, func(s *update.State) {
				s.LastAttempt = &update.AttemptResult{
					ID: "01ATTEMPT", From: v071, To: v073, Why: update.WhyAuto, Outcome: update.OutcomeRolledBack, Stage: update.StageRollback,
					RolledBack: true, Error: "v0.7.3 did not come up", FinishedAt: now.Add(-48 * time.Hour), FoldedAt: now.Add(-48 * time.Hour),
				}
			}),
			status: statusOK, not: []string{"last attempt"},
		},
		{
			name: "no updater wired: the same record notifies",
			state: autoRecord(now, func(s *update.State) {
				s.Running.CanApply = false
				s.Releases = append(s.Releases, signed(v073, now.Add(-48*time.Hour)))
			}),
			status: statusWarn, want: []string{"v0.7.3 is available (running v0.7.2; checked 2h ago) -- update with: seamlessd update"},
			not: []string{"automatic"},
		},
		{
			name:   "update.auto false notifies",
			state:  autoRecord(now, func(s *update.State) { s.Releases = append(s.Releases, signed(v073, now.Add(-48*time.Hour))) }),
			auto:   &no,
			status: statusWarn, want: []string{"v0.7.3 is available"}, not: []string{"automatic"},
		},
		{
			name: "paused after rollbacks",
			state: autoRecord(now, func(s *update.State) {
				s.Releases = append(s.Releases, signed(v073, now.Add(-72*time.Hour)), signed(v074, now.Add(-48*time.Hour)),
					signed(update.Version{Major: 0, Minor: 7, Patch: 5}, now.Add(-time.Hour)))
				s.Blocks = []update.Block{{Version: v073, Reason: update.BlockRolledBack}, {Version: v074, Reason: update.BlockRolledBack}}
				s.Paused = &update.Pause{Reason: update.PauseRollbacks, Versions: []update.Version{v073, v074}, At: now.Add(-3 * time.Hour)}
				s.LastAttempt = &update.AttemptResult{
					ID: "01ATTEMPT", From: v072, To: v074, Why: update.WhyAuto, Outcome: update.OutcomeRolledBack, Stage: update.StageRollback,
					RolledBack: true, Error: "v0.7.4 did not come up (no answer); rolled back to v0.7.2",
					FinishedAt: now.Add(-3 * time.Hour), FoldedAt: now.Add(-3 * time.Hour),
				}
			}),
			status: statusWarn,
			want: []string{
				"automatic updates paused themselves 3h ago: two updates in a row rolled back (v0.7.3, v0.7.4)",
				"; the last attempt (v0.7.2 -> v0.7.4, automatic, 3h ago) rolled back: v0.7.4 did not come up (no answer); rolled back to v0.7.2",
				"; v0.7.5 is available (running v0.7.2) -- resume them in the console under Settings > Updates, or update by hand with: seamlessd update",
			},
		},
		{
			name: "paused after an update that could not be rolled back: the recovery steps",
			state: autoRecord(now, func(s *update.State) {
				s.Releases = append(s.Releases, signed(v073, now.Add(-48*time.Hour)))
				s.Blocks = []update.Block{{Version: v073, Reason: update.BlockBroken}}
				s.Paused = &update.Pause{Reason: update.PauseBroken, Versions: []update.Version{v073}, At: now.Add(-time.Hour)}
				s.LastAttempt = &update.AttemptResult{
					ID: "01ATTEMPT", From: v072, To: v073, Why: update.WhyAuto, Outcome: update.OutcomeBroken, Stage: update.StageRollback,
					Error: failedRollbackError, LogPath: "/data/update/logs/01ATTEMPT.log",
					FinishedAt: now.Add(-time.Hour), FoldedAt: now.Add(-time.Hour),
				}
			}),
			status: statusWarn,
			want: []string{
				"automatic updates paused themselves 1h ago, because the update from v0.7.2 to v0.7.3 (automatic, 1h ago) could not be rolled back cleanly: " +
					failedRollbackError + "; log: /data/update/logs/01ATTEMPT.log -- resume them in the console under Settings > Updates once the install is sound",
			},
			not: []string{"update by hand", "is available"},
		},
		{
			name: "an update that could not be rolled back, unsettled while the daemon is down",
			state: autoRecord(now, func(s *update.State) {
				s.Releases = append(s.Releases, signed(v073, now.Add(-48*time.Hour)))
			}),
			attempt: func(t *testing.T) *update.Attempt {
				a := finishedRecord(t, now, v073, update.WhyAuto, func(a *update.Attempt) {
					a.Stage, a.Error = update.StageRollback, failedRollbackError
				})
				return &a
			},
			spawn:  true,
			status: statusWarn,
			want: []string{
				"the update from v0.7.2 to v0.7.3 (automatic, 10m ago) could not be rolled back cleanly: " + failedRollbackError,
				"(the daemon has not settled this attempt; is it running? seamlessd status)",
			},
		},
		{
			name: "spawned, the updater not recorded yet",
			state: autoRecord(now, func(s *update.State) {
				s.Releases = append(s.Releases, signed(v073, now.Add(-48*time.Hour)))
				s.Spawn = &update.Spawn{ID: "01SPAWN", From: v072, To: v073, Why: update.WhyAuto, Path: update.PathIdle, At: now.Add(-time.Minute)}
			}),
			status: statusInfo, want: []string{"installing v0.7.3 now (automatic; the updater is starting, spawned 1m ago)"},
		},
		{
			name: "spawned, the updater at work",
			state: autoRecord(now, func(s *update.State) {
				s.Releases = append(s.Releases, signed(v073, now.Add(-48*time.Hour)))
			}),
			attempt: func(t *testing.T) *update.Attempt {
				return &update.Attempt{
					ID: testAttemptID(t), From: v072, To: v073, Why: update.WhyAuto, Stage: update.StageInstall,
					StartedAt: now.Add(-3 * time.Minute), HeartbeatAt: now.Add(-20 * time.Second),
				}
			},
			spawn:  true,
			status: statusInfo, want: []string{"installing v0.7.3 now (automatic; stage install, started 3m ago)"},
		},
		{
			name:  "a manual update at work",
			state: autoRecord(now, nil),
			attempt: func(t *testing.T) *update.Attempt {
				return &update.Attempt{
					ID: testAttemptID(t), From: v072, To: v073, Why: update.WhyManual, Stage: update.StageConfirm,
					StartedAt: now.Add(-2 * time.Minute), HeartbeatAt: now.Add(-5 * time.Second),
				}
			},
			status: statusInfo, want: []string{"installing v0.7.3 now (seamlessd update; stage confirm, started 2m ago)"},
		},
		{
			name: "a spawned updater gone quiet",
			state: autoRecord(now, func(s *update.State) {
				s.Releases = append(s.Releases, signed(v073, now.Add(-48*time.Hour)))
			}),
			attempt: func(t *testing.T) *update.Attempt {
				return &update.Attempt{
					ID: testAttemptID(t), From: v072, To: v073, Why: update.WhyAuto, Stage: update.StageInstall,
					StartedAt: now.Add(-20 * time.Minute), HeartbeatAt: now.Add(-12 * time.Minute),
				}
			},
			spawn:  true,
			status: statusInfo,
			want:   []string{"the update to v0.7.3 (automatic) has gone quiet: no word from the updater for 12m, last at stage install"},
		},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			dir := t.TempDir()
			st := tt.state
			if tt.attempt != nil {
				a := tt.attempt(t)
				require.NoError(t, update.WriteAttempt(dir, *a))
				if tt.spawn {
					st.Spawn = &update.Spawn{ID: a.ID, From: a.From, To: a.To, Why: a.Why, Path: update.PathIdle, At: a.StartedAt}
				}
			}
			require.NoError(t, update.SaveState(dir, st))
			cfg := config.Defaults()
			cfg.DataDir = dir
			cfg.Update.Auto = tt.auto
			got := updatesCheck(context.Background(), cfg, nil, now)
			require.Equal(t, tt.status, got.status, got.detail)
			for _, w := range tt.want {
				require.Contains(t, got.detail, w)
			}
			for _, n := range tt.not {
				require.NotContains(t, got.detail, n)
			}
		})
	}
}

// TestUpdateView_Mode: the CLI reads the daemon's own facts, so a daemon
// that updates itself is ModeAuto here too, and the same record without an
// updater, or paused, is ModeNotify.
func TestUpdateView_Mode(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name  string
		state update.State
		want  update.Mode
	}{
		{"an updater wired", autoRecord(now, nil), update.ModeAuto},
		{"no updater wired", releaseRecord(now, nil), update.ModeNotify},
		{"paused", autoRecord(now, func(s *update.State) {
			s.Paused = &update.Pause{Reason: update.PauseRollbacks, At: now}
		}), update.ModeNotify},
		{"held, still automatic", autoRecord(now, func(s *update.State) {
			s.Hold = &update.Hold{Through: v073, From: v073, At: now}
		}), update.ModeAuto},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, update.SaveState(dir, tc.state))
			cfg := config.Defaults()
			cfg.DataDir = dir
			mode, reason := loadUpdateView(context.Background(), cfg, nil).status().Mode()
			require.Equal(t, tc.want, mode, reason)
		})
	}
}

// TestUpdateCheckRows_Automatic: `update --check`'s detail rows for a daemon
// that updates itself.
func TestUpdateCheckRows_Automatic(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	at := func(t time.Time) string { return t.Local().Format(timeLayout) }
	dir := t.TempDir()
	logPath := filepath.Join(dir, "update", "logs", "attempt.log")
	require.NoError(t, os.MkdirAll(filepath.Dir(logPath), 0o700))
	require.NoError(t, os.WriteFile(logPath, []byte("log\n"), 0o600))
	backup := filepath.Join(dir, "backups", "pre-update-v0.7.2-20261009T090000Z.tar.gz")

	v075 := update.Version{Major: 0, Minor: 7, Patch: 5}
	rec := finishedRecord(t, now, v075, update.WhyAuto, func(a *update.Attempt) {
		a.Stage, a.RolledBack = update.StageRollback, true
		a.Error = "v0.7.5 did not come up (no answer); rolled back to v0.7.2"
		a.LogPath, a.BackupPath = logPath, backup
	})
	require.NoError(t, update.WriteAttempt(dir, rec))
	st := autoRecord(now, func(s *update.State) {
		s.Releases = append(s.Releases, signed(v073, now.Add(-48*time.Hour)), signed(v074, now.Add(-time.Hour)), signed(v075, now.Add(-72*time.Hour)))
		s.Pending = &update.Pending{Running: v072, Since: now.Add(-2 * time.Hour)}
		s.Blocks = []update.Block{
			{Version: v075, Reason: update.BlockRolledBack, Attempt: rec.ID, At: now.Add(-10 * time.Minute)},
			{Version: v071, Reason: update.BlockVerify, At: now.Add(-time.Hour)}, // at or below the running version: inert
		}
		s.Backoff = &update.Backoff{Until: now.Add(2 * time.Hour), Count: 1, Version: v073, Reason: update.OutcomeInterrupted}
		s.LastAttempt = &update.AttemptResult{
			ID: rec.ID, From: v072, To: v075, Why: update.WhyAuto, Outcome: update.OutcomeRolledBack, Stage: update.StageRollback,
			RolledBack: true, Error: rec.Error, LogPath: logPath, StartedAt: rec.StartedAt, FinishedAt: rec.FinishedAt, FoldedAt: now.Add(-9 * time.Minute),
		}
	})
	require.NoError(t, update.SaveState(dir, st))
	require.NoError(t, os.WriteFile(filepath.Join(update.StateDir(dir), confirmDrillName), nil, 0o600))

	cfg := config.Defaults()
	cfg.DataDir = dir
	var buf bytes.Buffer
	updateCheckRows(&buf, loadUpdateView(context.Background(), cfg, nil), now)
	out := buf.String()
	for _, want := range []string{
		"mode     auto -- installs new releases by itself once they are 24h old",
		"target   v0.7.3 (soaked since " + at(now.Add(-24*time.Hour)) + "); then v0.7.4 (soaks until " + at(now.Add(23*time.Hour)) + ", in 23h)" +
			" -- v0.7.5 is blocked (below)",
		"pending  since " + at(now.Add(-2*time.Hour)) + "; deadline " + at(now.Add(22*time.Hour)) + " (in 22h)",
		"last     v0.7.2 -> v0.7.5 rolled back (automatic, 10m ago)",
		"error    v0.7.5 did not come up (no answer); rolled back to v0.7.2",
		"log      " + tildePath(logPath) + "\n",
		"backup   " + tildePath(backup) + " (removed; the newest 2 backups are kept)",
		"blocked  v0.7.5 (the update to it rolled back, " + at(now.Add(-10*time.Minute)) + ")",
		"backoff  next attempt in 2h (" + at(now.Add(2*time.Hour)) + ") -- 1 failure in a row on v0.7.3; the last was interrupted",
		"drill    on -- ",
		"so every update fails its confirmation and rolls back",
	} {
		require.Contains(t, out, want)
	}
	require.NotContains(t, out, "v0.7.1 (", "a block at or below the running version is inert")
	require.NotContains(t, out, "fix ", "only an update that applied with warnings needs install-hooks")
}

// TestUpdateCheckRows_SkewedClock: on a machine whose clock runs two days
// ahead of GitHub's, `update --check` measures the soak and the pending
// deadline on GitHub's clock, as the daemon does, so a soak the daemon still
// waits out never reads as over.
func TestUpdateCheckRows_SkewedClock(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	at := func(t time.Time) string { return t.Local().Format(timeLayout) }
	dir := t.TempDir()
	require.NoError(t, update.SaveState(dir, autoRecord(now, func(s *update.State) {
		s.CheckedAt = now.Add(-time.Minute)
		s.ServerDate = now.Add(-48*time.Hour - time.Minute)
		s.Releases = append(s.Releases, signed(v073, now.Add(-96*time.Hour)), signed(v074, now.Add(-50*time.Hour)))
		s.Pending = &update.Pending{Running: v072, Since: now.Add(-49 * time.Hour)}
	})))

	cfg := config.Defaults()
	cfg.DataDir = dir
	var buf bytes.Buffer
	updateCheckRows(&buf, loadUpdateView(context.Background(), cfg, nil), now)
	out := buf.String()
	require.Contains(t, out, "target   v0.7.3 (soaked since "+at(now.Add(-72*time.Hour))+"); then v0.7.4 (soaks until "+at(now.Add(-26*time.Hour))+", in 22h)")
	require.Contains(t, out, "pending  since "+at(now.Add(-49*time.Hour))+"; deadline "+at(now.Add(-25*time.Hour))+" (in 23h)")
}

// TestUpdateCheckRows_GateRefusal: a gate refusal that still concerns this
// install -- the daemon runs the release it started from -- gets a fix row
// with the owner's action, keyed on the refusal's word; once the daemon runs
// another version, or for a word this release does not know, there is none.
func TestUpdateCheckRows_GateRefusal(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	for _, tc := range []struct {
		name    string
		from    update.Version
		refusal string
		fix     string
	}{
		{"stale binary", v072, update.RefusalStaleBinary, "fix      restart the service so it runs the installed release: seamlessd restart\n"},
		{"self-check", v072, update.RefusalSelfCheck, "fix      update by hand from a terminal: seamlessd update\n"},
		{"automatic updates off: no action", v072, update.RefusalAutoOff, ""},
		{"a newer updater's word", v072, "disk_full", ""},
		{"from another version", v071, update.RefusalStaleBinary, ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			dir := t.TempDir()
			require.NoError(t, update.SaveState(dir, autoRecord(now, func(s *update.State) {
				s.LastAttempt = &update.AttemptResult{
					ID: "01ATTEMPT", From: tc.from, To: v073, Why: update.WhyAuto, Outcome: update.OutcomeFailed,
					Stage: update.StageGates, Error: "gates: refused", Refusal: tc.refusal,
					FinishedAt: now.Add(-time.Hour), FoldedAt: now.Add(-time.Hour),
				}
			})))
			cfg := config.Defaults()
			cfg.DataDir = dir
			var buf bytes.Buffer
			updateCheckRows(&buf, loadUpdateView(context.Background(), cfg, nil), now)
			out := buf.String()
			require.Contains(t, out, "error    refused\n")
			if tc.fix == "" {
				require.NotContains(t, out, "fix ")
				return
			}
			require.Contains(t, out, tc.fix)
		})
	}
}

// TestUpdateCheckRows_PauseHoldAndWarnings: the rows that show whatever the
// mode -- a pause, a hold -- and the install-hooks hint after an update that
// applied with warnings.
func TestUpdateCheckRows_PauseHoldAndWarnings(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	at := func(t time.Time) string { return t.Local().Format(timeLayout) }
	dir := t.TempDir()
	st := autoRecord(now, func(s *update.State) {
		s.Paused = &update.Pause{Reason: update.PauseRollbacks, Versions: []update.Version{v073, v074}, At: now.Add(-3 * time.Hour)}
		s.Hold = &update.Hold{Through: v074, From: v074, At: now.Add(-5 * time.Hour)}
		s.LastAttempt = &update.AttemptResult{
			ID: "01ATTEMPT", From: v071, To: v072, Why: update.WhyNow, Outcome: update.OutcomeApplied, Stage: update.StageDone, Warnings: true,
			Error: "applied with warnings: v0.7.2 is serving, but the installer exited with exit status 1", FinishedAt: now.Add(-time.Hour), FoldedAt: now.Add(-time.Hour),
		}
	})
	require.NoError(t, update.SaveState(dir, st))
	cfg := config.Defaults()
	cfg.DataDir = dir
	var buf bytes.Buffer
	updateCheckRows(&buf, loadUpdateView(context.Background(), cfg, nil), now)
	out := buf.String()
	for _, want := range []string{
		"mode     notify -- automatic updates paused themselves",
		"last     v0.7.1 -> v0.7.2 applied with warnings (Update now, 1h ago)",
		"error    applied with warnings: v0.7.2 is serving",
		"fix      seamlessd install-hooks",
		"hold     releases up to v0.7.4 are skipped -- this install went back from v0.7.4 on " + at(now.Add(-5*time.Hour)),
		"paused   since " + at(now.Add(-3*time.Hour)) + " -- two updates in a row rolled back (v0.7.3, v0.7.4); resume automatic updates in the console",
	} {
		require.Contains(t, out, want)
	}
	require.NotContains(t, out, "target", "a paused daemon does not update itself, so it has no target")
}

// TestUpdateCheckRows_PauseWordedOnce: the mode row (Status.Mode) and the
// paused row word one pause alike, with update.PauseWords, for each reason.
// The paused row used to say "updates rolled back in a row" right under a mode
// row saying "two updates in a row rolled back", and "the update to v0.7.3
// could not be rolled back cleanly" under "an update could not be rolled back
// cleanly".
func TestUpdateCheckRows_PauseWordedOnce(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	for reason, versions := range map[string][]update.Version{
		update.PauseRollbacks: {v073, v074},
		update.PauseBroken:    {v073},
	} {
		dir := t.TempDir()
		require.NoError(t, update.SaveState(dir, autoRecord(now, func(s *update.State) {
			s.Paused = &update.Pause{Reason: reason, Versions: versions, At: now.Add(-time.Hour)}
		})))
		cfg := config.Defaults()
		cfg.DataDir = dir
		var buf bytes.Buffer
		updateCheckRows(&buf, loadUpdateView(context.Background(), cfg, nil), now)
		out := buf.String()

		words := update.PauseWords(reason)
		names := "v0.7.3"
		if len(versions) == 2 {
			names = "v0.7.3, v0.7.4"
		}
		require.Contains(t, out, "mode     notify -- automatic updates paused themselves ("+words+")", reason)
		require.Contains(t, out, " -- "+words+" ("+names+"); resume automatic updates in the console", reason)
		require.Contains(t, updatesCheck(context.Background(), cfg, nil, now).detail,
			"automatic updates paused themselves 1h ago: "+words+" ("+names+")", reason)
	}
}

// TestUpdatesCheck_SkipsWordedAsUpdate: why automatic updates skip the newest
// release reads in update's own words -- update.HeldBack's, update.BlockWords
// for each block reason -- on the doctor row and the blocked row alike.
func TestUpdatesCheck_SkipsWordedAsUpdate(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	render := func(st update.State) (doctor, rows string) {
		dir := t.TempDir()
		require.NoError(t, update.SaveState(dir, st))
		cfg := config.Defaults()
		cfg.DataDir = dir
		var buf bytes.Buffer
		updateCheckRows(&buf, loadUpdateView(context.Background(), cfg, nil), now)
		return updatesCheck(context.Background(), cfg, nil, now).detail, buf.String()
	}
	for _, reason := range []string{update.BlockRolledBack, update.BlockVerify, update.BlockInstall, update.BlockBroken} {
		doctor, rows := render(autoRecord(now, func(s *update.State) {
			s.Releases = append(s.Releases, signed(v073, now.Add(-48*time.Hour)))
			s.Blocks = []update.Block{{Version: v073, Reason: reason, At: now.Add(-time.Hour)}}
		}))
		require.Contains(t, doctor, "but automatic updates skip it: "+update.BlockWords(reason)+" -- update with: seamlessd update", reason)
		require.Contains(t, rows, "blocked  v0.7.3 ("+update.BlockWords(reason)+", ", reason)
	}
	require.Contains(t, update.BlockWords(update.BlockInstall), "3 times in a row")

	doctor, _ := render(autoRecord(now, func(s *update.State) {
		s.Releases = append(s.Releases, update.Release{Version: v073, PublishedAt: now.Add(-48 * time.Hour)})
	}))
	_, why := update.HeldBack(update.Release{Version: v073}, nil, nil)
	require.Contains(t, doctor, "but automatic updates skip it: "+why+" -- update with: seamlessd update")

	doctor, _ = render(autoRecord(now, func(s *update.State) {
		s.Releases = append(s.Releases, signed(v073, now.Add(-48*time.Hour)))
		s.Hold = &update.Hold{Through: v073, From: v073, At: now.Add(-time.Hour)}
	}))
	_, why = update.HeldBack(signed(v073, now), nil, &update.Hold{Through: v073, From: v073})
	require.Contains(t, doctor, "v0.7.3 is held back: "+why+" in the console")
}

// TestUpdateCheckRows_UnderWay: the update row, for an attempt the updater
// reports running and for one not settled yet.
func TestUpdateCheckRows_UnderWay(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	dir := t.TempDir()
	a := update.Attempt{
		ID: testAttemptID(t), From: v072, To: v073, Why: update.WhyAuto, Stage: update.StageConfirm,
		StartedAt: now.Add(-4 * time.Minute), HeartbeatAt: now.Add(-10 * time.Second),
	}
	require.NoError(t, update.WriteAttempt(dir, a))
	require.NoError(t, update.SaveState(dir, autoRecord(now, func(s *update.State) {
		s.Releases = append(s.Releases, signed(v073, now.Add(-48*time.Hour)))
		s.Spawn = &update.Spawn{ID: a.ID, From: v072, To: v073, Why: update.WhyAuto, Path: update.PathIdle, At: now.Add(-5 * time.Minute)}
	})))
	cfg := config.Defaults()
	cfg.DataDir = dir
	var buf bytes.Buffer
	updateCheckRows(&buf, loadUpdateView(context.Background(), cfg, nil), now)
	require.Contains(t, buf.String(), "update   v0.7.2 -> v0.7.3 (automatic): stage confirm, started 4m ago")
	require.NotContains(t, buf.String(), "last ", "an attempt under way is not the last one")
}

func TestUpdateDrillCheck(t *testing.T) {
	cfg := config.Defaults()
	cfg.DataDir = t.TempDir()
	_, ok := updateDrillCheck(cfg)
	require.False(t, ok, "no switch, nothing to say")

	path := filepath.Join(update.StateDir(cfg.DataDir), confirmDrillName)
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	require.NoError(t, os.WriteFile(path, nil, 0o600))
	c, ok := updateDrillCheck(cfg)
	require.True(t, ok)
	require.Equal(t, statusWarn, c.status)
	require.Equal(t, "update drill", c.name)
	require.Contains(t, c.detail, path+" exists: the rollback drill is on, so every update fails its confirmation and rolls back -- delete it to end the drill")
	require.Contains(t, updateConfirmChecks(cfg), c)
}

func TestTLSTrustCheck(t *testing.T) {
	year := time.Now().Add(365 * 24 * time.Hour)
	cert, key := writeCertPair(t, year, "seam.lan", "127.0.0.1")
	otherCert, otherKey := writeCertPair(t, year, "other.lan")
	notPEM := filepath.Join(t.TempDir(), "ca.pem")
	require.NoError(t, os.WriteFile(notPEM, []byte("not a certificate"), 0o600))
	// A chain whose second certificate does not parse still loads (only the
	// leaf is parsed), and the daemon serves it; the client's handshake does
	// parse it, and fails.
	leafPEM, err := os.ReadFile(cert)
	require.NoError(t, err)
	badChain := filepath.Join(t.TempDir(), "chain.pem")
	require.NoError(t, os.WriteFile(badChain, append(leafPEM, []byte("-----BEGIN CERTIFICATE-----\nAAAA\n-----END CERTIFICATE-----\n")...), 0o600))

	for _, tc := range []struct {
		name   string
		cfg    config.Config
		report bool
		status checkStatus
		want   string
		fix    string
	}{
		{name: "plain http, a usable client: nothing to say", cfg: config.Config{Addr: "127.0.0.1:8081"}},
		{
			name:   "an unusable tls.ca_file fails every dial, TLS or not",
			cfg:    config.Config{Addr: "127.0.0.1:8081", TLS: config.TLS{CAFile: notPEM}},
			report: true, status: statusWarn, want: "no PEM certificate found -- this machine's hooks cannot reach the daemon",
		},
		{
			name:   "a private CA the system does not trust, no tls.ca_file",
			cfg:    config.Config{Addr: "0.0.0.0:8443", AdvertisedURL: "https://seam.lan:8443", TLS: config.TLS{CertFile: cert, KeyFile: key}},
			report: true, status: statusWarn, want: "this machine's client cannot verify https://seam.lan:8443 (",
			fix: "set tls.ca_file to the CA that signed " + cert,
		},
		{
			name:   "tls.ca_file names the CA",
			cfg:    config.Config{Addr: "0.0.0.0:8443", AdvertisedURL: "https://seam.lan:8443", TLS: config.TLS{CertFile: cert, KeyFile: key, CAFile: cert}},
			report: true, status: statusOK, want: "this machine's client verifies https://seam.lan:8443",
		},
		{
			name:   "the derived loopback URL is the name verified",
			cfg:    config.Config{Addr: "0.0.0.0:8443", TLS: config.TLS{CertFile: cert, KeyFile: key, CAFile: cert}},
			report: true, status: statusOK, want: "verifies https://127.0.0.1:8443",
		},
		{
			name:   "a trusted certificate for another name",
			cfg:    config.Config{Addr: "0.0.0.0:8443", AdvertisedURL: "https://seam.lan:8443", TLS: config.TLS{CertFile: otherCert, KeyFile: otherKey, CAFile: otherCert}},
			report: true, status: statusWarn, want: "cannot verify https://seam.lan:8443",
			fix: "reissue " + otherCert + " for seam.lan, or correct server_url",
		},
		{
			name:   "a chain certificate that does not parse",
			cfg:    config.Config{Addr: "0.0.0.0:8443", AdvertisedURL: "https://seam.lan:8443", TLS: config.TLS{CertFile: badChain, KeyFile: key, CAFile: cert}},
			report: true, status: statusWarn, want: "cannot verify https://seam.lan:8443 (x509: ",
		},
		{
			name: "an unloadable pair is the tls row's failure",
			cfg:  config.Config{Addr: "0.0.0.0:8443", TLS: config.TLS{CertFile: "/nope/cert.pem", KeyFile: "/nope/key.pem"}},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			c, ok := tlsTrustCheck(tc.cfg)
			require.Equal(t, tc.report, ok, c.detail)
			if !tc.report {
				return
			}
			require.Equal(t, "tls trust", c.name)
			require.Equal(t, tc.status, c.status, c.detail)
			require.Contains(t, c.detail, tc.want)
			if tc.status == statusWarn {
				require.Contains(t, c.detail, "every update would fail its confirmation and then its rollback's")
			}
			if tc.fix != "" {
				require.Contains(t, c.detail, tc.fix)
			}
		})
	}
}

// TestTLSTrustCheck_AgreesWithALiveRequest is the measurement behind deciding
// the TLS check from the config: for a daemon serving the configured pair,
// the verdict is exactly what config.HTTPClient's own request to /healthz
// finds -- and with the daemon down the request finds nothing either way,
// while the config still answers.
func TestTLSTrustCheck_AgreesWithALiveRequest(t *testing.T) {
	cert, key := writeCertPair(t, time.Now().Add(365*24*time.Hour), "127.0.0.1")
	pair, err := tls.LoadX509KeyPair(cert, key)
	require.NoError(t, err)
	srv := httptest.NewUnstartedServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusOK)
	}))
	srv.TLS = &tls.Config{Certificates: []tls.Certificate{pair}, MinVersion: tls.VersionTLS12}
	srv.StartTLS()
	defer srv.Close()

	request := func(cfg config.Config) error {
		hc, err := cfg.HTTPClient(5 * time.Second)
		require.NoError(t, err)
		resp, err := hc.Get(cfg.ServerURL() + "/healthz")
		if err != nil {
			return err
		}
		return resp.Body.Close()
	}
	for _, tc := range []struct {
		name string
		ca   string
		want checkStatus
	}{
		{"no tls.ca_file: the system roots do not know this CA", "", statusWarn},
		{"tls.ca_file names the CA", cert, statusOK},
	} {
		t.Run(tc.name, func(t *testing.T) {
			cfg := config.Config{Addr: "0.0.0.0:8443", AdvertisedURL: srv.URL, TLS: config.TLS{CertFile: cert, KeyFile: key, CAFile: tc.ca}}
			c, ok := tlsTrustCheck(cfg)
			require.True(t, ok)
			require.Equal(t, tc.want, c.status, c.detail)
			err := request(cfg)
			require.Equal(t, tc.want == statusOK, err == nil, "the live request must agree with the config's verdict: %v", err)
		})
	}

	t.Run("the daemon down: no request answers, the config still does", func(t *testing.T) {
		cfg := config.Config{Addr: "0.0.0.0:8443", AdvertisedURL: srv.URL, TLS: config.TLS{CertFile: cert, KeyFile: key}}
		srv.Close()
		require.Error(t, request(cfg))
		c, ok := tlsTrustCheck(cfg)
		require.True(t, ok)
		require.Equal(t, statusWarn, c.status, c.detail)
	})
}
