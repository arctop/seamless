package hooks

import (
	"fmt"
	"slices"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
)

// The model sniff runs at a session's start, on every prompt and at every
// Stop. A conversation on another machine records its skipped sniff once,
// however many prompts and turns follow, and the next conversation records
// its own.
func TestSetAmbientModel_ARemoteConversationRecordsItsSkippedSniffOnce(t *testing.T) {
	e := newHostEnv(t, config.PlanCapture{})
	remote := map[string]string{"host": remoteHostName, "repo_root": "/srv/app", "main_root": "/srv/app"}
	sniffs := func() int {
		return len(slices.DeleteFunc(e.skippedWhat(t), func(w string) bool { return w != "model-sniff" }))
	}

	e.postFrom(t, "session-start", remote, map[string]any{"session_id": "remote-9", "cwd": "/srv/app", "source": "startup"})
	require.Equal(t, 1, sniffs(), "the first skip is recorded")
	for _, prompt := range []string{"why does the deploy fail", "and the staging gate"} {
		e.postFrom(t, "user-prompt-submit", remote, map[string]any{
			"session_id": "remote-9", "cwd": "/srv/app", "hook_event_name": "UserPromptSubmit", "prompt": prompt,
		})
		e.postFrom(t, "stop", remote, map[string]any{
			"session_id": "remote-9", "cwd": "/srv/app", "last_assistant_message": "",
		})
	}
	require.Equal(t, 1, sniffs(), "the conversation's prompts and turns record no more")

	e.postFrom(t, "session-start", remote, map[string]any{"session_id": "remote-10", "cwd": "/srv/app", "source": "startup"})
	require.Equal(t, 2, sniffs(), "another conversation records its own")
}

// A local conversation never records a skip, at its start, on its prompts or
// at a Stop.
func TestSetAmbientModel_ALocalConversationRecordsNoSkip(t *testing.T) {
	e := newHostEnv(t, config.PlanCapture{})
	local := map[string]string{"host": localHostName}
	e.postFrom(t, "session-start", local, map[string]any{"session_id": "local-9", "cwd": "/work/demo", "source": "startup"})
	e.postFrom(t, "user-prompt-submit", local, map[string]any{
		"session_id": "local-9", "cwd": "/work/demo", "hook_event_name": "UserPromptSubmit", "prompt": "why does the deploy fail",
	})
	e.postFrom(t, "stop", local, map[string]any{"session_id": "local-9", "cwd": "/work/demo", "last_assistant_message": ""})
	require.Empty(t, e.skipEvents(t))
}

// The set of remembered conversations is bounded: a new key that finds it
// full starts it over, so it never holds more than sniffSkipsMax, and a key
// is new again only after such a reset.
func TestOnceSet_IsBounded(t *testing.T) {
	var o onceSet
	require.True(t, o.first("a"))
	require.False(t, o.first("a"), "a key is recorded once")
	for i := 1; i < sniffSkipsMax; i++ {
		require.True(t, o.first(fmt.Sprintf("k%d", i)))
	}
	require.Len(t, o.seen, sniffSkipsMax)
	require.False(t, o.first("a"), "a full set still remembers its keys")

	require.True(t, o.first("one-more"), "a new key past the bound is new")
	require.Len(t, o.seen, 1, "and the set started over with it")
	require.True(t, o.first("a"), "a key from before the reset is new again: one more record, never one a prompt")
}
