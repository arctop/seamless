package main

import (
	"context"
	"database/sql"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"

	// Test-only, as in plan_test.go and access_test.go: the binary must not
	// import these (they pull SQLite in), but the test binary can, which is
	// what pins the transcribed reason and refusal to the daemon's own and runs
	// the commands against the real console routes.
	"github.com/arctop/seamless/internal/console"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/screening"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

const (
	proposalID  = "01KPROPOSAL000000000000001"
	staleID     = "01KSTALEPROPOSAL0000000001"
	screenedID  = "01KSCREENED000000000000001"
	promotedID  = "01KPROMOTED000000000000001"
	heldAccount = "_example_agent"
)

// fxHeld is the console's held queue: a proposal (with a field this CLI does
// not know, which --json must keep), a stale proposal in another project, a
// write screening held, and a proposal already promoted.
var fxHeld = `{"pending":[` +
	`{"id":"` + proposalID + `","createdAt":"0001-01-01T00:00:00Z","status":"held","op":"write","tool":"memory_write",` +
	`"itemKind":"memory","project":"example-project","name":"run-the-linter","memoryKind":"constraint",` +
	`"reasons":[{"code":"account.restricted","words":"a restricted account proposed a binding rule; it waits for your promotion"}],` +
	`"what":"constraint memory","preview":"Run the linter first.\n","proposal":true,"account":"` + heldAccount + `","extra":"kept"},` +
	`{"id":"` + staleID + `","createdAt":"0001-01-01T00:00:00Z","status":"held","op":"write","tool":"memory_write",` +
	`"itemKind":"memory","project":"other-project","name":"pin-the-toolchain","memoryKind":"convention",` +
	`"reasons":[{"code":"account.restricted","words":"w"}],"what":"convention memory","stale":true,"proposal":true,"account":"` + heldAccount + `"},` +
	`{"id":"` + screenedID + `","createdAt":"0001-01-01T00:00:00Z","status":"held","op":"write","tool":"memory_write",` +
	`"itemKind":"memory","project":"example-project","name":"ignore-everything","memoryKind":"constraint",` +
	`"reasons":[{"code":"instruction.override","words":"w"}],"what":"constraint memory"}` +
	`],"recent":[` +
	`{"id":"` + promotedID + `","createdAt":"0001-01-01T00:00:00Z","status":"released","itemKind":"memory","project":"example-project",` +
	`"name":"old-rule","reasons":[{"code":"account.restricted","words":"w"}],"what":"constraint memory","proposal":true,"account":"` + heldAccount + `"}` +
	`],"tainted":[],"lockdown":{"on":false},"review":{"armed":false},"flagged":[],"drifted":[]}`

// heldConsole answers the queue with fxHeld and every action with the
// console's own JSON shapes.
func heldConsole(t *testing.T) *accessConsole {
	t.Helper()
	return newAccessConsole(t, func(w http.ResponseWriter, r *http.Request) {
		switch {
		case r.Method == http.MethodGet && r.URL.Path == "/console/held":
			_, _ = w.Write([]byte(fxHeld + "\n"))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/release"):
			_, _ = w.Write([]byte(`{"id":"` + proposalID + `","status":"released","item":"01KMEMORY"}` + "\n"))
		case r.Method == http.MethodPost && strings.HasSuffix(r.URL.Path, "/discard"):
			_, _ = w.Write([]byte(`{"id":"` + proposalID + `","status":"discarded"}` + "\n"))
		default:
			w.WriteHeader(http.StatusNotFound)
		}
	})
}

// withOwnerTerminal is the env with the owner's terminal and the given answers typed.
func withOwnerTerminal(e *env, answers string) *env {
	e.terminal = func() bool { return true }
	e.stdin = strings.NewReader(answers)
	return e
}

func postedURIs(reqs []seenRequest) []string {
	var out []string
	for _, r := range reqs {
		if r.method == http.MethodPost {
			out = append(out, r.uri)
		}
	}
	return out
}

func TestProposalsParse(t *testing.T) {
	for _, tc := range []struct {
		argv []string
		want string
	}{
		{[]string{"proposals", proposalID}, "to decide one: seam promote <hold>, or seam proposals discard <hold>"},
		{[]string{"promote"}, "missing argument: seam promote requires <hold>"},
		{[]string{"promote", "a", "b"}, "takes at most 1 positional argument"},
		{[]string{"proposals", "discard"}, "missing argument: seam proposals discard requires <hold>"},
	} {
		_, err := parse(commands(), tc.argv)
		require.Error(t, err, "%v", tc.argv)
		require.Contains(t, err.Error(), tc.want)
	}
	p, err := parse(commands(), []string{"proposals", "--json", "--project", "example-project"})
	require.NoError(t, err)
	require.Equal(t, "proposals", p.cmd.name)
	p, err = parse(commands(), []string{"proposals", "discard", proposalID})
	require.NoError(t, err)
	require.Equal(t, "proposals discard", p.cmd.name, "the longer name wins")
}

// The list shows the proposals only -- never a write screening held -- with
// who proposed each, and needs no terminal: it decides nothing.
func TestProposals_ListsOnlyProposals(t *testing.T) {
	ac := heldConsole(t)
	e, out, errb := ac.env()
	require.Equal(t, 0, dispatch(context.Background(), e, []string{"proposals"}), errb.String())
	require.Equal(t,
		"  "+proposalID+"  constraint memory example-project/run-the-linter  proposed by account "+heldAccount+"  -\n"+
			"  "+staleID+"  convention memory other-project/pin-the-toolchain  proposed by account "+heldAccount+"  -  (changed since: discard only)\n",
		out.String())
	require.NotContains(t, out.String(), screenedID)
	require.Equal(t, "/console/held?format=json", ac.requests()[0].uri)
	require.Equal(t, "Bearer fixture-key", ac.requests()[0].header.Get("Authorization"))

	e, out, errb = ac.env()
	require.Equal(t, 0, dispatch(context.Background(), e, []string{"proposals", "--project", "other-project"}), errb.String())
	require.NotContains(t, out.String(), proposalID)
	require.Contains(t, out.String(), staleID)

	e, out, errb = ac.env()
	require.Equal(t, 0, dispatch(context.Background(), e, []string{"proposals", "--project", "nowhere"}), errb.String())
	require.Equal(t, "(no proposals waiting)\n", out.String())
}

// --json prints the console's own row objects, unknown fields kept.
func TestProposals_JSONPassesTheRowsThrough(t *testing.T) {
	ac := heldConsole(t)
	e, out, errb := ac.env()
	require.Equal(t, 0, dispatch(context.Background(), e, []string{"proposals", "--json", "--project", "example-project"}), errb.String())
	var got struct {
		Proposals []map[string]any `json:"proposals"`
	}
	require.NoError(t, json.Unmarshal([]byte(out.String()), &got))
	require.Len(t, got.Proposals, 1)
	row := got.Proposals[0]
	require.Equal(t, proposalID, row["id"])
	require.Equal(t, "kept", row["extra"], "a field this CLI does not know survives")
	require.Equal(t, heldAccount, row["account"])
	require.True(t, strings.HasSuffix(out.String(), "}\n"))

	e, out, errb = ac.env()
	require.Equal(t, 0, dispatch(context.Background(), e, []string{"proposals", "--json", "--project", "nowhere"}), errb.String())
	require.JSONEq(t, `{"proposals":[]}`, out.String())
}

// A daemon that keeps no proposals says so, rather than "the screening
// feature is disabled".
func TestProposals_DaemonWithoutRestrictedAccounts(t *testing.T) {
	ac := newAccessConsole(t, func(w http.ResponseWriter, _ *http.Request) {
		w.WriteHeader(http.StatusForbidden)
		_, _ = w.Write([]byte(`{"error":"` + screeningOffError + `"}`))
	})
	e, _, errb := ac.env()
	require.Equal(t, 1, dispatch(context.Background(), e, []string{"proposals"}))
	require.Contains(t, errb.String(), "this daemon keeps no proposals: it names no restricted accounts (peer_auth.restricted)")
}

// Deciding is the owner's: without a terminal neither command reads or
// writes anything.
func TestPromote_RefusesWithoutATerminal(t *testing.T) {
	for _, argv := range [][]string{{"promote", proposalID}, {"proposals", "discard", proposalID}} {
		ac := heldConsole(t)
		e, out, errb := ac.env()
		require.Equal(t, 1, dispatch(context.Background(), e, argv), "%v", argv)
		require.Contains(t, errb.String(), "seam "+strings.Join(argv[:len(argv)-1], " ")+
			" is the owner's own terminal path: it needs a terminal on stdin and stdout, and an agent cannot use it")
		require.Contains(t, errb.String(), "(the owner decides proposals here, or on the console's Held screen)")
		require.Empty(t, out.String())
		require.Empty(t, ac.requests(), "nothing is fetched, let alone decided")
	}
	e, out, _ := stubEnv()
	require.Equal(t, 0, dispatch(context.Background(), e, []string{"promote", "--help"}), "help needs no terminal")
	require.Contains(t, out.String(), "usage: seam promote <hold>")
}

func TestPromote_PromotesAfterTheOwnerSaysYes(t *testing.T) {
	ac := heldConsole(t)
	e, out, errb := ac.env()
	require.Equal(t, 0, dispatch(context.Background(), withOwnerTerminal(e, "y\n"), []string{"promote", proposalID}), errb.String())
	text := out.String()
	for _, want := range []string{
		"Proposal " + proposalID,
		"Proposed by     account " + heldAccount,
		"Where           example-project/run-the-linter",
		"Why it waits    account.restricted: a restricted account proposed a binding rule; it waits for your promotion",
		"    | Run the linter first.",
		"Promote example-project/run-the-linter? It lands exactly as written. [y/N] ",
		"Promoted example-project/run-the-linter: it landed as account " + heldAccount + " wrote it.\n",
	} {
		require.Contains(t, text, want)
	}
	require.Equal(t, []string{"/console/held/" + proposalID + "/release?format=json"}, postedURIs(ac.requests()))
	require.Equal(t, "Bearer fixture-key", ac.requests()[1].header.Get("Authorization"))

	// Anything but yes changes nothing; so does a unique prefix answered no.
	for _, answer := range []string{"n\n", "\n", ""} {
		ac := heldConsole(t)
		e, out, errb := ac.env()
		require.Equal(t, 0, dispatch(context.Background(), withOwnerTerminal(e, answer), []string{"promote", proposalID[:14]}), errb.String())
		require.Contains(t, out.String(), "Nothing changed.")
		require.Empty(t, postedURIs(ac.requests()), "answer %q", answer)
	}
}

func TestProposalsDiscard_DiscardsAfterTheOwnerSaysYes(t *testing.T) {
	ac := heldConsole(t)
	e, out, errb := ac.env()
	require.Equal(t, 0, dispatch(context.Background(), withOwnerTerminal(e, "yes\n"), []string{"proposals", "discard", staleID}), errb.String())
	require.Contains(t, out.String(), "Discard other-project/pin-the-toolchain? Nothing lands, and its text is erased. [y/N] ")
	require.Contains(t, out.String(), "Discarded other-project/pin-the-toolchain: nothing landed, and its text is gone.\n")
	require.Equal(t, []string{"/console/held/" + staleID + "/discard?format=json"}, postedURIs(ac.requests()))
}

// What promote refuses, before asking anything: a write screening held, a
// stale proposal, one already decided, an id that names nothing or several.
func TestPromote_Refusals(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
		want string
	}{
		{"a screening hold", []string{"promote", screenedID},
			screenedID + " is a write held by write screening, not a restricted account's proposal: decide it on the console's Held screen"},
		{"a screening hold, discard", []string{"proposals", "discard", screenedID}, "not a restricted account's proposal"},
		{"a stale proposal", []string{"promote", staleID},
			"other-project/pin-the-toolchain changed after " + staleID + " was proposed, so promoting it would undo that change: " +
				"discard it with seam proposals discard " + staleID},
		{"already promoted", []string{"promote", promotedID}, promotedID + " is already promoted: nothing to decide"},
		{"unknown", []string{"promote", "01KNOSUCHHOLD"}, `no proposal "01KNOSUCHHOLD" is waiting (seam proposals lists them)`},
		{"ambiguous", []string{"promote", "01K"}, `"01K" names 3 held writes`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			ac := heldConsole(t)
			e, out, errb := ac.env()
			require.Equal(t, 1, dispatch(context.Background(), withOwnerTerminal(e, "y\n"), tc.argv))
			require.Contains(t, errb.String(), tc.want)
			require.NotContains(t, out.String(), "[y/N]", "refused before asking")
			require.Empty(t, postedURIs(ac.requests()))
		})
	}
}

