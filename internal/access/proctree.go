package access

import (
	"errors"
	"fmt"
	"strconv"
	"strings"
)

// The agent's process tree (the 2026-11-06 design, Q3): a lease covers the
// agent process -- the Claude Code or Codex process agentproc.Anchor names and
// the SessionStart hook stamps on the session -- and every process it started.
// The MCP bridge, `seam` CLI calls, the hooks and the agent's own scripts share
// one approval; nothing outside the tree does.
//
// The daemon learns the tree from the kernel alone. At accept it reads which
// process opened the socket connection (ReadPeer: the audit token on macOS,
// SO_PEERCRED and a pidfd on Linux) and pins that pid to the process, so a
// later walk can tell it from one that reused the pid. Per request it walks the
// parent chain from there (Peer.Chain), verifying every link: a parent must
// have started no later than its child, and on macOS its kernel unique id must
// be the one the child records for its parent. A link that does not verify
// ends the chain, so a process whose chain no longer reaches the anchor -- an
// orphan reparented to launchd or init, say -- gets nothing. Nothing the client
// sends (the X-Seamless-Agent-Process header included) enters the chain.
//
// The residual risk is a debugger running as the same uid: it can attach to
// any process in the tree and act inside the approval. macOS limits
// task_for_pid through the hardened runtime and SIP, and Linux through yama's
// ptrace_scope (1 or higher); the guide states it.

// MaxChainHops bounds the walk. An agent sits a handful of processes above
// anything it runs; past this many hops the walk has left the agent's tree.
const MaxChainHops = 32

// ErrPeerGone is a socket peer whose pid no longer names the process that
// connected: it exited, and the pid may already belong to another process.
var ErrPeerGone = errors.New("access: the process that opened this connection is gone")

// errNoProcess is a pid the process table does not list.
var errNoProcess = errors.New("access: no such process")

// ChainHop is one process of a peer's chain: its token (pid, start time,
// boot) and the executable name the kernel reports, which is for display
// only and never part of an identity.
type ChainHop struct {
	Token ProcessToken
	Name  string
}

// Tokens returns the chain's tokens, the peer first.
func Tokens(hops []ChainHop) []ProcessToken {
	out := make([]ProcessToken, len(hops))
	for i, h := range hops {
		out[i] = h.Token
	}
	return out
}

// AgentID renders the token the way internal/agentproc renders an agent
// process identity ("<pid>.<start>", in the same per-platform start unit), so
// the approval core can compare a session's stamped agent process with a hop
// of the kernel's chain for equality. It is never parsed.
func (t ProcessToken) AgentID() string {
	return strconv.FormatInt(int64(t.PID), 10) + "." + strconv.FormatUint(t.Version, 10)
}

// procFacts is what the walk needs to know about one process.
type procFacts struct {
	ppid int32
	// start is the process start time in agentproc's unit: microseconds since
	// the epoch on macOS, clock ticks since boot on Linux. It is compared, never
	// interpreted.
	start uint64
	name  string
	// unique and parentUnique are macOS's per-boot unique ids of the process
	// and of the parent it records (proc_info); 0 where the kernel keeps none.
	unique, parentUnique uint64
	// pin is what the peer is matched against (Peer.pin): macOS's pidversion,
	// which the audit token also carries, or Linux's start time.
	pin uint64
}

// procLookup reads one process's facts. The platforms provide the real one;
// tests inject a table.
type procLookup func(pid int32) (procFacts, error)

// walkChain walks from pid up its parent chain. pin decides whether the
// process now at pid is the one that connected; a pid that no longer names it
// is ErrPeerGone, and the walk never starts. Every link above is verified
// before it is taken (see the file comment), and the first that does not
// verify -- or cannot be read -- ends the chain there: what lies above it is
// unproven, so it is left out. The walk stops below pid 1 (launchd, init) and
// at MaxChainHops.
func walkChain(pid int32, pin uint64, boot string, look procLookup) ([]ChainHop, error) {
	if pid <= 0 {
		return nil, fmt.Errorf("access: peer pid %d: %w", pid, errNoProcess)
	}
	cur, err := look(pid)
	if err != nil {
		return nil, fmt.Errorf("access: peer process %d: %w", pid, err)
	}
	if cur.start == 0 || cur.pin != pin {
		return nil, ErrPeerGone
	}
	hop := ChainHop{Token: ProcessToken{PID: pid, Version: cur.start, Boot: boot}, Name: cur.name}
	if !hop.Token.Valid() {
		return nil, fmt.Errorf("access: peer process %d has no valid token (boot %q)", pid, boot)
	}
	hops := []ChainHop{hop}
	for len(hops) < MaxChainHops && cur.ppid > 1 {
		parent, err := look(cur.ppid)
		if err != nil || parent.start == 0 || parent.start > cur.start {
			break
		}
		if cur.parentUnique != 0 && parent.unique != 0 && parent.unique != cur.parentUnique {
			break
		}
		hops = append(hops, ChainHop{Token: ProcessToken{PID: cur.ppid, Version: parent.start, Boot: boot}, Name: parent.name})
		cur = parent
	}
	return hops, nil
}

// parseProcStat reads the fields the walk needs from a Linux /proc/<pid>/stat
// line: "pid (comm) state ppid ... starttime ...", where starttime is field 22
// in clock ticks since boot. comm may itself hold spaces and parentheses, so
// the numbered fields are split from the LAST ')'. It is here rather than in
// the Linux file so its tests run everywhere.
func parseProcStat(line string) (procFacts, error) {
	open := strings.IndexByte(line, '(')
	end := strings.LastIndexByte(line, ')')
	if open < 0 || end < open {
		return procFacts{}, errMalformedStat
	}
	// fields[0] is field 3 (state), so field N sits at index N-3.
	fields := strings.Fields(line[end+1:])
	if len(fields) < 20 {
		return procFacts{}, errMalformedStat
	}
	ppid, err := strconv.ParseInt(fields[1], 10, 32)
	if err != nil {
		return procFacts{}, errMalformedStat
	}
	start, err := strconv.ParseUint(fields[19], 10, 64)
	if err != nil {
		return procFacts{}, errMalformedStat
	}
	return procFacts{ppid: int32(ppid), start: start, name: line[open+1 : end]}, nil
}

// errMalformedStat is a /proc/<pid>/stat line that does not parse.
var errMalformedStat = errors.New("access: malformed /proc stat line")
