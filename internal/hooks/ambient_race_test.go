package hooks

import (
	"context"
	"database/sql"
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// openRaceDB opens a fresh migrated database for the ambient-race tests, without
// the seeded memories/handlers of newHandlerServer (these tests call the handler
// methods directly).
func openRaceDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return db
}

// TestEnsureAmbientSession_ResumeDoesNotClobberHarvest is the regression test
// for the resume/harvest write race: ensureAmbientSession used to read the full
// session row and write every mutable field back, so a resume racing a
// transcript harvest could overwrite the harvest's findings with the stale value
// it had read. The resume path is now a targeted UPDATE that never touches
// findings, so the harvester's last write must always survive. Run with -race.
func TestEnsureAmbientSession_ResumeDoesNotClobberHarvest(t *testing.T) {
	db := openRaceDB(t)
	h := NewHandler(Config{DB: db, APIKey: testKey})
	ctx := context.Background()

	payload := hookPayload{SessionID: "racecafe-0001", Source: "startup"}
	name := ambientName(ClientClaudeCode, payload.SessionID)
	require.Equal(t, name, h.ensureAmbientSession(ctx, ClientClaudeCode, "", payload))
	sess, ok, err := store.AmbientSessionByExternalIdentity(
		ctx, db, ClientClaudeCode.externalIdentity(), payload.SessionID)
	require.NoError(t, err)
	require.True(t, ok)

	const iters = 200
	var wg sync.WaitGroup
	wg.Add(2)
	// Harvester: sequential full-row findings writes, exactly what the
	// SessionEnd hook's completeClaudeSessions does.
	go func() {
		defer wg.Done()
		for i := range iters {
			s := sess
			s.Findings = fmt.Sprintf("harvest-%d", i)
			s.UpdatedAt = time.Now().UTC()
			if err := store.UpdateSession(ctx, db, s); err != nil {
				t.Errorf("harvest write %d: %v", i, err)
				return
			}
		}
	}()
	// Resumer: concurrent SessionStart hooks resuming the same ambient session.
	go func() {
		defer wg.Done()
		for i := range iters {
			if got := h.ensureAmbientSession(ctx, ClientClaudeCode, "", payload); got != name {
				t.Errorf("resume %d: got %q, want %s", i, got, name)
				return
			}
		}
	}()
	wg.Wait()

	final, ok, err := store.AmbientSessionByExternalIdentity(
		ctx, db, ClientClaudeCode.externalIdentity(), payload.SessionID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, fmt.Sprintf("harvest-%d", iters-1), final.Findings,
		"a racing resume must never clobber the harvest's findings")
}

// TestEnsureAmbientSession_ConcurrentCreateSingleRow stresses the create race:
// several SessionStart hooks for the same Claude session can all miss the resume
// and collide on the UNIQUE session name. Every caller must still get the
// ambient name back (the losers resume the winner's row) and exactly one session
// row may exist. Run with -race.
func TestEnsureAmbientSession_ConcurrentCreateSingleRow(t *testing.T) {
	db := openRaceDB(t)
	h := NewHandler(Config{DB: db, APIKey: testKey})
	ctx := context.Background()

	const n = 8
	payload := hookPayload{SessionID: "fresh000-0001", Source: "startup"}
	name := ambientName(ClientClaudeCode, payload.SessionID)
	results := make([]string, n)
	var wg sync.WaitGroup
	for i := range n {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			results[i] = h.ensureAmbientSession(ctx, ClientClaudeCode, "", payload)
		}(i)
	}
	wg.Wait()

	for i, got := range results {
		require.Equal(t, name, got, "goroutine %d must resolve the ambient session", i)
	}
	sessions, err := store.ListSessions(ctx, db, "", time.Time{}, 0)
	require.NoError(t, err)
	require.Len(t, sessions, 1, "racing creates must not fork a second ambient row")
}

