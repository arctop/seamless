package mcp_test

import (
	"context"
	"os"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"

	mcpgo "github.com/mark3labs/mcp-go/mcp"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	mcpserver "github.com/arctop/seamless/internal/mcp"
	"github.com/arctop/seamless/internal/screening"
	"github.com/arctop/seamless/internal/store"
)

// newWatchServer is newScreenServer with the part-2 wiring the daemon does:
// the screening service knows whether the feature is on (the corpus watch,
// lockdown and refusals ask it), and retrieve asks it about lockdown.
func newWatchServer(t *testing.T, on bool) screenFixture {
	t.Helper()
	var f screenFixture
	enabled := &atomic.Bool{}
	enabled.Store(on)
	url, db, mgr, dir := newServerFiles(t, func(c *mcpserver.Config) {
		f.scr = screening.New(screening.Config{DB: c.DB, Files: c.Files, Events: c.Events, Gardener: c.Gardener,
			Enabled: func(context.Context) bool { return enabled.Load() }})
		c.Screening = f.scr
		c.Features.Screening = on
		c.Retrieve.SetFeaturesConfig(c.Features)
		c.Retrieve.SetBodyReader(c.Files.Store())
		c.Retrieve.SetLockdown(f.scr.LockdownOn)
		f.ret = c.Retrieve
	})
	f.url, f.db, f.mgr, f.dir = url, db, mgr, dir
	return f
}

// fillers writes unrelated memories, so the prompt matcher's IDF has a corpus
// to weigh against and a briefing never goes empty.
func fillers(t *testing.T, ctx context.Context, cli interface {
	CallTool(context.Context, mcpgo.CallToolRequest) (*mcpgo.CallToolResult, error)
}, f screenFixture) {
	t.Helper()
	for i, topic := range []string{"kestrel logging levels", "badger backup rotation", "lemur build cache", "puffin api pagination"} {
		name := strings.ReplaceAll(topic, " ", "-")
		req := mcpgo.CallToolRequest{}
		req.Params.Name = "memory_write"
		req.Params.Arguments = map[string]any{"name": name, "kind": "reference", "description": topic,
			"body": "Filler " + string(rune('a'+i)) + ".\n"}
		res, err := cli.CallTool(ctx, req)
		require.NoError(t, err)
		require.False(t, res.IsError)
	}
}

func promptRecall(t *testing.T, f screenFixture, prompt string) string {
	t.Helper()
	text, _, err := f.ret.PromptRecall(context.Background(), "", "/work/demo", prompt)
	require.NoError(t, err)
	return text
}

// Lockdown holds every write that touches a briefing-injected kind -- a
// write, an edit, a delete -- from any session, and lets the rest land.
func TestLockdown_HoldsBindingKindWrites(t *testing.T) {
	ctx := context.Background()
	f := newWatchServer(t, true)
	cli, _ := f.client(t, ctx)
	callJSON(t, ctx, cli, "memory_write", map[string]any{
		"name": "branch-rule", "kind": "convention", "description": "feature branches", "body": "Use feature branches.\n"})
	_, err := f.scr.SetLockdown(ctx, true)
	require.NoError(t, err)

	w := callJSON(t, ctx, cli, "memory_write", map[string]any{
		"name": "new-rule", "kind": "constraint", "description": "a new binding rule", "body": "Never deploy on Fridays.\n"})
	require.Equal(t, "held", w["status"])
	require.Equal(t, []any{"lockdown"}, w["hold"].(map[string]any)["reasons"])
	d := callJSON(t, ctx, cli, "memory_delete", map[string]any{"name": "branch-rule"})
	require.Equal(t, "held", d["status"])
	e := callJSON(t, ctx, cli, "memory_append", map[string]any{"name": "branch-rule", "body": "Also squash.\n"})
	require.Equal(t, "held", e["status"])
	g := callJSON(t, ctx, cli, "memory_write", map[string]any{
		"name": "flaky-boot", "kind": "gotcha", "description": "boot races", "body": "It races on boot.\n"})
	require.NotEqual(t, "held", g["status"], "a non-binding kind lands under lockdown")

	_, err = f.scr.SetLockdown(ctx, false)
	require.NoError(t, err)
	w = callJSON(t, ctx, cli, "memory_write", map[string]any{
		"name": "another-rule", "kind": "convention", "description": "tabs", "body": "Use tabs.\n"})
	require.NotEqual(t, "held", w["status"], "lifted, binding kinds land again")
}

