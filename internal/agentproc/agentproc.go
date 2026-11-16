// Package agentproc names the agent process -- the Claude Code or Codex process
// -- that launched the current seam process.
//
// Seamless hears about one agent session on two channels that share nothing on
// the wire. The SessionStart hook creates the ambient session and knows the
// client's own session id; the MCP connection carries the agent's tool calls and
// knows only a server-minted transport id that encodes no client identity (memory
// mcp-session-id-opaque-and-restart-unstable). What the two DO share is their
// parent: the agent process runs `seam hook` for the hook, and `seam mcp-proxy`
// (stdio) or `seam mcp-headers` (the HTTP headersHelper) for the transport. So
// each of those seam processes can name the agent that launched it, and the
// daemon joins the channels on that name -- a tool call whose connection names
// the process a session-start hook named belongs to that hook's session, with no
// session_start call and no guess.
//
// The name is the nearest ancestor that is not a shell, because a client may run
// its helpers through one: Claude Code runs headersHelper with `sh -c`, while its
// exec-form hooks and stdio servers are its direct children. git is passed over
// too, since it runs `seam lock check` from its hooks on the agent's behalf. The
// name carries the process's start time as well as its pid, so a reused pid can
// never be mistaken for the agent that used to own it.
//
// Chain goes on past that nearest ancestor, for the one caller that may: the
// repo-lock check, whose git an agent may have run through a program of its own
// (node, make, python), which is then the nearest ancestor and names no agent.
//
// Every failure reports false and the caller sends nothing: the daemon then
// resolves the call exactly as it did before this package existed.
package agentproc

import (
	"errors"
	"os"
	"regexp"
	"strconv"
	"strings"
)

// maxDepth bounds the ancestor walk. A client puts at most a shell or two between
// itself and a helper it runs; a walk that climbs further has left the client and
// is reading the user's terminal, which no hook shares.
const maxDepth = 6

// chainDepth bounds Chain's walk, which climbs on past the first non-shell
// ancestor: an agent that runs git through a wrapper puts the wrapper and its
// own shells between itself and git (make runs a recipe through sh, a script
// runs node), so the agent can sit further up than maxDepth reaches. Past the
// agent the walk soon reaches the terminal's owner and then pid 1.
const chainDepth = 16

// errNoStart is a process whose start time could not be read. Without it the
// identity could outlive the process and be inherited by an unrelated one that
// reuses the pid, so the walk refuses it rather than emitting a weaker name.
var errNoStart = errors.New("agentproc: process start time unavailable")

// errNoProcess is a pid the process table does not list (it exited mid-walk).
var errNoProcess = errors.New("agentproc: no such process")

// procInfo is what the walk needs to know about one process.
type procInfo struct {
	ppid  int
	name  string // executable name as the OS reports it (no directory)
	start uint64 // start time in the platform's own unit; compared, never interpreted
}

// Anchor returns the identity of the agent process that launched this one, or
// false when it cannot be determined: an unsupported OS, a process table the
// walk could not read, or no non-shell ancestor within reach.
func Anchor() (string, bool) {
	look, err := newLookup()
	if err != nil {
		return "", false
	}
	return anchorFrom(os.Getpid(), look)
}

// anchorFrom names the nearest ancestor of self that is neither a shell nor
// git (isShell, isGit), within maxDepth.
func anchorFrom(self int, look func(int) (procInfo, error)) (string, bool) {
	ids := walk(self, look, maxDepth, 1)
	if len(ids) == 0 {
		return "", false
	}
	return ids[0], true
}

// Chain returns the identities of self's ancestors that are neither a shell nor
// git, nearest first, within chainDepth: Anchor's answer first, whenever it has
// one, then each such process above it. nil when the process table cannot be
// read.
//
// It serves `seam lock check` alone. An agent may run git through a program of
// its own -- node, make, python -- and that program is then the nearest anchor,
// which no session-start hook stamped, so the check's connection binds nothing
// and the agent's own lock reads as someone else's. The check asks again as
// each process further up and keeps the first the daemon binds. That is sound
// for a check: the daemon binds only a process a session-start hook stamped, so
// the walk can reach only an agent that git runs under, never a sibling. It is
// not sound for a connection that lives on (an MCP server, a hook's session):
// that is why Anchor stops at the first wrapper (see shells).
func Chain() []string {
	look, err := newLookup()
	if err != nil {
		return nil
	}
	return walk(os.Getpid(), look, chainDepth, 0)
}