func TestCodexAmbientIdentity_SameUUIDv7PrefixStaysIsolated(t *testing.T) {
	db := openRaceDB(t)
	ctx := context.Background()
	require.NoError(t, store.SetSetting(ctx, db, store.SettingRepoProjectMap,
		`{"/work/alpha":"alpha","/work/beta":"beta"}`))
	recorder := events.NewRecorder(db)
	h := NewHandler(Config{DB: db, Events: recorder, APIKey: testKey})

	// UUIDv7's first eight hex digits are timestamp material, so these two ids
	// model real Codex sessions created in the same collision window.
	const (
		idA = "019f7291-40f1-7311-8997-0d497579d27b"
		idB = "019f7291-b0b2-7d0a-8a84-7526b23b72f1"
	)
	nameA := h.ensureAmbientSession(ctx, ClientCodex, "", hookPayload{
		SessionID: idA, CWD: "/work/alpha", Source: "startup",
	})
	nameB := h.ensureAmbientSession(ctx, ClientCodex, "", hookPayload{
		SessionID: idB, CWD: "/work/beta", Source: "startup",
	})
	require.NotEqual(t, nameA, nameB)
	require.Equal(t, "cx/019f7291-", nameA[:len("cx/019f7291-")])
	require.Equal(t, "cx/019f7291-", nameB[:len("cx/019f7291-")])

	h.setAmbientModel(ctx, ClientCodex, "", idA, "gpt-alpha", "")
	h.setAmbientModel(ctx, ClientCodex, "", idB, "gpt-beta", "")
	a, ok, err := store.AmbientSessionByExternalIdentity(ctx, db, "codex", idA)
	require.NoError(t, err)
	require.True(t, ok)
	b, ok, err := store.AmbientSessionByExternalIdentity(ctx, db, "codex", idB)
	require.NoError(t, err)
	require.True(t, ok)
	require.NotEqual(t, a.ID, b.ID)
	require.Equal(t, "alpha", a.ProjectSlug)
	require.Equal(t, "beta", b.ProjectSlug)
	require.Equal(t, "gpt-alpha", a.Model)
	require.Equal(t, "gpt-beta", b.Model)

	// Make both rows stale, then land activity/findings/model on A only. B must
	// retain every old value and be the only row eligible for the reaper.
	stale := time.Now().UTC().Add(-2 * time.Hour)
	a.UpdatedAt = stale
	b.UpdatedAt = stale
	require.NoError(t, store.UpdateSession(ctx, db, a))
	require.NoError(t, store.UpdateSession(ctx, db, b))
	h.touchAmbient(ctx, ClientCodex, idA)
	h.setAmbientModel(ctx, ClientCodex, "", idA, "gpt-alpha-switched", "")
	h.harvestCodexStop(ctx, "", stopPayload{
		SessionID: idA, LastAssistantMessage: "alpha turn complete",
	})

	a, _, err = store.SessionByID(ctx, db, a.ID)
	require.NoError(t, err)
	b, _, err = store.SessionByID(ctx, db, b.ID)
	require.NoError(t, err)
	require.Equal(t, "gpt-alpha-switched", a.Model)
	require.Equal(t, "(auto-harvested) alpha turn complete", a.Findings)
	require.True(t, a.UpdatedAt.After(stale))
	require.Equal(t, "gpt-beta", b.Model)
	require.Empty(t, b.Findings)
	require.WithinDuration(t, stale, b.UpdatedAt, time.Second)

	// Event provenance resolves through the same full identity, so identical
	// display prefixes still stamp distinct Seamless ULIDs and projects.
	h.recordHookPrompt(ctx, ClientCodex, idA, "alpha prompt")
	h.recordHookPrompt(ctx, ClientCodex, idB, "beta prompt")
	prompts, err := recorder.ByKinds(ctx, []core.EventKind{core.EventHookPrompt}, "", "", 10)
	require.NoError(t, err)
	require.Len(t, prompts, 2)
	byExternalID := make(map[string]core.Event, len(prompts))
	for _, event := range prompts {
		externalID, _ := event.Payload["claude_session_id"].(string)
		byExternalID[externalID] = event
	}
	require.Equal(t, a.ID, byExternalID[idA].SessionID)
	require.Equal(t, "alpha", byExternalID[idA].ProjectSlug)
	require.Equal(t, b.ID, byExternalID[idB].SessionID)
	require.Equal(t, "beta", byExternalID[idB].ProjectSlug)
	require.Equal(t, nameA, h.ambientDisplayName(ctx, ClientCodex, idA))
	require.Equal(t, nameB, h.ambientDisplayName(ctx, ClientCodex, idB))

	reaped, err := store.ExpireStaleSessions(ctx, db, time.Now().UTC().Add(-time.Hour))
	require.NoError(t, err)
	require.Len(t, reaped, 1)
	require.Equal(t, b.ID, reaped[0].ID)
	a, _, err = store.SessionByID(ctx, db, a.ID)
	require.NoError(t, err)
	b, _, err = store.SessionByID(ctx, db, b.ID)
	require.NoError(t, err)
	require.Equal(t, core.SessionActive, a.Status)
	require.Equal(t, core.SessionExpired, b.Status)
}

