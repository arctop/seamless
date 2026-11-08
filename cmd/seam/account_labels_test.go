package main

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// A restricted local account's items say so in the CLI's text rendering, in
// the wording every surface uses; the owner's render exactly as before (the
// golden cases pin that byte for byte).

const (
	fxAccountMemory = `{"id":"01KAAAAAAAAAAAAAAAAAAAAAAA","kind":"gotcha","name":"lease-steal-window","description":"a claim can be stolen",` +
		`"project":"example-project","body":"Wait for expiry.\n","tags":[],"source_session":"","content_hash":"abc",` +
		`"account":"_example_agent"}`
	fxAccountNote = `{"id":"01KCCCCCCCCCCCCCCCCCCCCCCC","title":"Lease design","slug":"lease-design","description":"how leases work",` +
		`"project":"example-project","body":"Leases expire.","tags":[],"source_url":"","content_hash":"def","account":"_example_agent"}`
	fxAccountSession = `{"session":{"id":"01KHHHHHHHHHHHHHHHHHHHHHHH","name":"cc/quiet-otter","projectSlug":"example-project",` +
		`"status":"active","account":"_example_agent"},"findings":"","toolCalls":1,"memoryReads":0,"memoryWrites":0,` +
		`"injectedItems":0,"readAfterInject":0,"events":[]}`
)

func TestAccountLabels_TextRendering(t *testing.T) {
	fx := newDaemonFixture(t, map[string]toolFunc{
		"memory_read": fixedReply(fxAccountMemory),
		"notes_read":  fixedReply(fxAccountNote),
		"tasks_list": fixedReply(`{"tasks":[{"id":"01KDDDDDDDDDDDDDDDDDDDDDDD","title":"write the migration","status":"open",` +
			`"account":"_example_agent"},{"id":"01KGGGGGGGGGGGGGGGGGGGGGGG","title":"owner idea","status":"open"}]}`),
	}, map[string]string{
		"GET /console/sessions/01KHHHHHHHHHHHHHHHHHHHHHHH?format=json": fxAccountSession,
	}, fxHealthz)

	out, errb, code := fx.run(t, "memory", "read", "lease-steal-window")
	require.Equal(t, 0, code, errb)
	require.Contains(t, out, "provenance:  written by account _example_agent\n")

	out, errb, code = fx.run(t, "note", "read", "lease-design")
	require.Equal(t, 0, code, errb)
	require.Contains(t, out, "provenance:  written by account _example_agent\n")

	out, errb, code = fx.run(t, "task", "list")
	require.Equal(t, 0, code, errb)
	require.Equal(t, "  01KDDDDD  [open       ] write the migration  [written by account _example_agent]\n"+
		"  01KGGGGG  [open       ] owner idea\n", out)

	out, errb, code = fx.run(t, "sessions", "01KHHHHHHHHHHHHHHHHHHHHHHH")
	require.Equal(t, 0, code, errb)
	require.Equal(t, "cc/quiet-otter  [active]  example-project\nwritten by account _example_agent\n", firstLines(out, 2))
}

func TestWrittenBy(t *testing.T) {
	require.Empty(t, writtenBy(""), "the owner's items carry no label")
	require.Equal(t, "written by account _example_agent", writtenBy("_example_agent"))
	require.Equal(t, `written by account a\x1b[2Jb`, writtenBy("a\x1b[2Jb"), "a terminal escape is spelled out")
}

func firstLines(s string, n int) string {
	lines := strings.SplitAfter(s, "\n")
	if len(lines) > n {
		lines = lines[:n]
	}
	return strings.Join(lines, "")
}
