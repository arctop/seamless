package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// seam task append sends body_append (tasks_update), and seam task list --id
// --revisions asks for the task's earlier bodies and prints them under it.
func TestTaskHistory_AppendAndRevisions(t *testing.T) {
	fx := newDaemonFixture(t, map[string]toolFunc{
		"tasks_update": func(map[string]any) toolReply {
			return toolReply{text: `{"id":"` + task + `","status":"open","body":"the plan\n\n2026-11-21 09:30 -- found it\n"}`}
		},
		"tasks_list": func(map[string]any) toolReply {
			return toolReply{text: `{"project":"demo","tasks":[{"id":"` + task + `","status":"open","title":"the step",` +
				`"body_revisions":[{"revision":2,"body":"the plan\nline two\n","change":"append",` +
				`"replaced_at":"2026-11-21T09:30:00.000000000Z","replaced_by":"01KSESSION"},` +
				`{"revision":1,"body":"the first plan","change":"replace","replaced_at":"2026-11-21T09:00:00.000000000Z"}]}]}`}
		},
	}, nil, "")

	stdout, stderr, code := fx.run(t, "task", "append", task, "found", "it")
	require.Equal(t, 0, code, stderr)
	require.Equal(t, "task 01KDDDDD: line added to the body\n", stdout)
	call := fx.lastCall(t)
	require.Equal(t, "tasks_update", call.name)
	require.Equal(t, "found it", call.args["body_append"])
	require.Equal(t, task, call.args["id"])

	stdout, stderr, code = fx.run(t, "task", "list", "--id", task, "--revisions")
	require.Equal(t, 0, code, stderr)
	require.Equal(t, true, fx.lastCall(t).args["body_revisions"])
	require.Equal(t, "  01KDDDDD  [open       ] the step\n"+
		"    revision 2, appended to 2026-11-21T09:30:00.000000000Z by 01KSESSION\n"+
		"      the plan\n      line two\n"+
		"    revision 1, replaced 2026-11-21T09:00:00.000000000Z\n"+
		"      the first plan\n", stdout)

	n := fx.callCount()
	_, stderr, code = fx.run(t, "task", "list", "--revisions")
	require.Equal(t, 1, code)
	require.Contains(t, stderr, "--revisions reads one task's earlier bodies: pass --id")
	_, stderr, code = fx.run(t, "task", "append", task, " ")
	require.Equal(t, 1, code)
	require.Contains(t, stderr, "seam task append: no text given")
	require.Equal(t, n, fx.callCount(), "nothing was sent")
}