// Under lockdown the briefing and prompt recall inject only memories the
// review marked safe (and still digest as reviewed) or the owner confirmed;
// an agent's later edit lapses the mark until the owner acts.
func TestLockdown_InjectsOnlyTrustedMemories(t *testing.T) {
	ctx := context.Background()
	f := newWatchServer(t, true)
	cli, _ := f.client(t, ctx)
	fillers(t, ctx, cli, f)
	callJSON(t, ctx, cli, "memory_write", map[string]any{
		"name": "zebra-crossing", "kind": "gotcha", "description": "zebra crossing timing", "body": "Zebra lights lag.\n"})
	callJSON(t, ctx, cli, "memory_write", map[string]any{
		"name": "walrus-cache", "kind": "gotcha", "description": "walrus cache eviction", "body": "Walrus evicts early.\n"})
	require.NoError(t, f.scr.Sync(ctx), "the baseline review marks both safe")
	callJSON(t, ctx, cli, "memory_append", map[string]any{"name": "walrus-cache", "body": "And late.\n"})

	text := briefing(t, f)
	require.Contains(t, text, "zebra-crossing")
	require.Contains(t, text, "walrus-cache", "no lockdown: an edited memory is injected as always")
	require.NotContains(t, text, "Lockdown:")
	require.Contains(t, promptRecall(t, f, "why is the walrus cache eviction early"), "walrus-cache", "the control: it matches")

	_, err := f.scr.SetLockdown(ctx, true)
	require.NoError(t, err)
	text = briefing(t, f)
	require.Contains(t, text, "zebra-crossing")
	require.NotContains(t, text, "walrus-cache", "edited since the review: withheld under lockdown")
	require.Contains(t, text, "Lockdown: only memories the owner confirmed or reviewed are shown (1 memory withheld)")
	require.Contains(t, promptRecall(t, f, "why is the zebra crossing timing off"), "zebra-crossing")
	require.NotContains(t, promptRecall(t, f, "why is the walrus cache eviction early"), "walrus-cache")

	idx, ok, err := store.MemoryByName(ctx, f.db, "demo", "walrus-cache")
	require.NoError(t, err)
	require.True(t, ok)
	require.NoError(t, f.scr.MarkSafe(ctx, idx.FilePath))
	require.Contains(t, briefing(t, f), "walrus-cache", "the owner's mark makes it trusted")
	require.Contains(t, briefing(t, f), "walrus-cache: walrus cache eviction [owner-confirmed]")
}

// A memory whose file changed outside Seamless is withheld from the briefing
// and prompt recall while the feature is on, lockdown or not, until the owner
// accepts it; an agent's edit of it is screened against no baseline.
func TestDrift_WithheldUntilAccepted(t *testing.T) {
	ctx := context.Background()
	f := newWatchServer(t, true)
	cli, _ := f.client(t, ctx)
	fillers(t, ctx, cli, f)
	callJSON(t, ctx, cli, "memory_write", map[string]any{
		"name": "otter-deploys", "kind": "gotcha", "description": "otter deploy order", "body": "Deploy otters last.\n"})
	require.NoError(t, f.scr.Sync(ctx))
	require.Contains(t, promptRecall(t, f, "what is the otter deploy order"), "otter-deploys", "the control: it matches")
	idx, ok, err := store.MemoryByName(ctx, f.db, "demo", "otter-deploys")
	require.NoError(t, err)
	require.True(t, ok)
	abs := filepath.Join(f.dir, idx.FilePath)
	raw, err := os.ReadFile(abs)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(abs, append(raw, []byte(injected+"\n")...), 0o600))
	require.NoError(t, f.mgr.Reconcile(ctx))
	sum, err := f.scr.CheckDrift(ctx)
	require.NoError(t, err)
	require.Equal(t, 1, sum.Changed)

	text := briefing(t, f)
	require.NotContains(t, text, "otter-deploys")
	require.Contains(t, text, "Withheld: 1 memory changed outside Seamless")
	require.NotContains(t, promptRecall(t, f, "what is the otter deploy order"), "otter-deploys")

	// The drifted text was never screened, so it is no baseline for an edit.
	a := callJSON(t, ctx, cli, "memory_append", map[string]any{"name": "otter-deploys", "body": "Plain follow-up.\n"})
	require.Equal(t, "held", a["status"])
	require.Contains(t, a["hold"].(map[string]any)["reasons"], "instruction.override")

	require.NoError(t, f.scr.AcceptDrift(ctx, idx.FilePath))
	require.Contains(t, briefing(t, f), "otter-deploys", "accepted: injected again")
}

