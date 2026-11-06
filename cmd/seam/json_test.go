package main

// --json prints the daemon's answer as-is: the exact tool result text or console
// body the stand-in daemon sent, newline-terminated, so every field survives to
// jq -- including the ones no renderer prints (the fixtures carry several).

import (
	"context"
	"encoding/json"
	"errors"
	"testing"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
)

func TestJSON_PrintsTheDaemonsAnswerAsIs(t *testing.T) {
	fx := newGoldenFixture(t)
	for _, tc := range []struct {
		name string
		argv []string
		code int
		want string
	}{
		{"recall", []string{"recall", "lease", "--json"}, 0, fxRecall},
		{"memory read", []string{"memory", "read", "lease-steal-window", "--json"}, 0, fxMemoryRead},
		{"memory list", []string{"memory", "list", "--kind", "gotcha", "--json"}, 0, fxRecallMemories},
		{"note read", []string{"note", "read", "lease-design", "--json"}, 0, fxNoteRead},
		{"note list", []string{"note", "list", "lease", "--json"}, 0, fxRecallNotes},
		{"ready", []string{"ready", "--json"}, 0, fxReady},
		// tasks_ready always answers both lists, so --blocked has nothing to add.
		{"ready blocked", []string{"ready", "--blocked", "--json"}, 0, fxReady},
		{"task list", []string{"task", "list", "--json"}, 0, fxTasks},
		{"plan list", []string{"plan", "list", "--json"}, 0, fxPlans},
		{"plan show", []string{"plan", "show", "lease-rework", "--json"}, 0, fxPlanDetail},
		{"sessions", []string{"sessions", "--json"}, 0, fxSessions},
		{"sessions status", []string{"sessions", "--status", "active", "--json"}, 0, fxSessionsActive},
		{"sessions detail", []string{"sessions", "01KHHHHHHHHHHHHHHHHHHHHHHH", "--json"}, 0, fxSessionDetail},
		{"usage", []string{"usage", "--json"}, 0, fxUsage},
		{"project isolation show", []string{"project", "isolation", "example-project", "--json"}, 0, fxProjectView},
		{"project isolation applied", []string{"project", "isolation", "example-project", "sealed", "--yes", "--json"}, 0, fxIsolationApplied},
		// An unconfirmed tighten still exits 1 under --json: the answer is printed,
		// and the verdict that nothing was applied is still the command's result.
		{"project isolation confirm", []string{"project", "isolation", "example-project", "sealed", "--json"}, 1, fxIsolationConfirm},
		{"version", []string{"version", "--json"}, 0, fxHealthz},
		{"status", []string{"status", "--json"}, 0, `{"healthz":` + fxHealthz + `,"project_list":` + fxProjects + `}`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := fx.run(t, tc.argv...)
			require.Equal(t, tc.code, code, "stderr: %s", stderr)
			require.True(t, json.Valid([]byte(stdout)), "stdout must parse as JSON: %q", stdout)
			require.Equal(t, tc.want+"\n", stdout, "the daemon's answer, byte for byte")
		})
	}
}

// --json changes the rendering and nothing else: the tool is called with exactly
// the arguments the text rendering sends, so the two can never answer different
// questions.
func TestJSON_SendsTheSameArguments(t *testing.T) {
	fx := newGoldenFixture(t)
	for _, argv := range [][]string{
		{"recall", "lease", "grace", "--scope", "notes", "--limit", "3", "--project", "example-project"},
		{"ready", "--plan", "lease-rework", "--project", "example-project"},
		{"task", "list", "--status", "done", "--plan", "lease-rework"},
		{"usage"},
		{"memory", "list", "--kind", "gotcha", "--limit", "5"},
		{"note", "list", "lease", "--project", "example-project"},
	} {
		_, stderr, code := fx.run(t, argv...)
		require.Equal(t, 0, code, "%v: %s", argv, stderr)
		text := fx.lastCall(t)

		_, stderr, code = fx.run(t, append(argv, "--json")...)
		require.Equal(t, 0, code, "%v --json: %s", argv, stderr)
		asJSON := fx.lastCall(t)

		require.Equal(t, text.name, asJSON.name, "%v", argv)
		require.Equal(t, text.args, asJSON.args, "%v", argv)
	}
}

// plan list filters --project on the client, from rows the route returns for
// every project. --json prints the route's answer untouched, so the pair cannot
// both be honored; it is refused with the filter that does both, before any
// request -- never a listing that silently ignores --project.
func TestJSON_PlanListRefusesProjectFilter(t *testing.T) {
	e, out, errb := stubEnv() // loadConfig is nil: reaching it would panic
	code := dispatch(context.Background(), e, []string{"plan", "list", "--json", "--project", "example-project"})
	require.Equal(t, 1, code)
	require.Empty(t, out.String())
	require.Contains(t, errb.String(), `jq '.rows[] | select(.project == "example-project")'`)
}

// A tool answer that is not JSON is refused under --json exactly as the text
// rendering refuses it: a pipe into jq must never receive something other than
// seamlessd's answer as if it were one.
func TestJSON_RefusesAnAnswerThatIsNotJSON(t *testing.T) {
	fx := newDaemonFixture(t, map[string]toolFunc{
		"usage_summary": fixedReply("<html>a proxy answered</html>"),
		"tasks_list":    fixedReply(""),
	}, map[string]string{"GET /console/plans?format=json&w=all": "<html>"}, "not json")
	for _, argv := range [][]string{
		{"usage", "--json"},
		{"task", "list", "--json"},
		{"plan", "list", "--json"},
		{"version", "--json"},
	} {
		stdout, stderr, code := fx.run(t, argv...)
		require.Equal(t, 1, code, "%v", argv)
		require.Empty(t, stdout, "%v", argv)
		require.Contains(t, stderr, "unreadable", "%v", argv)
	}
}

// status --json keeps status's contract: a half that could not be read is
// null, every failure is named, and the command exits 1.
func TestJSON_StatusPartialAnswerStillFails(t *testing.T) {
	fx := newGoldenFixture(t)
	e, out, errb := fx.env()
	e.dial = func(context.Context) (*mcpclient.Client, config.Config, error) {
		return nil, config.Config{}, errors.New("connection refused")
	}
	require.Equal(t, 1, dispatch(context.Background(), e, []string{"status", "--json"}))
	require.Equal(t, `{"healthz":`+fxHealthz+`,"project_list":null}`+"\n", out.String())
	require.Contains(t, errb.String(), "status: 1 check(s) failed: projects: mcp unavailable: connection refused")

	fx = newDaemonFixture(t, goldenTools(), nil, "not json")
	stdout, stderr, code := fx.run(t, "status", "--json")
	require.Equal(t, 1, code)
	require.Equal(t, `{"healthz":null,"project_list":`+fxProjects+`}`+"\n", stdout)
	require.Contains(t, stderr, "status: 1 check(s) failed: server: unreadable health response: not JSON")
}

// A tool error is the command's error under --json too: nothing on stdout, the
// tool's text on stderr.
func TestJSON_ToolErrorIsTheCommandsError(t *testing.T) {
	fx := newDaemonFixture(t, map[string]toolFunc{
		"recall": func(map[string]any) toolReply {
			return toolReply{text: "recall: ambiguous scope", isError: true}
		},
	}, nil, fxHealthz)
	stdout, stderr, code := fx.run(t, "recall", "x", "--json")
	require.Equal(t, 1, code)
	require.Empty(t, stdout)
	require.Contains(t, stderr, "recall: ambiguous scope")
}