// The transcriptions pinned to the daemon: the reason code is screening's,
// and the refusal is what the real console answers for its held queue while
// neither write screening nor restricted accounts are on.
func TestProposals_TranscriptionsMatchTheDaemon(t *testing.T) {
	require.Equal(t, string(screening.ReasonRestrictedAccount), reasonRestrictedAccount)
	require.True(t, screening.IsProposal([]string{reasonRestrictedAccount}))

	rig := newHeldRig(t, config.Features{}, false)
	e, _, _ := rig.env()
	cfg, err := e.loadConfig()
	require.NoError(t, err)
	_, err = consoleRequest(cfg, http.MethodGet, heldQueuePath)
	require.EqualError(t, err, screeningOffError)
}

// End to end against the real console routes, with write screening off and
// restricted accounts configured: seam lists the proposal the queue holds,
// and promote lands it -- the shapes seam decodes are the console's own.
func TestProposals_AgainstTheConsole(t *testing.T) {
	rig := newHeldRig(t, config.Features{}, true)
	ctx := context.Background()
	p := rig.hold(t, "run-the-linter", []string{string(screening.ReasonRestrictedAccount)})
	sh := rig.hold(t, "ignore-everything", []string{"instruction.override"})

	e, out, errb := rig.env()
	require.Equal(t, 0, dispatch(ctx, e, []string{"proposals"}), errb.String())
	require.Contains(t, out.String(), p.ID+"  constraint memory example-project/run-the-linter  proposed by account "+heldAccount)
	require.NotContains(t, out.String(), sh.ID)

	e, out, errb = rig.env()
	require.Equal(t, 0, dispatch(ctx, withOwnerTerminal(e, "y\n"), []string{"promote", p.ID}), errb.String())
	require.Contains(t, out.String(), "    | Run the linter first.")
	require.Contains(t, out.String(), "Promoted example-project/run-the-linter: it landed as account "+heldAccount+" wrote it.")
	m, ok, err := store.MemoryByName(ctx, rig.db, "example-project", "run-the-linter")
	require.NoError(t, err)
	require.True(t, ok, "the promotion landed the memory")
	require.Equal(t, heldAccount, m.Account)

	// Decided, it is no longer anything to decide.
	e, _, errb = rig.env()
	require.Equal(t, 1, dispatch(ctx, withOwnerTerminal(e, "y\n"), []string{"promote", p.ID}))
	require.Contains(t, errb.String(), p.ID+" is already promoted: nothing to decide")
}