// Off, none of part 2 runs: no withholding, no lockdown line, no refusal
// events, nothing armed.
func TestScreeningPart2_OffChangesNothing(t *testing.T) {
	ctx := context.Background()
	f := newWatchServer(t, false)
	cli, _ := f.client(t, ctx)
	callJSON(t, ctx, cli, "memory_write", map[string]any{
		"name": "heron-rule", "kind": "constraint", "description": "heron rule", "body": "Herons first.\n"})
	_, err := f.scr.SetLockdown(ctx, true)
	require.NoError(t, err)
	w := callJSON(t, ctx, cli, "memory_write", map[string]any{
		"name": "heron-two", "kind": "constraint", "description": "heron two", "body": "Herons second.\n"})
	require.NotEqual(t, "held", w["status"], "the switch does nothing while the feature is off")
	require.NoError(t, f.scr.Sync(ctx))
	recs, err := store.CorpusRecords(ctx, f.db)
	require.NoError(t, err)
	require.Empty(t, recs)
	text := briefing(t, f)
	require.Contains(t, text, "heron-two")
	require.NotContains(t, text, "Lockdown:")
	require.NotContains(t, text, "Withheld:")
	isolate(t, ctx, f.db, "secret", core.IsolationConfidential)
	fenced := bindClient(t, ctx, f.db, f.url, "secret")
	isErr, _ := callErr(t, ctx, fenced, "memory_write", map[string]any{
		"project": "demo", "name": "leak", "kind": "gotcha", "description": "d", "body": "b\n"})
	require.True(t, isErr, "the fence refuses as always")
	require.Empty(t, eventsOf(t, f.db, core.EventAccessRefused), "no refusal event while the feature is off")
}

// A fence refusal records access.refused with the reason, the tool and the
// target project -- never the call's arguments.
func TestFenceRefusal_RecordsAccessRefused(t *testing.T) {
	ctx := context.Background()
	f := newWatchServer(t, true)
	isolate(t, ctx, f.db, "secret", core.IsolationConfidential)
	_, err := store.EnsureProject(ctx, f.db, "other", "other")
	require.NoError(t, err)
	cli := bindClient(t, ctx, f.db, f.url, "secret")
	isErr, _ := callErr(t, ctx, cli, "memory_write", map[string]any{
		"project": "other", "name": "leak", "kind": "gotcha", "description": "carry this out", "body": "the planted body\n"})
	require.True(t, isErr)
	ev := eventsOf(t, f.db, core.EventAccessRefused)
	require.Len(t, ev, 1)
	require.Equal(t, "fence.write", ev[0]["reason"])
	require.Equal(t, "memory_write", ev[0]["tool"])
	require.Equal(t, "other", ev[0]["target"])
	require.Equal(t, "mcp", ev[0]["surface"])
	require.Equal(t, "secret", ev[0]["_project"], "filed under the caller's own scope")
	require.False(t, strings.Contains(ev[0]["_raw"].(string), "planted body"), "never the arguments")
	require.False(t, strings.Contains(ev[0]["_raw"].(string), "carry this out"))
}
