package main

// Golden output for every read command's DEFAULT rendering.
//
// The opt-in rule for the agent-facing CLI work: existing users see no change
// unless they ask for one. --json and the new verbs are additive, so the text a
// command printed before them must still be what it prints now, byte for byte.
// These expectations were captured from the commands as they stood BEFORE the
// --json flags were added, against the canned daemon in daemon_fixture_test.go,
// and are deliberately literal: a renderer change has to edit this file, in the
// diff, rather than slip through.
//
// Ages are pinned without a clock: a zero time renders "-" and a far-future one
// renders "now" (agoShort), so nothing here drifts with the calendar.

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// The canned daemon. Each reply carries fields no renderer prints, which is the
// point for --json: those must survive to stdout untouched.
const (
	fxRecall = `{"hits":[` +
		`{"kind":"memory","id":"01KAAAAAAAAAAAAAAAAAAAAAAA","name":"lease-steal-window","title":"lease-steal-window","description":"a claim can be stolen inside the lease grace","project":"example-project","age":"3d","source":"fused","score":0.0331},` +
		`{"kind":"task","id":"01KBBBBBBBBBBBBBBBBBBBBBBB","name":"","title":"fix the lease grace","description":"tighten the window","age":"1h","source":"fts","score":0.0164,"status":"open"},` +
		`{"kind":"note","id":"01KCCCCCCCCCCCCCCCCCCCCCCC","name":"lease-design","title":"Lease design","description":"how leases work","age":"2w","source":"semantic","score":0.0159,"favorite":true}` +
		`]}`
	fxRecallEmpty = `{"hits":[]}`

	fxReady = `{"ready":[` +
		`{"id":"01KDDDDDDDDDDDDDDDDDDDDDDD","title":"write the migration","status":"open","project":"example-project"},` +
		`{"id":"01KEEEEEEEEEEEEEEEEEEEEEEE","title":"wire the console","status":"open"}` +
		`],"blocked":[` +
		`{"id":"01KFFFFFFFFFFFFFFFFFFFFFFF","title":"ship it","status":"open","blockers":[{"id":"01KDDDDDDDDDDDDDDDDDDDDDDD","title":"write the migration","status":"open"}]}` +
		`]}`
	fxReadyEmpty = `{"ready":[],"blocked":[]}`

	fxTasks = `{"tasks":[` +
		`{"id":"01KDDDDDDDDDDDDDDDDDDDDDDD","title":"write the migration","status":"in_progress","body":"details"},` +
		`{"id":"01KGGGGGGGGGGGGGGGGGGGGGGG","title":"old idea","status":"dropped"}` +
		`]}`
	fxTasksEmpty = `{"tasks":[]}`

	fxUsage = `{"memories":{"active":12,"byKind":{"constraint":3,"gotcha":9}},"notes":4,` +
		`"sessions":{"active":1,"completed":7},"tasks":{"done":2,"open":5},` +
		`"retrieval":{"injections":40,"reads":10,"topInjected":[{"name":"lease-steal-window","count":7}],` +
		`"topUtility":[{"name":"lease-steal-window","score":1.25}]},"gardenerPending":{}}`

	fxProjects = `{"projects":[{"slug":"example-project","isolation":"open"},{"slug":"other-project","isolation":"open"}]}`

	fxHealthz = `{"status":"ok","version":"0.7.2+cafe123","commit":"cafe123","built":"2026-11-01T00:00:00Z","distribution":"release","update_available":"0.7.3","uptime":"1h"}`

	fxSessions = `{"total":2,"active":1,"sessions":[` +
		`{"id":"01KHHHHHHHHHHHHHHHHHHHHHHH","name":"cc/quiet-otter","project":"example-project","status":"active","ambient":true,"findings":"","updated":"2999-01-01T00:00:00Z"},` +
		`{"id":"01KJJJJJJJJJJJJJJJJJJJJJJJ","name":"","project":"","status":"completed","ambient":false,"findings":"done","updated":"0001-01-01T00:00:00Z"}` +
		`]}`
	fxSessionsActive = `{"total":2,"active":1,"sessions":[` +
		`{"id":"01KHHHHHHHHHHHHHHHHHHHHHHH","name":"cc/quiet-otter","project":"example-project","status":"active","ambient":true,"findings":"","updated":"2999-01-01T00:00:00Z"}` +
		`]}`
	fxSessionDetail = `{"session":{"id":"01KHHHHHHHHHHHHHHHHHHHHHHH","name":"cc/quiet-otter","projectSlug":"example-project","status":"active","model":"example-model-1"},` +
		`"findings":"found the lease bug\nand fixed it","toolCalls":14,"memoryReads":3,"memoryWrites":2,"injectedItems":8,"readAfterInject":2,"events":[]}`

	fxPlans = `{"rows":[` +
		`{"noteId":"01KKKKKKKKKKKKKKKKKKKKKKKK","slug":"lease-rework","basename":"lease-rework","title":"Rework leases","project":"example-project","status":"approved","iteration":2,"agents":1,"tasksDone":1,"tasksTotal":3,"updated":"2999-01-01T00:00:00Z","phase":"in_progress"},` +
		`{"noteId":"01KMMMMMMMMMMMMMMMMMMMMMMM","slug":"global-cleanup","basename":"global-cleanup","title":"Clean up","project":"","status":"draft","iteration":1,"agents":0,"tasksDone":0,"tasksTotal":0,"updated":"0001-01-01T00:00:00Z","phase":"ready"}` +
		`],"count":2,"total":2,"window":"all","windowLabel":"all time"}`
	fxPlansEmpty  = `{"rows":[],"count":0,"total":2,"window":"24h","windowLabel":"24 hours"}`
	fxPlanDetail  = `{"plan":{"noteId":"01KKKKKKKKKKKKKKKKKKKKKKKK","slug":"lease-rework","basename":"lease-rework","title":"Rework leases","project":"example-project","status":"approved","iteration":2,"agents":1,"tasksDone":1,"tasksTotal":3,"updated":"2999-01-01T00:00:00Z"},"body":"# Rework leases\n\nStep one.","attached":[{"id":"01KNNNNNNNNNNNNNNNNNNNNNNN","title":"Lease notes","slug":"lease-notes","isAgent":false,"updated":"0001-01-01T00:00:00Z"},{"id":"01KPPPPPPPPPPPPPPPPPPPPPPP","title":"Explore agent","slug":"cc-agent-1","isAgent":true,"updated":"2999-01-01T00:00:00Z"}],"tasks":[{"id":"01KDDDDDDDDDDDDDDDDDDDDDDD","title":"write the migration","status":"done"}],"canApprove":false}`
	fxProjectView = `{"slug":"example-project","isolation":"confidential","isolationPromise":"Nothing leaves this project.","memories":12}`

	fxMemoryRead = `{"id":"01KAAAAAAAAAAAAAAAAAAAAAAA","kind":"gotcha","name":"lease-steal-window","description":"a claim can be stolen inside the lease grace",` +
		`"project":"example-project","body":"The lease grace lets a second claimer in.\n\nWait for expiry.\n","tags":["leases","claims"],` +
		`"source_session":"01KQQQQQQQQQQQQQQQQQQQQQQQ","content_hash":"abc123","model":"example-model-1"}`
	fxMemorySuperseded = `{"id":"01KRRRRRRRRRRRRRRRRRRRRRRR","kind":"decision","name":"old-lease","description":"the old lease rule","project":"",` +
		`"body":"","tags":[],"source_session":"","content_hash":"0ff","warning":"superseded by lease-steal-window; read that instead"}`
	fxNoteRead = `{"id":"01KCCCCCCCCCCCCCCCCCCCCCCC","title":"Lease design","slug":"lease-design","description":"how leases work","project":"example-project",` +
		`"body":"# Lease design\n\nLeases expire.","tags":["plan:lease-rework"],"source_url":"https://example.com/leases","content_hash":"def456"}`
	fxRecallMemories = `{"hits":[` +
		`{"kind":"memory","id":"01KAAAAAAAAAAAAAAAAAAAAAAA","name":"lease-steal-window","title":"lease-steal-window","description":"a claim can be stolen inside the lease grace","project":"example-project","age":"3d","source":"browse","score":0},` +
		`{"kind":"memory","id":"01KSSSSSSSSSSSSSSSSSSSSSSS","name":"global-rule","title":"global-rule","description":"","age":"2w","source":"browse","score":0}` +
		`]}`
	fxRecallNotes = `{"hits":[` +
		`{"kind":"note","id":"01KCCCCCCCCCCCCCCCCCCCCCCC","name":"lease-design","title":"Lease design","description":"how leases work","project":"example-project","age":"2w","source":"fused","score":0.0312}` +
		`]}`

	fxIsolationApplied = `{"project":"example-project","requested":"sealed","state":"sealed","promise":"Nothing in or out.","requestedPromise":"Nothing in or out.",` +
		`"applied":true,"confirmRequired":false,"globalMemories":0,"filed":2,"effects":{"families":["fam-a"],"parent":"parent-project"}}`
	fxIsolationConfirm = `{"project":"example-project","requested":"sealed","state":"open","promise":"Shares normally.","requestedPromise":"Nothing in or out.",` +
		`"applied":false,"confirmRequired":true,"globalMemories":3,"filed":0,"effects":{"families":["fam-a"],"parent":"parent-project"}}`
)

