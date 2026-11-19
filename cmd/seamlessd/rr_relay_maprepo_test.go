//go:build darwin || linux

package main

// The owner maps a relay restricted account's clone on its own computer with
// `seamlessd map-repo --host` (plan networked-relay, task
// 01M7NYVADT3WEPRFVNPMVS7Q7R; the owner chose option (a), 2026-11-18). The
// account's texts stay word for word (decision B-D9), and the step they name
// now works for a clone behind a relay: through the relay's real TLS path
// (newRRRelayRig), the hook and session_start place the clone once the owner
// maps it under the relay's paired name, and under no other host.

import (
	"bytes"
	"context"
	"encoding/json"
	"net/url"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/devices"
)

func TestRRRelayReach_MapRepoHostPlacesARelayAccountsClone(t *testing.T) {
	d := newRRRelayRig(t)
	ctx := context.Background()
	const clone = "/Users/example-agent/repos/docs"
	uid := rrRelayUIDs[rrRelayPlain]
	label := rrRelayLabel(rrRelayPlain)

	// start posts the SessionStart of the caller of uid uid in the clone's path
	// through relay id, and returns the briefing the hook answered with.
	start := func(id *devices.RelayIdentity, uid uint32, sessionID string) string {
		t.Helper()
		q := url.Values{"host": {"example-claimed-host"}, "agent_process": {"7200.72000"}, "repo_root": {clone}, "main_root": {clone}}
		body := d.hook(t, id, uid, "session-start", q, map[string]any{"session_id": sessionID, "cwd": clone, "source": "startup",
			"hook_event_name": "SessionStart"})
		var shaped struct {
			HookSpecificOutput struct {
				AdditionalContext string `json:"additionalContext"`
			} `json:"hookSpecificOutput"`
		}
		require.NoError(t, json.Unmarshal([]byte(body), &shaped), body)
		return shaped.HookSpecificOutput.AdditionalContext
	}
	recorded := func(sessionID string) int {
		return raCount(t, d.db, `SELECT COUNT(*) FROM sessions WHERE claude_session_id LIKE ?`, "%"+sessionID+"%")
	}

	// Unmapped: today's notice, word for word, naming map-repo, and nothing
	// recorded.
	rrmPlainNotice(t, start(d.laptop, uid, "rrm-unmapped"), label)
	require.Zero(t, recorded("rrm-unmapped"))

	// The same path mapped on this machine (no --host) is a directory on the
	// server's disk: the relay account's clone still maps to nothing, and the
	// note names --host and the paired relays.
	var out, warn bytes.Buffer
	require.NoError(t, mapRepo(ctx, d.db, &out, &warn, clone, rrRelayDocs, ""))
	require.Contains(t, warn.String(), "seamlessd map-repo --host NAME --path "+clone+" --project "+rrRelayDocs)
	require.Contains(t, warn.String(), "(paired here: "+relayvDesk+", "+relayvLaptop+")")
	rrmPlainNotice(t, start(d.laptop, uid, "rrm-local"), label)
	require.Zero(t, recorded("rrm-local"))

	// Mapped under the relay's paired name, in any case: the next session there
	// is recorded in the project, as that account on that computer.
	warn.Reset()
	require.NoError(t, mapRepo(ctx, d.db, &out, &warn, clone, rrRelayDocs, "Example-Laptop"))
	require.Empty(t, warn.String(), "a paired relay's name needs no warning")
	inside := start(d.laptop, uid, "rrm-mapped")
	require.Contains(t, inside, "<seam-briefing>")
	require.NotContains(t, inside, "maps to no project")
	var project, account, host string
	require.NoError(t, d.db.QueryRow(`SELECT project_slug, account, host FROM sessions WHERE claude_session_id LIKE ?`,
		"%rrm-mapped%").Scan(&project, &account, &host))
	require.Equal(t, rrRelayDocs, project)
	require.Equal(t, label, account)
	require.Equal(t, relayvLaptop, host)

	// session_start with a cwd inside the clone places it too.
	got := paCallOK(t, d.client(t, rrRelayPlain), "session_start", map[string]any{"name": "rrm-mcp", "cwd": clone + "/src"})
	require.Equal(t, rrRelayDocs, got["project"], "%v", got)

	// The same path behind the other relay is that computer's directory, which
	// nothing mapped: its plain account (uid 512 there) is told so.
	deskUID := rrRelayUIDs[rrRelayDesk]
	other := start(d.desk, deskUID, "rrm-other-relay")
	rrmPlainNotice(t, other, access.RelayAccountLabel(rrRelayDesk, relayvDesk))
	require.Zero(t, recorded("rrm-other-relay"))
}

// rrmPlainNotice requires briefing to be a plain account's restricted notice
// (internal/hooks restrictedNotice), whose map-repo step stays word for word.
func rrmPlainNotice(t *testing.T, briefing, label string) {
	t.Helper()
	require.Contains(t, briefing, "this directory maps to no project account "+label+" may reach")
	require.Contains(t, briefing, "the owner maps this clone of an open project with "+
		"`seamlessd map-repo --path <repo> --project <slug>`, and the next session started in it is recorded.")
}
