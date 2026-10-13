package hooks

import (
	"fmt"
	"time"
)

// anyHTTPSBaseURL stands for every https base URL: profileForBaseURL reads only
// the scheme, so one https URL selects the same profile as any other.
const anyHTTPSBaseURL = "https://localhost"

// CommandHookTimeouts returns the timeout the client's profile gives each
// `seam hook <arg>` command hook it can wire, keyed by the arg, on either
// transport: an https base URL makes every Claude Code hook a command hook
// (profileForBaseURL), so Claude Code's user-prompt-submit is here although an
// http install wires it as an http hook. An arg wired with two timeouts keeps
// the shorter one; today the transport changes a hook's type, never its
// timeout.
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
	out := make(map[string]time.Duration)
	for _, shape := range [][]hookSpec{profile, profileForBaseURL(profile, anyHTTPSBaseURL)} {
		for _, hs := range shape {
			if hs.CLIArg == "" {
				continue // an http hook: the client makes the request itself
			}
			timeout := time.Duration(hs.Timeout) * time.Second
			if prev, ok := out[hs.CLIArg]; !ok || timeout < prev {
				out[hs.CLIArg] = timeout
			}
		}
	}
	return out, nil
}
