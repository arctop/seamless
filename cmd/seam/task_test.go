package main

import (
	"context"
	"testing"

	"github.com/stretchr/testify/require"
)

// The bugs the tasks group carried into the table, pinned against the real
// commands() rather than a fixture. Each one used to succeed while doing
// something other than what was typed, which is why none of them was ever
// reported: there was nothing to report.
//
// parse opens no connection, so every case here runs without a server. A case
// that reached a handler would nil-deref e.dial and say so loudly.

func TestTaskParse_RejectsTheSilentlyIgnoredArguments(t *testing.T) {
	for _, tc := range []struct {
		name string
		argv []string
		want string
	}{
		{
			// The worst one: --id was registered but NArg was never checked, so the
			// bare id was dropped and every task in the project came back. The
			// caller cannot tell that from a real answer.
			"task list <id> listed every task",
			[]string{"task", "list", "01K7ABCD"},
			`unexpected argument "01K7ABCD": seam task list takes no positional arguments`,
		},
		{
			// The remedy is not guessable from the generic frame, so it is per-spec.
			"and the hint names --id",
			[]string{"task", "list", "01K7ABCD"},
			"(to load one task by id, use --id)",
		},
		{
			// runTaskTransition read args[0] and ignored the rest: the second id was
			// silently never transitioned.
			"task done id1 id2 dropped id2",
			[]string{"task", "done", "01AAA", "01BBB"},
			`unexpected argument "01BBB": seam task done takes at most 1 positional argument`,
		},
		{
			"task claim takes exactly one id",
			[]string{"task", "claim", "01AAA", "01BBB"},
			"takes at most 1 positional argument",
		},
		{
			"a missing id is named, not defaulted",
			[]string{"task", "done"},
			"missing argument: seam task done requires <id>",
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, err := parse(commands(), tc.argv)
			require.Error(t, err)
			require.Contains(t, err.Error(), tc.want)
		})
	}
}

// --lease 0 and --lease -5 used to fall through `if *lease > 0` to the server's
// 900s default: the caller asked for one lease and silently got another. As a
// posIntValue they are parse errors, which is what makes the 0 default
// unambiguous -- 0 can now only mean "absent".
func TestTaskParse_LeaseRejectsNonPositive(t *testing.T) {
	for _, bad := range []string{"0", "-5", "abc"} {
		_, err := parse(commands(), []string{"task", "claim", "--lease", bad, "01AAA"})
		require.Error(t, err, "--lease %s must not reach the server", bad)
		require.Contains(t, err.Error(), "must be a positive integer")
	}

	p, err := parse(commands(), []string{"task", "claim", "--lease", "60", "01AAA"})
	require.NoError(t, err)
	require.Equal(t, 60, *p.opts.(*taskClaimOpts).lease)
}

// --status is derived from core.TaskStatuses, so the client rejects a typo at
// parse time instead of round-tripping. "in-progress" is the plausible near-miss.
func TestTaskParse_StatusEnum(t *testing.T) {
	_, err := parse(commands(), []string{"task", "list", "--status", "in-progress"})
	require.Error(t, err)
	require.Contains(t, err.Error(), "valid values are open, in_progress, done, dropped")

	p, err := parse(commands(), []string{"task", "list", "--status", "in_progress"})
	require.NoError(t, err)
	require.Equal(t, "in_progress", *p.opts.(*taskListOpts).status)
}

// The permuting parser means the id may come before or after the flags. The old
// requireFlagsFirst rejected the trailing-flag order outright.
func TestTaskParse_FlagsAndIdInEitherOrder(t *testing.T) {
	for _, argv := range [][]string{
		{"task", "claim", "--lease", "60", "01AAA"},
		{"task", "claim", "01AAA", "--lease", "60"},
	} {
		p, err := parse(commands(), argv)
		require.NoError(t, err, "%v", argv)
		require.Equal(t, []string{"01AAA"}, p.pos)
		require.Equal(t, 60, *p.opts.(*taskClaimOpts).lease)
	}
}

// A bare family name is not a command, but it is not a typo either: the caller
// knows what they want and needs the word for it. `seam task` used to list every
// task, which is a different command's job (`seam task list`).
func TestDispatch_BareFamilyNameListsItsSubcommands(t *testing.T) {
	e, _, errb := stubEnv()
	require.Equal(t, 2, dispatch(context.Background(), e, []string{"task"}))
	require.Contains(t, errb.String(), `unknown command "task": valid values are task list, task add`)

	// A flag after the family name is not a subcommand attempt, so it is not
	// quoted back as one.
	e, _, errb = stubEnv()
	require.Equal(t, 2, dispatch(context.Background(), e, []string{"task", "--project", "p"}))
	require.Contains(t, errb.String(), `unknown command "task": valid values are`)

	// A real typo names what was actually typed.
	e, _, errb = stubEnv()
	require.Equal(t, 2, dispatch(context.Background(), e, []string{"task", "bogus"}))
	require.Contains(t, errb.String(), `unknown command "task bogus": valid values are task list`)
}

