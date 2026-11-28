package hooks

import (
	"fmt"
	"time"
)

// CommandHookTimeouts returns the timeout the client's profile gives each
// `seam hook <arg>` command hook it wires, keyed by the arg: every hook of the
// profile, on every transport.
//
// It exists for the seam CLI's test, like CommandHookEndpoints. The CLI sets
// how long each hook re-dials a daemon it cannot dial, per event and client
// (cmd/seam/hook.go hookEvents and hookClientDialBudgets), and when it gives
// up altogether (each event's deadline), and cannot import this package to
// read the timeouts both must fit inside. So its tests pin each client's
// budget at or under half of that client's timeout here, and each deadline at
// least hookDeadlineMargin under it: a hook whose daemon cannot be reached,
// whether its dials are refused or hang, then fails open on its own before its
// client kills it.
func CommandHookTimeouts(client Client) (map[string]time.Duration, error) {
	_, profile, err := resolveHookProfile(client)
	if err != nil {
		return nil, fmt.Errorf("hooks.CommandHookTimeouts: %w", err)
	}
	out := make(map[string]time.Duration, len(profile))
	for _, hs := range profile {
		out[hs.CLIArg] = time.Duration(hs.Timeout) * time.Second
	}
	return out, nil
}