func TestEnsureAmbientSession_ResumeSourcesAndLegacyNameStayOneRow(t *testing.T) {
	db := openRaceDB(t)
	ctx := context.Background()
	require.NoError(t, store.SetSetting(ctx, db, store.SettingRepoProjectMap,
		`{"/work/alpha":"alpha","/work/beta":"beta"}`))
	h := NewHandler(Config{DB: db, APIKey: testKey})

	const externalID = "019f7291-40f1-7311-8997-0d497579d27b"
	startupName := h.ensureAmbientSession(ctx, ClientCodex, "", hookPayload{
		SessionID: externalID, CWD: "/work/alpha", Source: "startup",
	})
	startup, ok, err := store.AmbientSessionByExternalIdentity(ctx, db, "codex", externalID)
	require.NoError(t, err)
	require.True(t, ok)

	for _, source := range []string{"resume", "compact"} {
		got := h.ensureAmbientSession(ctx, ClientCodex, "", hookPayload{
			SessionID: externalID, CWD: "/work/beta", Source: source,
		})
		require.Equal(t, startupName, got)
	}
	resumed, ok, err := store.AmbientSessionByExternalIdentity(ctx, db, "codex", externalID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, startup.ID, resumed.ID)
	require.Equal(t, "beta", resumed.ProjectSlug)
	all, err := store.ListSessions(ctx, db, "", time.Time{}, 0)
	require.NoError(t, err)
	require.Len(t, all, 1)

	const legacyID = "019f7291-legacy-full-id"
	now := time.Now().UTC().Add(-time.Hour)
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: "01LEGACYCODEX000000000000", Name: "cx/019f7291", ProjectSlug: "alpha",
		Status: core.SessionExpired, ExternalSessionID: legacyID, ExternalClient: "codex",
		Ambient: true, CreatedAt: now, UpdatedAt: now,
	}))
	legacyName := h.ensureAmbientSession(ctx, ClientCodex, "", hookPayload{
		SessionID: legacyID, CWD: "/work/beta", Source: "resume",
	})
	require.Equal(t, "cx/019f7291", legacyName, "a migrated legacy row keeps its display name")
	legacy, ok, err := store.AmbientSessionByExternalIdentity(ctx, db, "codex", legacyID)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, core.SessionActive, legacy.Status)
	require.Equal(t, "beta", legacy.ProjectSlug)
	all, err = store.ListSessions(ctx, db, "", time.Time{}, 0)
	require.NoError(t, err)
	require.Len(t, all, 2, "legacy resume must not fork a digest-named row")
}

func TestAmbientIdentity_SameExternalIDAcrossClientsCannotCrossMatch(t *testing.T) {
	db := openRaceDB(t)
	ctx := context.Background()
	h := NewHandler(Config{DB: db, APIKey: testKey})
	const externalID = "same-id-from-two-clients"

	claudeName := h.ensureAmbientSession(ctx, ClientClaudeCode, "", hookPayload{
		SessionID: externalID, Source: "startup",
	})
	codexName := h.ensureAmbientSession(ctx, ClientCodex, "", hookPayload{
		SessionID: externalID, Source: "startup",
	})
	require.NotEqual(t, claudeName, codexName)
	claude, ok, err := store.AmbientSessionByExternalIdentity(ctx, db, "claude-code", externalID)
	require.NoError(t, err)
	require.True(t, ok)
	codex, ok, err := store.AmbientSessionByExternalIdentity(ctx, db, "codex", externalID)
	require.NoError(t, err)
	require.True(t, ok)
	require.NotEqual(t, claude.ID, codex.ID)

	h.harvestCodexStop(ctx, "", stopPayload{
		SessionID: externalID, LastAssistantMessage: "codex only",
	})
	h.completeClaudeSessions(ctx, ClientClaudeCode, endPayload{SessionID: externalID})
	claude, _, err = store.SessionByID(ctx, db, claude.ID)
	require.NoError(t, err)
	codex, _, err = store.SessionByID(ctx, db, codex.ID)
	require.NoError(t, err)
	require.Equal(t, core.SessionCompleted, claude.Status)
	require.Equal(t, core.FindingNoSummary, claude.Findings,
		"Claude completion must not receive Codex Stop findings")
	require.Equal(t, core.SessionActive, codex.Status)
	require.Equal(t, "(auto-harvested) codex only", codex.Findings)
}