// task block and task unblock take the task id and at least one blocker, with
// --session on either side, and send the blockers as tasks_update's
// add_depends_on / remove_depends_on.
func TestTaskParse_BlockerVerbs(t *testing.T) {
	for _, sub := range []string{"block", "unblock"} {
		_, err := parse(commands(), []string{"task", sub, "01AAA"})
		require.Error(t, err, sub)
		require.Contains(t, err.Error(), "missing argument: seam task "+sub+" requires <id> <blocker>...")

		for _, argv := range [][]string{
			{"task", sub, "--session", "cc/x", "01AAA", "01BBB", "01CCC"},
			{"task", sub, "01AAA", "01BBB", "--session", "cc/x", "01CCC"},
		} {
			p, err := parse(commands(), argv)
			require.NoError(t, err, "%v", argv)
			require.Equal(t, []string{"01AAA", "01BBB", "01CCC"}, p.pos)
			require.Equal(t, "cc/x", *p.opts.(*taskActorOpts).session)
		}
	}
	require.Equal(t, "seam task unblock [--session NAME] <id> <blocker>...", synopsis(taskUnblockCmd))
	require.Equal(t, "seam task block [--session NAME] <id> <blocker>...", synopsis(taskBlockCmd))
}

const (
	task      = "01KDDDDDDDDDDDDDDDDDDDDDDD"
	blocker   = "01KEEEEEEEEEEEEEEEEEEEEEEE"
	stranger  = "01KGGGGGGGGGGGGGGGGGGGGGGG"
	stranger2 = "01KHHHHHHHHHHHHHHHHHHHHHHH"
)

// What the verbs send and print, against the stand-in daemon: unblock names
// what it removed and what was not a blocker (in full: the ids share their
// leading characters), block names what the task now waits on, and a task
// whose answer is withheld prints only the update.
func TestTaskBlockers_SendAndPrint(t *testing.T) {
	fx := newDaemonFixture(t, map[string]toolFunc{
		"tasks_update": func(a map[string]any) toolReply {
			switch {
			case a["id"] == "01KWWWWWWWWWWWWWWWWWWWWWWW":
				return toolReply{text: `{"id":"01KWWWWWWWWWWWWWWWWWWWWWWW","project":"example-vault","status":"open",` +
					`"withheld":{"reason":"example","fields":["title"]}}`}
			case a["add_depends_on"] != nil:
				return toolReply{text: `{"id":"` + task + `","status":"open","depends_on":["` + blocker + `"]}`}
			case len(a["remove_depends_on"].([]any)) == 1:
				return toolReply{text: `{"id":"` + task + `","status":"open","removed_depends_on":["` + blocker + `"]}`}
			}
			return toolReply{text: `{"id":"` + task + `","status":"open","removed_depends_on":["` + blocker + `"],` +
				`"not_depends_on":["` + stranger + `","` + stranger2 + `"]}`}
		},
	}, nil, "")

	for _, tc := range []struct {
		name   string
		argv   []string
		param  string
		sent   []any
		stdout string
	}{
		{"unblock one", []string{"task", "unblock", task, blocker}, "remove_depends_on", []any{blocker},
			"task 01KDDDDD: removed blocker " + blocker + "\n"},
		{"unblock with non-blockers, comma form", []string{"task", "unblock", task, blocker + ", " + stranger, stranger2},
			"remove_depends_on", []any{blocker, stranger, stranger2},
			"task 01KDDDDD: removed blocker " + blocker + "\n" +
				"task 01KDDDDD: " + stranger + ", " + stranger2 + " are not blockers; nothing removed\n"},
		{"block", []string{"task", "block", task, blocker}, "add_depends_on", []any{blocker},
			"task 01KDDDDD now waits on " + blocker + "\n"},
		{"unblock withheld", []string{"task", "unblock", "01KWWWWWWWWWWWWWWWWWWWWWWW", blocker}, "remove_depends_on", []any{blocker},
			"task 01KWWWWW unblock -> open\n"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stdout, stderr, code := fx.run(t, tc.argv...)
			require.Equal(t, 0, code, "stderr: %s", stderr)
			require.Equal(t, tc.stdout, stdout)
			call := fx.lastCall(t)
			require.Equal(t, "tasks_update", call.name)
			require.Equal(t, tc.sent, call.args[tc.param])
			require.Equal(t, tc.argv[2], call.args["id"])
		})
	}

	n := fx.callCount()
	stdout, stderr, code := fx.run(t, "task", "unblock", task, " , ")
	require.Equal(t, 1, code)
	require.Empty(t, stdout)
	require.Contains(t, stderr, "seam task unblock: no blocker id given")
	require.Equal(t, n, fx.callCount(), "nothing was sent")
}