// goldenTools is the canned tool surface. A reply that depends on an argument
// is how one fixture serves both the populated and the empty rendering.
func goldenTools() map[string]toolFunc {
	return map[string]toolFunc{
		"recall": func(a map[string]any) toolReply {
			switch {
			case str(a["query"]) == "nothing":
				return toolReply{text: fxRecallEmpty}
			case str(a["scope"]) == "memories":
				return toolReply{text: fxRecallMemories}
			case str(a["scope"]) == "notes":
				return toolReply{text: fxRecallNotes}
			}
			return toolReply{text: fxRecall}
		},
		"memory_read": func(a map[string]any) toolReply {
			if str(a["name"]) == "old-lease" {
				return toolReply{text: fxMemorySuperseded}
			}
			return toolReply{text: fxMemoryRead}
		},
		"notes_read": fixedReply(fxNoteRead),
		"tasks_release": func(map[string]any) toolReply {
			return toolReply{text: "tasks_release: task 01KDDDDD is not claimed by this session", isError: true}
		},
		"tasks_ready": func(a map[string]any) toolReply {
			if str(a["plan"]) == "empty-plan" {
				return toolReply{text: fxReadyEmpty}
			}
			return toolReply{text: fxReady}
		},
		"tasks_list": func(a map[string]any) toolReply {
			if str(a["status"]) == "open" {
				return toolReply{text: fxTasksEmpty}
			}
			return toolReply{text: fxTasks}
		},
		"usage_summary": fixedReply(fxUsage),
		"project_list":  fixedReply(fxProjects),
	}
}