// walk climbs from self through at most depth ancestors and collects, nearest
// first, the identity of each that is neither a shell nor git, stopping once it
// holds limit of them (0: no limit). A process it cannot read ends the walk with
// what it has, as does a parent that started after its child.
//
// That start-time check costs nothing on Unix, where it always holds; on
// Windows the recorded parent id outlives the parent, so a parent that exited
// and had its pid reused would otherwise send the walk into an unrelated
// process.
func walk(self int, look func(int) (procInfo, error), depth, limit int) []string {
	cur, err := look(self)
	if err != nil {
		return nil
	}
	var ids []string
	pid := cur.ppid
	for range depth {
		if pid <= 1 {
			break
		}
		info, err := look(pid)
		if err != nil || info.start > cur.start {
			break
		}
		if !isShell(info.name) && !isGit(info.name) {
			ids = append(ids, format(pid, info.start))
			if len(ids) == limit {
				break
			}
		}
		cur, pid = info, info.ppid
	}
	return ids
}

// shells are the interpreters a client may put between itself and a helper.
// Nothing else is skipped but git (isGit), which runs the repo-lock hooks: a
// wrapper that is not a shell (the Claude desktop app's `disclaimer`, say) is
// an anchor of its own, which can only ever fail to match -- skipping it would
// pin every MCP server that app spawns to the app itself, and one session's
// hook would then claim all of them. git is different because it never sits
// above an agent, only below one.
var shells = map[string]bool{
	"sh": true, "bash": true, "zsh": true, "dash": true, "ksh": true, "mksh": true,
	"ash": true, "fish": true, "csh": true, "tcsh": true, "busybox": true, "nu": true,
	"cmd": true, "powershell": true, "pwsh": true,
}

// isShell reports whether an executable name is a shell. It tolerates the forms
// the platforms report: a path, a login shell's leading dash, Windows' .exe
// suffix, and any letter case.
func isShell(name string) bool {
	return shells[baseName(name)]
}

// isGit reports whether an executable name is git, or one of its dashed
// programs (git-receive-pack; Linux truncates a name to 15 bytes, which keeps
// the prefix). The walk passes over git as it passes over a shell: git runs
// its hooks -- and its aliases, `rebase --exec` and `bisect run` -- as its own
// children, so `seam lock check` in a reference-transaction hook sits below
// git (seam <- sh <- git <- zsh <- claude) and must name the agent above it,
// the one the session-start hook stamped. Skipping git cannot pin anything to
// the wrong agent: git never hosts an agent session, so the walk can only
// reach the agent that ran git, or, at a terminal, whatever owns the terminal
// (tmux, login), which no session is stamped with and which binds nothing.
func isGit(name string) bool {
	n := baseName(name)
	return n == "git" || strings.HasPrefix(n, "git-")
}

// baseName is an executable name in the one form isShell and isGit compare:
// its last path element, lowercased, without a login shell's leading dash or
// Windows' .exe suffix.
func baseName(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	if i := strings.LastIndexAny(n, `/\`); i >= 0 {
		n = n[i+1:]
	}
	n = strings.TrimPrefix(n, "-")
	return strings.TrimSuffix(n, ".exe")
}

// format renders an identity as "<pid>.<start>".
func format(pid int, start uint64) string {
	return strconv.Itoa(pid) + "." + strconv.FormatUint(start, 10)
}

// tokenRe is the shape format produces.
var tokenRe = regexp.MustCompile(`^[1-9][0-9]{0,9}\.[1-9][0-9]{0,19}$`)

// Valid reports whether s is an identity Anchor could have produced. The daemon
// checks it before storing or matching one: the value crosses the wire as a
// header and a query parameter, and anything else is not an identity.
func Valid(s string) bool {
	return tokenRe.MatchString(s)
}