// heldRig is a real console over a fresh database, served over TCP with a
// bearer key, the way the owner's seam reaches it.
type heldRig struct {
	db  *sql.DB
	srv *httptest.Server
	scr *screening.Service
}

func newHeldRig(t *testing.T, feats config.Features, restricted bool) *heldRig {
	t.Helper()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mgr, err := files.NewManager(dir, db, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	rec := events.NewRecorder(db)
	scr := screening.New(screening.Config{DB: db, Files: mgr, Events: rec})
	cs, err := console.New(console.Config{DB: db, Events: rec, Files: mgr, Screening: scr, APIKey: "fixture-key",
		Level: "advanced", Features: feats, RestrictedAccounts: restricted})
	require.NoError(t, err)
	mux := http.NewServeMux()
	cs.Register(mux)
	srv := httptest.NewServer(mux)
	t.Cleanup(srv.Close)
	return &heldRig{db: db, srv: srv, scr: scr}
}

// env is a test env aimed at the rig's console.
func (r *heldRig) env() (*env, *strings.Builder, *strings.Builder) {
	e, _, _ := stubEnv()
	var out, errb strings.Builder
	e.stdout, e.stderr = &out, &errb
	cfg := config.Defaults()
	cfg.Addr = strings.TrimPrefix(r.srv.URL, "http://")
	cfg.MCP.APIKey = "fixture-key"
	e.loadConfig = func() (config.Config, error) { return cfg, nil }
	return e, &out, &errb
}

// hold queues a constraint the way the MCP surface does; a proposal carries
// the account in its detail and its frontmatter.
func (r *heldRig) hold(t *testing.T, name string, reasons []string) store.HeldWrite {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	now := time.Now().UTC()
	detail := map[string]string{"path": files.MemoryRelPath("example-project", name)}
	account := ""
	if screening.IsProposal(reasons) {
		account = heldAccount
		detail[screening.DetailAccount] = account
	}
	content, err := files.RenderMemory(core.Memory{ID: id, Kind: core.KindConstraint, Name: name,
		Description: "a held rule", Project: "example-project", Body: "Run the linter first.\n",
		Created: now, Updated: now, ValidFrom: now, Account: account})
	require.NoError(t, err)
	h, err := r.scr.Hold(context.Background(), store.HeldWrite{
		Op: store.HeldOpWrite, Tool: "memory_write", ItemKind: core.ItemKindMemory, ItemID: id,
		Project: "example-project", Name: name, MemoryKind: core.KindConstraint, Reasons: reasons,
		Content: content, Detail: detail,
	})
	require.NoError(t, err)
	return h
}