// goldenConsole is the canned console JSON surface, keyed as the CLI requests it.
func goldenConsole() map[string]string {
	return map[string]string{
		"GET /console/sessions?format=json":                                                   fxSessions,
		"GET /console/sessions?format=json&status=active":                                     fxSessionsActive,
		"GET /console/sessions/01KHHHHHHHHHHHHHHHHHHHHHHH?format=json":                        fxSessionDetail,
		"GET /console/plans?format=json&w=all":                                                fxPlans,
		"GET /console/plans?format=json&w=24h":                                                fxPlansEmpty,
		"GET /console/plans/lease-rework?format=json":                                         fxPlanDetail,
		"GET /console/projects/example-project?format=json":                                   fxProjectView,
		"POST /console/projects/example-project/isolation?confirm=1&format=json&state=sealed": fxIsolationApplied,
		"POST /console/projects/example-project/isolation?format=json&state=sealed":           fxIsolationConfirm,
	}
}

func newGoldenFixture(t *testing.T) *daemonFixture {
	t.Helper()
	return newDaemonFixture(t, goldenTools(), goldenConsole(), fxHealthz)
}

// serverPlaceholder stands in for the fixture's random address in a golden.
const serverPlaceholder = "http://SERVER"

// goldenCases is every read command's default output, as captured before
// --json existed. code is the exit status; stdout is compared byte for byte.
var goldenCases = []struct {
	name   string
	argv   []string
	code   int
	stdout string
}{
	{"recall", []string{"recall", "lease", "grace"}, 0,
		"[memory] lease-steal-window (3d, fused 0.033)\n" +
			"    a claim can be stolen inside the lease grace\n" +
			"[task open] fix the lease grace (1h, fts 0.016)\n" +
			"    tighten the window\n" +
			"[note] lease-design (2w, semantic 0.016)\n" +
			"    how leases work\n"},
	{"recall empty", []string{"recall", "nothing"}, 0,
		"no results\n"},
	{"ready", []string{"ready"}, 0,
		"ready (2):\n" +
			"  01KDDDDD  write the migration\n" +
			"  01KEEEEE  wire the console\n"},
	{"ready blocked", []string{"ready", "--blocked"}, 0,
		"ready (2):\n" +
			"  01KDDDDD  write the migration\n" +
			"  01KEEEEE  wire the console\n" +
			"blocked (1):\n" +
			"  01KFFFFF  ship it\n" +
			"      blocked by write the migration (open)\n"},
	{"ready empty", []string{"ready", "--plan", "empty-plan"}, 0,
		"ready: (nothing actionable)\n"},
	{"task list", []string{"task", "list"}, 0,
		"  01KDDDDD  [in_progress] write the migration\n" +
			"  01KGGGGG  [dropped    ] old idea\n"},
	{"task list empty", []string{"task", "list", "--status", "open"}, 0,
		"(no tasks)\n"},
	{"plan list", []string{"plan", "list"}, 0,
		"  lease-rework             [approved ] Rework leases  (example-project, iter 2, 1 agents, tasks 1/3, now)\n" +
			"  global-cleanup           [draft    ] Clean up  (-, iter 1, 0 agents, tasks 0/0, -)\n"},
	{"plan list project", []string{"plan", "list", "--project", "example-project"}, 0,
		"  lease-rework             [approved ] Rework leases  (example-project, iter 2, 1 agents, tasks 1/3, now)\n"},
	{"plan list empty", []string{"plan", "list", "--window", "24h"}, 0,
		"(no captured plans)\n"},
	{"plan show", []string{"plan", "show", "lease-rework"}, 0,
		"lease-rework  [approved]  example-project\n" +
			"title:    Rework leases\n" +
			"file:     lease-rework.md (iteration 2)\n" +
			"attached: 2 note(s)\n" +
			"  [note ] 01KNNNNN  Lease notes (-)\n" +
			"  [agent] 01KPPPPP  Explore agent (now)\n" +
			"tasks:    1\n" +
			"  01KDDDDD  [done       ] write the migration\n" +
			"\n" +
			"# Rework leases\n\nStep one.\n"},
	{"status", []string{"status"}, 0,
		"server:   ok (" + serverPlaceholder + ")\n" +
			"version:  0.7.2+cafe123\n" +
			"update:   v0.7.3 (seamlessd update --check on the daemon's machine shows how to update)\n" +
			"data dir: /data/seamless\n" +
			"projects: 2 [example-project other-project]\n"},
	{"sessions", []string{"sessions"}, 0,
		"2 sessions (1 active)\n" +
			"  cc/quiet-otter       example-project active    now (ambient)\n" +
			"  01KJJJJJ             -          completed -\n"},
	{"sessions status", []string{"sessions", "--status", "active"}, 0,
		"2 sessions (1 active)\n" +
			"  cc/quiet-otter       example-project active    now (ambient)\n"},
	{"sessions detail", []string{"sessions", "01KHHHHHHHHHHHHHHHHHHHHHHH"}, 0,
		"cc/quiet-otter  [active]  example-project\n" +
			"model: example-model-1\n" +
			"tool calls: 14  writes: 2  reads: 3  read-after-inject: 2/8\n" +
			"\n" +
			"findings:\n" +
			"found the lease bug\nand fixed it\n"},
	{"usage", []string{"usage"}, 0,
		"memories: 12 active\n" +
			"  by kind: constraint=3 gotcha=9\n" +
			"notes:    4\n" +
			"sessions: active=1 completed=7\n" +
			"tasks: done=2 open=5\n" +
			"retrieval: 40 injections, 10 reads (25% read-after-inject)\n" +
			"  most injected:\n" +
			"    lease-steal-window               7x\n" +
			"  highest utility (decayed demand):\n" +
			"    lease-steal-window               1.25\n" +
			"proposals: (none)\n"},
	{"project isolation show", []string{"project", "isolation", "example-project"}, 0,
		"example-project  isolation: confidential\n" +
			"  Nothing leaves this project.\n"},
	{"project isolation applied", []string{"project", "isolation", "example-project", "sealed", "--yes"}, 0,
		"example-project  isolation set to sealed\n" +
			"  Nothing in or out.\n" +
			"  left family fam-a\n" +
			"  detached from parent parent-project\n" +
			"  filed 2 relocation proposals in the Gardener inbox\n"},
	{"project isolation confirm", []string{"project", "isolation", "example-project", "sealed"}, 1,
		"example-project  isolation: open -> sealed (not applied)\n" +
			"  Nothing in or out.\n" +
			"  leaves family fam-a\n" +
			"  detaches from parent parent-project\n" +
			"  3 global memories originated in this project's sessions; relocation proposals will be filed in the Gardener inbox\n"},
	{"version", []string{"version"}, 0,
		"seamlessd 0.7.2 (commit cafe123, built 2026-11-01T00:00:00Z, release build)\n" +
			"update available: v0.7.3 (seamlessd update --check on the daemon's machine shows how to update)\n"},
}

func TestGolden_DefaultOutputIsUnchanged(t *testing.T) {
	fx := newGoldenFixture(t)
	for _, tc := range goldenCases {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := fx.run(t, tc.argv...)
			require.Equal(t, tc.code, code, "stderr: %s", stderr)
			require.Equal(t, tc.stdout, strings.ReplaceAll(stdout, fx.srv.URL, serverPlaceholder))
		})
	}
}
