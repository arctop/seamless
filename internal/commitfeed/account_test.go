package commitfeed

import (
	"context"
	"encoding/json"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// A restricted local account's sessions are listed by their qualified name,
// "<account>:<name>" (which may start with "_"), with account; a task names
// the restricted account that recorded a commit on it, else the one that
// created it, else the one whose words its text carries. The owner's entries
// carry no account (TestBuild_OwnerGolden).
func TestBuild_NamesARestrictedAccount(t *testing.T) {
	ctx := context.Background()
	db := feedDB(t)
	mapRepo(t, db, "h1", "/work/app", "app", "git@github.com:owner/app.git")
	day := 24 * time.Hour
	const acct, other = "_example_agent", "_example_peer"
	session := func(name, account string) string {
		t.Helper()
		id, err := core.NewID()
		require.NoError(t, err)
		require.NoError(t, store.CreateSession(ctx, db, core.Session{ID: id, Name: name, ProjectSlug: "app", Status: core.SessionCompleted,
			CWD: "/work/app", Host: "h1", Account: account, CreatedAt: taken.Add(-day), UpdatedAt: taken.Add(-day + time.Hour)}))
		return id
	}
	task := func(title, account string) string {
		t.Helper()
		id, err := core.NewID()
		require.NoError(t, err)
		require.NoError(t, store.CreateTask(ctx, db, core.Task{ID: id, ProjectSlug: "app", Title: title, Status: core.TaskOpen,
			Account: account, CreatedAt: taken.Add(-day), UpdatedAt: taken.Add(-day)}))
		return id
	}
	recordOn := func(taskID, by, commit string) {
		t.Helper()
		_, err := store.UpdateTask(ctx, db, taskID, store.TaskPatch{AddCommits: []string{commit}}, by, taken.Add(-time.Hour))
		require.NoError(t, err)
	}

	// CreateSession stores a restricted account's session inside its namespace.
	agentSess := session("cc/0a1b2c3d-4e5f6a7b", acct)
	_, err := store.RecordSessionCommits(ctx, db, agentSess, agentSess, []string{commitA}, taken.Add(-day+time.Minute))
	require.NoError(t, err)
	oddName := session("a name with spaces", acct)
	oddAcct := session("cc/9999-8888", "agent$box")
	ownerSess := session("cc/1111-2222", "")
	otherSess := session("cc/3333-4444", other)

	created := task("filed by the account", acct)
	recorded := task("the owner's, a commit recorded by the account", "")
	recordOn(recorded, agentSess, commitB)
	ownerOnly := task("the owner's, its commit recorded by the owner", "")
	recordOn(ownerOnly, ownerSess, commitC)
	both := task("filed by one account, recorded by another", acct)
	recordOn(both, otherSess, commitA)
	rewritten := task("the owner's, rewritten by the account", "")
	_, err = db.ExecContext(ctx, `UPDATE tasks SET updated_account = ? WHERE id = ?`, acct, rewritten)
	require.NoError(t, err)
	untouched := task("the owner's alone", "")

	snap, trim, err := Build(ctx, db, Options{Host: "h", Window: 30 * day, Now: taken})
	require.NoError(t, err)
	require.Equal(t, Trimmed{}, trim)
	raw := encode(t, snap)
	require.NoError(t, CheckJSON(raw, taken), "the snapshot passes the contract: %s", raw)
	got := decode(t, raw)

	byID := map[string]map[string]any{}
	for _, x := range got["sessions"].([]any) {
		e := x.(map[string]any)
		byID[e["id"].(string)] = e
	}
	require.Equal(t, map[string]any{
		"id": "_example_agent:cc/0a1b2c3d-4e5f6a7b", "account": acct, "repo": "app",
		"started_at": "2026-10-30T12:00:00.000Z", "ended_at": "2026-10-30T13:00:00.000Z", "commits": []any{commitA},
	}, byID["_example_agent:cc/0a1b2c3d-4e5f6a7b"], "by the qualified name it reports everywhere, with its account")
	require.Equal(t, acct, byID[oddName]["account"], "a name off the pattern: its ULID, still with its account")
	require.Regexp(t, `^h:[0-9a-f]{24}$`, byID[oddAcct]["account"], "an account off the pattern: its hash, never dropped")
	require.Equal(t, accountRef("agent$box"), byID[oddAcct]["account"])
	require.Equal(t, other, byID["_example_peer:cc/3333-4444"]["account"])
	require.NotContains(t, byID["cc/1111-2222"], "account", "the owner's session carries none")

	byTask := map[string]map[string]any{}
	for _, x := range got["tasks"].([]any) {
		e := x.(map[string]any)
		byTask[e["id"].(string)] = e
	}
	require.Equal(t, map[string]any{"id": created, "account": acct, "repo": "app"}, byTask[created])
	require.Equal(t, map[string]any{"id": recorded, "account": acct, "repo": "app", "commits": []any{commitB}}, byTask[recorded],
		"the account that vouched for its commit")
	require.Equal(t, other, byTask[both]["account"], "the recorder before the creator")
	require.Equal(t, acct, byTask[rewritten]["account"], "the account whose words it carries")
	require.Equal(t, map[string]any{"id": ownerOnly, "repo": "app", "commits": []any{commitC}}, byTask[ownerOnly])
	require.Equal(t, map[string]any{"id": untouched, "repo": "app"}, byTask[untouched])
}

// sessionNamed lists a session by its name only on the pattern: a bare name
// for anyone, the qualified form only for the session of the account it
// names, within 128 in all.
func TestSessionNamed(t *testing.T) {
	long := "_example_agent:cc/" + strings.Repeat("a", maxSessionID-len("_example_agent:cc/"))
	for _, tc := range []struct {
		name, account string
		ok            bool
	}{
		{"cc/0a1b2c3d-4e5f6a7b", "", true},
		{"example_agent:cc/abc", "example_agent", true},
		{"_example_agent:cc/abc", "_example_agent", true},
		{long, "_example_agent", true},
		{long + "a", "_example_agent", false},
		{"_example_agent:cc/abc", "", false},
		{"_example_agent:cc/abc", "_other", false},
		{"_example_agent:_x", "_example_agent", false},
		{"bad$acct:cc/abc", "bad$acct", false},
	} {
		require.Equal(t, tc.ok, sessionNamed(tc.name, tc.account), "%q (account %q)", tc.name, tc.account)
	}
}

// CheckJSON takes account where the contract has it -- on a session and on a
// task, absent or null for the owner -- and a session id in the qualified
// form, and refuses anything else there.
func TestCheckJSON_Account(t *testing.T) {
	good := func() map[string]any {
		return map[string]any{
			"kind": "seamless_feed", "version": 1, "host": "h1", "taken_at": "2026-10-31T12:00:00.000Z",
			"since": "2026-10-01T12:00:00Z",
			"sessions": []any{
				map[string]any{"id": "_example_agent:cc/aaaa-1111", "account": "_example_agent", "repo": "app",
					"started_at": "2026-10-30T12:00:00Z", "ended_at": nil},
				map[string]any{"id": "cc/bbbb-2222", "account": nil, "repo": nil, "started_at": "2026-10-30T12:00:00Z", "ended_at": nil},
			},
			"tasks": []any{
				map[string]any{"id": "01ARZ3NDEKTSV4RRFFQ69G5FAV", "account": "h:0123456789abcdef01234567"},
				map[string]any{"id": "01ARZ3NDEKTSV4RRFFQ69G5FAW"},
			},
		}
	}
	raw, err := json.Marshal(good())
	require.NoError(t, err)
	require.NoError(t, CheckJSON(raw, taken))

	session := func(m map[string]any) map[string]any { return m["sessions"].([]any)[0].(map[string]any) }
	task := func(m map[string]any) map[string]any { return m["tasks"].([]any)[0].(map[string]any) }
	for _, tc := range []struct {
		name   string
		mutate func(map[string]any)
		where  string
	}{
		{"an empty account", func(m map[string]any) { session(m)["account"] = "" }, "sessions[0].account: empty"},
		{"an account with a space", func(m map[string]any) { session(m)["account"] = "an agent" }, "sessions[0].account"},
		{"an account with a colon", func(m map[string]any) { session(m)["account"] = "a:b" }, "sessions[0].account"},
		{"an account starting with a dash", func(m map[string]any) { task(m)["account"] = "-agent" }, "tasks[0].account"},
		{"an account that is no string", func(m map[string]any) { task(m)["account"] = 7 }, "tasks[0].account"},
		{"a short hash", func(m map[string]any) { task(m)["account"] = "h:0123" }, "tasks[0].account"},
		{"a long account", func(m map[string]any) { task(m)["account"] = strings.Repeat("a", 65) }, "tasks[0].account"},
		{"account on the envelope", func(m map[string]any) { m["account"] = "_example_agent" }, "snapshot: a name"},
		{"a qualified id past 128", func(m map[string]any) {
			session(m)["id"] = "_example_agent:cc/" + strings.Repeat("a", 111)
		}, "sessions[0].id: longer than 128"},
		{"a qualified id with an empty name", func(m map[string]any) { session(m)["id"] = "_example_agent:" }, "sessions[0].id"},
		{"an id starting with an underscore alone", func(m map[string]any) { session(m)["id"] = "_cc/aaaa" }, "sessions[0].id"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			m := good()
			tc.mutate(m)
			raw, err := json.Marshal(m)
			require.NoError(t, err)
			err = CheckJSON(raw, taken)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.where)
		})
	}
}
