package retrieve

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// fakeBodies serves full memories by path, standing in for the files store.
type fakeBodies map[string]core.Memory

func (f fakeBodies) ReadMemory(relPath string) (core.Memory, error) { return f[relPath], nil }

func provenanceFixture(t *testing.T) (*Service, context.Context) {
	t.Helper()
	db := setupDB(t)
	ctx := context.Background()
	require.NoError(t, store.SetSetting(ctx, db, store.SettingRepoProjectMap, `{"/work/app":"app"}`))
	insMem(t, db, "01RULE", "constraint", "no-force-push", "never force push to main", "app")
	insMem(t, db, "01CONV", "convention", "tabs", "indent with tabs", "app")
	insMem(t, db, "01GOT", "gotcha", "cache-race", "the cache test races the boot", "app")
	now := time.Now().UTC()
	require.NoError(t, store.CreateSession(ctx, db, core.Session{ID: "01SESS", Name: "cc/abc12345",
		ProjectSlug: "app", Status: core.SessionActive, CreatedAt: now, UpdatedAt: now}))
	_, err := db.ExecContext(ctx, `INSERT INTO events (id, ts, kind, session_id, project_slug, item_id, payload)
		VALUES ('01EV', ?, 'memory.written', '01SESS', 'app', '01GOT', '{}')`, core.FormatTime(now))
	require.NoError(t, err)

	// The owner confirmed the constraint as it reads now; the convention was
	// confirmed with content it no longer has.
	rule := core.Memory{ID: "01RULE", Kind: core.KindConstraint, Project: "app", Name: "no-force-push",
		Description: "never force push to main", Body: "Rebase instead.\n", FilePath: "memory/x/no-force-push.md"}
	conv := core.Memory{ID: "01CONV", Kind: core.KindConvention, Project: "app", Name: "tabs",
		Description: "indent with tabs", Body: "Tabs.\n", FilePath: "memory/x/tabs.md"}
	require.NoError(t, store.UpsertOwnerConfirmation(ctx, db, rule.ID, core.MemoryDigest(rule), store.ConfirmedViaRelease, now))
	stale := conv
	stale.Body = "Spaces.\n"
	require.NoError(t, store.UpsertOwnerConfirmation(ctx, db, conv.ID, core.MemoryDigest(stale), store.ConfirmedViaRelease, now))

	svc := New(db, nil, budgets(), nil)
	svc.SetBodyReader(fakeBodies{rule.FilePath: rule, conv.FilePath: conv})
	return svc, ctx
}

// Off, every briefing and recall line is exactly what it was: no marks, and
// the output is byte-identical to a service that never heard of provenance.
func TestProvenance_OffRendersNoMarks(t *testing.T) {
	svc, ctx := provenanceFixture(t)
	b, _, err := svc.Briefing(ctx, BriefingInput{CWD: "/work/app", Source: "startup"})
	require.NoError(t, err)
	require.NotContains(t, b, "owner-confirmed")
	require.NotContains(t, b, "agent-written")
	require.Contains(t, b, "- no-force-push: never force push to main\n")

	r, _, err := svc.PromptRecall(ctx, "", "/work/app", "why does the cache test race the boot")
	require.NoError(t, err)
	require.NotContains(t, r, "agent-written")
}

// On, each injected memory says who stands behind it: the owner (only while
// the content still matches what was confirmed), or an agent and its session.
func TestProvenance_OnMarksEveryInjectedMemory(t *testing.T) {
	svc, ctx := provenanceFixture(t)
	svc.SetFeaturesConfig(config.Features{Screening: true})

	b, _, err := svc.Briefing(ctx, BriefingInput{CWD: "/work/app", Source: "startup"})
	require.NoError(t, err)
	require.Contains(t, b, "- no-force-push: never force push to main [owner-confirmed]\n")
	require.Contains(t, b, "- tabs: indent with tabs [agent-written]\n",
		"a confirmation of content the memory no longer has has lapsed")
	require.Contains(t, b, "- cache-race: the cache test races the boot [agent-written, session cc/abc12345]\n")

	sub, _, err := svc.Briefing(ctx, BriefingInput{CWD: "/work/app", Source: "startup", AgentType: "general"})
	require.NoError(t, err)
	require.Contains(t, sub, "no-force-push: never force push to main [owner-confirmed]")

	r, _, err := svc.PromptRecall(ctx, "", "/work/app", "why does the cache test race the boot")
	require.NoError(t, err)
	require.Contains(t, r, "[agent-written, session cc/abc12345]")

	p := svc.MemoryProvenance(ctx, []core.Memory{{ID: "01GOT", Project: "app"}, {ID: "01UNKNOWN"}})
	require.Equal(t, Provenance{Session: "cc/abc12345"}, p["01GOT"])
	require.Equal(t, "agent-written", p["01UNKNOWN"].Label())

	// A session is named only inside the memory's own scope: the same write,
	// seen as a global memory, names no project's session to the reader.
	global := svc.MemoryProvenance(ctx, []core.Memory{{ID: "01GOT", Project: ""}})
	require.Equal(t, Provenance{}, global["01GOT"])
}
