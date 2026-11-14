package retrieve

import (
	"context"
	"fmt"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// A conversation's home and its grants' targets (isolation.requests): the
// hooks resolve them (conversationScope) and the matcher reads exactly that
// scope -- the home's memories, the global scope's, and each target's -- in a
// corpus of its own, while a cwd's recall reads its project's, as before.

const homeDeskPrompt = "who owns the intake queue triage rotation"

// homeRetrieveDB plants a matching memory in example-desk, and filler in
// example-legal (the home) and example-ops (a cwd's project), each fenced as
// in the hooks' tests.
func homeRetrieveDB(t *testing.T) *Service {
	t.Helper()
	db := setupDB(t)
	ctx := context.Background()
	require.NoError(t, store.SetSetting(ctx, db, store.SettingRepoProjectMap, `{"/work/ops":"example-ops"}`))
	for slug, iso := range map[string]core.Isolation{
		"example-ops": core.IsolationOpen, "example-legal": core.IsolationConfidential, "example-desk": core.IsolationConfidential,
	} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
		require.NoError(t, store.SetProjectIsolation(ctx, db, slug, iso))
	}
	insMem(t, db, "01HDSK1", "gotcha", "desk-intake-queue", "intake queue triage rotation schedule", "example-desk")
	insMem(t, db, "01HLEG1", "constraint", "legal-privilege-rule", "privileged memos stay with counsel", "example-legal")
	insMem(t, db, "01HLEG2", "reference", "legal-nda-template", "mutual nda template version three", "example-legal")
	insMem(t, db, "01HLEG3", "decision", "legal-filing-calendar", "court filing calendar deadlines", "example-legal")
	insMem(t, db, "01HOPS1", "gotcha", "ops-intake-queue", "intake queue triage rotation for paging", "example-ops")
	insMem(t, db, "01HOPS2", "reference", "ops-cache-ttl", "edge cache entries live five minutes", "example-ops")
	insMem(t, db, "01HOPS3", "decision", "ops-escalation", "escalate primary then secondary", "example-ops")
	insMem(t, db, "01HOPS4", "constraint", "ops-rollback-rule", "roll back a release within one hour", "example-ops")
	return New(db, nil, budgets(), nil)
}

// ProjectPromptRecall reads the home and, only when named, the targets beside
// it; each scope is its own corpus, keyed so a cwd's recall keeps the key it
// always had; and the cwd's recall never reads a target.
func TestProjectPromptRecall_TheHomeAndItsGrantTargets(t *testing.T) {
	ctx := context.Background()
	svc := homeRetrieveDB(t)

	out, ids, err := svc.ProjectPromptRecall(ctx, "example-legal", nil, homeDeskPrompt)
	require.NoError(t, err)
	require.Empty(t, out, "the home alone holds no match")
	require.Empty(t, ids)

	out, ids, err = svc.ProjectPromptRecall(ctx, "example-legal", []string{"example-desk", "", "example-legal", "example-desk"}, homeDeskPrompt)
	require.NoError(t, err)
	require.Contains(t, out, "desk-intake-queue")
	require.NotContains(t, out, "ops-intake-queue")
	require.Equal(t, []string{"01HDSK1"}, ids)

	out, _, err = svc.PromptRecall(ctx, "", "/work/ops", homeDeskPrompt)
	require.NoError(t, err)
	require.Contains(t, out, "ops-intake-queue")
	require.NotContains(t, out, "desk-intake-queue", "a cwd's recall reads no grant's target")

	svc.corpus.mu.Lock()
	defer svc.corpus.mu.Unlock()
	for _, key := range []string{"example-legal", "example-legal\x00example-desk", "example-ops"} {
		_, ok := svc.corpus.entries[key]
		require.True(t, ok, "%q", key)
	}
	require.Len(t, svc.corpus.entries, 3)
}

// A home the caller may not read -- locked to the owner under peer_auth --
// recalls nothing, as a cwd in a locked project does.
func TestProjectPromptRecall_AHomeOutOfReachRecallsNothing(t *testing.T) {
	svc := homeRetrieveDB(t)
	policy, err := access.New(config.PeerAuth{Enabled: true, Owner: "owner", ForceLocked: []string{"example-legal"}},
		func(n string) (access.Account, error) {
			if n == "owner" {
				return access.Account{UID: 501, Name: "owner"}, nil
			}
			return access.Account{}, fmt.Errorf("no account %s", n)
		})
	require.NoError(t, err)
	ctx := access.WithView(context.Background(), policy.OwnerView())
	out, ids, err := svc.ProjectPromptRecall(ctx, "example-legal", []string{"example-desk"}, homeDeskPrompt)
	require.NoError(t, err)
	require.Empty(t, out)
	require.Empty(t, ids)
}

// A subagent's RELEVANT section reads BriefingInput.Also beside its project
// -- the parent conversation's grants' targets -- while its constraints stay
// the project's own; without Also it is the project's scope, as before.
func TestBriefing_SubagentRelevantReadsAlso(t *testing.T) {
	ctx := context.Background()
	svc := homeRetrieveDB(t)
	in := BriefingInput{AgentType: "Explore", Prompt: homeDeskPrompt}

	text, _, err := svc.ProjectBriefing(ctx, "example-legal", in)
	require.NoError(t, err)
	require.Contains(t, text, "legal-privilege-rule")
	require.NotContains(t, text, "desk-intake-queue")

	in.Also = []string{"example-desk"}
	text, ids, err := svc.ProjectBriefing(ctx, "example-legal", in)
	require.NoError(t, err)
	require.Contains(t, text, "legal-privilege-rule")
	require.Contains(t, text, "desk-intake-queue")
	require.Contains(t, ids, "01HDSK1", "the matched target memory counts as injected")
}

// A main-session briefing never reads Also: it is the project's own.
func TestBriefing_MainSessionIgnoresAlso(t *testing.T) {
	ctx := context.Background()
	svc := homeRetrieveDB(t)
	plain, _, err := svc.ProjectBriefing(ctx, "example-legal", BriefingInput{Source: "startup"})
	require.NoError(t, err)
	widened, _, err := svc.ProjectBriefing(ctx, "example-legal",
		BriefingInput{Source: "startup", Prompt: homeDeskPrompt, Also: []string{"example-desk"}})
	require.NoError(t, err)
	require.Equal(t, plain, widened)
}
