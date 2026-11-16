package main

// seam lock check -- the repo-lock git hooks' check (plan repo-locks, RL4). git
// runs it from two hooks: reference-transaction, for every ref update in this
// checkout (a commit, a cherry-pick, `merge --ff-only`, a rebase, a reset, a
// tag, update-ref plumbing), and pre-push, for the refs a push updates on the
// remote. It reads the updates on stdin and refuses, with exit code 77, an
// update that moves a ref one of the repository's locks guards unless the
// caller may move it:
//
//   - an agent session (one the daemon binds this connection to through the
//     agent process that ran git, directly or through a program of its own
//     such as node or make: lcList) only while it holds the lock;
//   - any other caller, such as the owner at a terminal, unless another session
//     holds the lock.
//
// Everything that is not a refusal allows: an update no lock could guard, the
// feature off on the server, no lock declared, and every failure to find out
// (the daemon down, a checkout it cannot name). A broken checker must never
// wedge git, so those warn on stderr and exit 0.
//
// The fast path decides without the daemon which updates could move a guarded
// ref (lcCandidate); a transaction with none, such as a commit on a detached
// HEAD or a fetch's remote-tracking refs, exits before dialing.

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"slices"
	"strings"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"

	"github.com/arctop/seamless/internal/agentproc"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/gitread"
)

// The two git hooks seam lock check serves.
const (
	lcHookRefTx   = "reference-transaction"
	lcHookPrePush = "pre-push"
)

// lcHooks lists them, for the --hook flag.
var lcHooks = []string{lcHookRefTx, lcHookPrePush}

// lcExitRefused is the exit code of a refusal, and the only code the hook
// scripts refuse on (sysexits' EX_NOPERM). Every other code -- 1 for a failure,
// 2 for a seam that has no `lock check`, 127 for no seam at all -- warns and
// allows, so an older or missing seam never blocks a commit.
const lcExitRefused = 77

// lcDaemonTimeout bounds the daemon conversation: git waits on the hook, and a
// daemon that does not answer in time is a daemon that is down.
const lcDaemonTimeout = 10 * time.Second

// lcPrepared is the reference-transaction state that checks. git also runs the
// hook while a transaction is preparing, once it is committed and once it is
// aborted; the first is before the refs are locked, and the exit code of the
// last two is ignored.
const lcPrepared = "prepared"

var lockCheckCmd = spec("lock check", groupLocks,
	"the repo-lock git hooks' check: refuse an update to a guarded ref this caller may not move",
	between(0, 2, "hook-arg"), bindLockCheck, runLockCheck).withLong(lockCheckLong)

const lockCheckLong = `Run by the reference-transaction and pre-push git hooks, not by hand: it
reads the hook's ref updates on stdin, as git sends them, and the hook's
arguments after --. It exits 77 to refuse an update that moves a ref one of
this repository's locks guards, unless this caller may move it: an agent
session only while it holds the lock, any other caller unless another session
holds it. Every other outcome exits 0, a failure to reach the daemon with a
warning. See the coordinate-agents guide for the hook scripts.`

type lockCheckOpts struct {
	hook *string
	cwd  *string
}

func bindLockCheck(fs *flag.FlagSet) *lockCheckOpts {
	return &lockCheckOpts{
		hook: enumFlag(fs, "hook", "", "the git `HOOK` running the check: reference-transaction or pre-push", lcHooks),
		cwd:  fs.String("cwd", "", "the checkout `DIR` (default: the working directory, where git runs its hooks)"),
	}
}

// lcUpdate is one ref update a hook was handed: the ref, its old value and
// the value it is to take, each an object name, all zeros for none, or (for a
// symbolic ref) "ref:<target>".
type lcUpdate struct {
	ref, old, new string
}

// lcRefusedError is a refusal: dispatch exits lcExitRefused for it.
type lcRefusedError struct{ msg string }

func (e *lcRefusedError) Error() string     { return e.msg }
func (e *lcRefusedError) seamExitCode() int { return lcExitRefused }

// lcExitCoder is the interface dispatch asks an error for its own exit code
// through. Its method is unexported, so no error from outside this package
// (an *exec.ExitError, say) can carry a code by accident.
type lcExitCoder interface{ seamExitCode() int }

// The refusal is one: dispatch exits lcExitRefused for it.
var _ = lcExitCoder((*lcRefusedError)(nil))

// lcHookGit names the git subcommand of the git process that ran this hook;
// a variable so a test can stand in for the process table.
var lcHookGit = lcGitCommand

// lcAgentChain names the processes this check may ask as, nearest first
// (agentproc.Chain); a variable so a test can stand in for the process table.
var lcAgentChain = agentproc.Chain

func runLockCheck(ctx context.Context, e *env, o *lockCheckOpts, pos []string) error {
	hook := *o.hook
	if hook == "" {
		return errors.New("pass --hook reference-transaction or --hook pre-push")
	}
	if hook == lcHookRefTx && len(pos) > 0 && pos[0] != lcPrepared {
		return nil // only "prepared" checks; stdin is left unread
	}
	updates, err := lcReadUpdates(e.stdin, hook)
	if err != nil {
		return err
	}
	dir := *o.cwd
	if dir == "" {
		if dir, err = os.Getwd(); err != nil {
			return err
		}
	}
	root := gitread.RepoRoot(dir)
	candidates := lcCandidates(updates, hook, root)
	if len(candidates) > 0 && hook == lcHookRefTx {
		candidates = lcExempt(candidates, updates)
	}
	if len(candidates) == 0 {
		return nil
	}
	remote := ""
	if hook == lcHookPrePush && len(pos) > 0 {
		remote = pos[0]
	}
	return lcCheck(ctx, e, hook, remote, dir, candidates)
}

// lcReadUpdates parses the hook's stdin. reference-transaction sends
// "<old> <new> <ref>" lines; pre-push sends "<local ref> <local oid> <remote
// ref> <remote oid>", whose remote ref is the one a push moves, from the
// remote oid to the local one. A line of another shape is skipped.
func lcReadUpdates(r io.Reader, hook string) ([]lcUpdate, error) {
	var out []lcUpdate
	sc := bufio.NewScanner(r)
	sc.Buffer(make([]byte, 0, 64<<10), 1<<20)
	for sc.Scan() {
		f := strings.Fields(sc.Text())
		switch {
		case hook == lcHookRefTx && len(f) == 3:
			out = append(out, lcUpdate{ref: f[2], old: f[0], new: f[1]})
		case hook == lcHookPrePush && len(f) == 4:
			out = append(out, lcUpdate{ref: f[2], old: f[3], new: f[1]})
		}
	}
	if err := sc.Err(); err != nil {
		return nil, fmt.Errorf("read the %s hook's ref updates: %w", hook, err)
	}
	return out, nil
}

// lcIgnoredRefs are the namespaces under refs/ no lock is checked in: refs a
// fetch or the remote moves (refs/remotes/), git's own per-worktree working
// refs (bisect, rebase -r's labels, refs/worktree/), and maintenance's
// prefetch copies. A lock may name a ref there, but no hook checks it: a
// fetch must never be refused, and the rest are private to one checkout.
var lcIgnoredRefs = []string{"refs/remotes/", "refs/bisect/", "refs/rewritten/", "refs/worktree/", "refs/prefetch/"}

// lcCandidate reports whether an update to ref could move a ref a lock
// guards, decided without the daemon. Every guarded ref is a full name under
// refs/ (core.ValidRepoLockRef), so HEAD, ORIG_HEAD, AUTO_MERGE, FETCH_HEAD and
// a worktree's own HEAD never are; nor are lcIgnoredRefs and the stash. Every
// other ref under refs/ is: a branch, a tag, a note.
func lcCandidate(ref string) bool {
	if !strings.HasPrefix(ref, "refs/") || ref == "refs/stash" {
		return false
	}
	return !slices.ContainsFunc(lcIgnoredRefs, func(p string) bool { return strings.HasPrefix(ref, p) })
}

// lcZero reports an all-zeros object name: no value.
func lcZero(oid string) bool {
	return oid != "" && strings.Trim(oid, "0") == ""
}

// lcCandidates keeps the updates that could move a guarded ref. An update
// that leaves its ref where it is moves nothing: in a push, a remote ref that
// already holds the local value; in a transaction, a ref whose new value is
// the one it holds now (git rewriting a ref into packed-refs, or a no-op
// update-ref). root is the checkout's root, for reading that value.
func lcCandidates(updates []lcUpdate, hook, root string) []lcUpdate {
	var out []lcUpdate
	for _, u := range updates {
		if !lcCandidate(u.ref) {
			continue
		}
		switch {
		case hook == lcHookPrePush && u.new == u.old:
			continue
		case hook == lcHookRefTx && !lcZero(u.new) && root != "" && u.new == gitread.RefValue(root, u.ref):
			continue
		}
		out = append(out, u)
	}
	return out
}

// lcExemptCommands are the git commands whose ref transactions are never
// checked: a clone brings the remote's refs here, and pack-refs, gc and
// maintenance rewrite where refs are stored without moving them (pack-refs
// writes each ref into packed-refs, then deletes its loose file).
var lcExemptCommands = []string{"clone", "pack-refs", "gc", "maintenance"}

// lcExempt drops the updates of a transaction git runs for a command that
// lands nothing. A fetch is checked only for the refs/heads/ and other refs it
// moves by an explicit refspec (`git fetch origin main:main` moves the local
// branch); the tags it brings are the remote's, already landed. When the git
// process cannot be read (another OS, or a failed read), a transaction that
// updates a remote-tracking ref is taken for a fetch.
func lcExempt(candidates, all []lcUpdate) []lcUpdate {
	cmd, known := lcHookGit()
	fetch := cmd == "fetch"
	if known && slices.Contains(lcExemptCommands, cmd) {
		return nil
	}
	if !known {
		fetch = slices.ContainsFunc(all, func(u lcUpdate) bool { return strings.HasPrefix(u.ref, "refs/remotes/") })
	}
	if !fetch {
		return candidates
	}
	return slices.DeleteFunc(candidates, func(u lcUpdate) bool { return strings.HasPrefix(u.ref, "refs/tags/") })
}

// lcGuards reports whether the guarded ref pattern, a full ref name or a
// pattern with one '*' (core.ValidRepoLockRef), covers ref. The '*' matches any
// run of characters, '/' included, as in a refspec.
func lcGuards(pattern, ref string) bool {
	before, after, star := strings.Cut(pattern, "*")
	if !star {
		return pattern == ref
	}
	return len(ref) >= len(before)+len(after) && strings.HasPrefix(ref, before) && strings.HasSuffix(ref, after)
}

// lcAllowed is the rule: the holder passes; an agent session that does not
// hold the lock is refused; any other caller is refused only while another
// session holds it. agent is the daemon's caller_bound; a missing
// held_by_caller reads as false.
func lcAllowed(l map[string]any, agent bool) bool {
	if l["held_by_caller"] == true {
		return true
	}
	return !agent && l["held"] != true
}

// lcCheck asks the daemon for this repository's locks and refuses the updates
// the caller may not make. remote is the remote a push goes to, as git names
// it to pre-push ("" for a transaction).
func lcCheck(ctx context.Context, e *env, hook, remote, dir string, candidates []lcUpdate) error {
	checkout, err := lkCheckout(dir)
	if err != nil {
		lcWarn(e, "%v", err)
		return nil
	}
	ctx, cancel := context.WithTimeout(ctx, lcDaemonTimeout)
	defer cancel()
	cli, _, err := e.dial(ctx)
	if err != nil {
		lcWarn(e, "seamlessd is unreachable (%v)", err)
		return nil
	}
	defer func() { _ = cli.Close() }()
	out, err := lcList(ctx, cli, checkout, lcAgentChain())
	switch {
	case errors.Is(err, errLkNotServed):
		return nil // repo locks are off on this server: nothing to check
	case err != nil:
		lcWarn(e, "locks_list failed (%v)", err)
		return nil
	}
	agent := out["caller_bound"] == true
	locks, _ := out["locks"].([]any)
	var refusals []string
	for _, item := range locks {
		l, _ := item.(map[string]any)
		if l == nil || str(l["state"]) != string(core.RepoLockActive) {
			continue // a declaration waiting for the owner guards nothing yet
		}
		var moved []string
		for _, u := range candidates {
			if !slices.Contains(moved, u.ref) && slices.ContainsFunc(strs(l["refs"]), func(p string) bool { return lcGuards(p, u.ref) }) {
				moved = append(moved, u.ref)
			}
		}
		if len(moved) > 0 && !lcAllowed(l, agent) {
			refusals = append(refusals, lcRefusal(hook, remote, l, moved))
		}
	}
	if len(refusals) == 0 {
		return nil
	}
	return &lcRefusedError{msg: strings.Join(refusals, "\n"+hintIndent) + "\n" + hintIndent + lcOverride}
}

// lcList asks for the checkout's locks as the agent that ran git. The nearest
// process above git that is not a shell is that agent when it ran git itself,
// but a program of its own when it ran git through one -- fanout-land's node,
// make, python -- and such a program binds no session: the answer would call
// the caller unbound, so the agent's own hold would read as another session's
// and refuse it, while a free lock let it through. So while an answer says the
// caller is bound to no session, lcList asks again as the next process up the
// chain, and keeps the first answer that binds. The daemon binds only a
// process a session-start hook stamped, so the walk can reach only an agent
// git runs under. A chain that binds nowhere (a terminal) keeps the nearest's
// answer, as does a server too old to say (no caller_bound).
//
// chain is agentproc.Chain's answer; with none, the connection's own identity
// (mcpHeaders) asks once.
func lcList(ctx context.Context, cli *mcpclient.Client, checkout map[string]any, chain []string) (map[string]any, error) {
	if len(chain) == 0 {
		out, _, err := lkCall(ctx, cli, "locks_list", checkout)
		return out, err
	}
	var nearest map[string]any
	for _, proc := range chain {
		out, _, err := lkCall(withAgentProcess(ctx, proc), cli, "locks_list", checkout)
		if err != nil {
			return nil, err
		}
		if bound, said := out["caller_bound"]; bound == true || !said {
			return out, nil
		}
		if nearest == nil {
			nearest = out
		}
	}
	return nearest, nil
}

// lcOverride closes every refusal: what does not skip the check, and the
// owner's way past it.
const lcOverride = "--no-verify does not skip the reference-transaction hook, which runs this check on every ref update. " +
	"The owner's override is a force-release: seam lock release --force, or the console's Locks page."

// lcRefusal says why one lock refuses: what moves, who holds the lock and
// since and until when (a holder only as the daemon lets this caller see it),
// and the remedy. A free lock refuses only an agent session (lcAllowed).
func lcRefusal(hook, remote string, l map[string]any, refs []string) string {
	what := "this update moves " + strings.Join(refs, ", ")
	if hook == lcHookPrePush {
		if remote == "" {
			remote = "the remote"
		}
		what = "this push moves " + strings.Join(refs, ", ") + " on " + remote
	}
	head := fmt.Sprintf("refused: %s, which LOCK %s guards", what, str(l["label"]))
	if l["held"] == true {
		who := "another session"
		if s := str(l["holder_session"]); s != "" {
			who = "session " + s
		}
		return fmt.Sprintf("%s, and %s holds the lock%s.\n%sWait for its release, or queue for the lock: seam lock acquire --wait",
			head, who, lcSpan(l), hintIndent)
	}
	state := "the lock is free"
	if l["lapsed"] == true {
		state = "the lock is free (its last hold lapsed" + lcAt(" at ", str(l["until"])) + ")"
	}
	return fmt.Sprintf("%s, %s, and this session does not hold it: an agent session moves a guarded ref only "+
		"while it holds the lock.\n%sTake it first: seam lock acquire (--wait queues behind a holder)", head, state, hintIndent)
}

// lcSpan renders a hold's since and until: " (since S, until U)".
func lcSpan(l map[string]any) string {
	var parts []string
	if s := lkWhen(str(l["since"])); s != "" {
		parts = append(parts, "since "+s)
	}
	if u := lkWhen(str(l["until"])); u != "" {
		parts = append(parts, "until "+u)
	}
	if len(parts) == 0 {
		return ""
	}
	return " (" + strings.Join(parts, ", ") + ")"
}

// lcAt renders prefix and a timestamp, or "" for none.
func lcAt(prefix, ts string) string {
	if w := lkWhen(ts); w != "" {
		return prefix + w
	}
	return ""
}

// lcWarn prints a warning that the check could not run, and that the update
// goes ahead.
func lcWarn(e *env, format string, args ...any) {
	fmt.Fprintf(e.stderr, "seam lock check: warning: "+format+"; the repository's locks were not checked, so the update goes ahead\n", args...)
}

// lcBase is a process name or path's last element, lowercased, without a
// login shell's leading '-' or Windows' ".exe".
func lcBase(name string) string {
	n := strings.ToLower(strings.TrimSpace(name))
	if i := strings.LastIndexAny(n, `/\`); i >= 0 {
		n = n[i+1:]
	}
	n = strings.TrimPrefix(n, "-")
	return strings.TrimSuffix(n, ".exe")
}

// lcShells are the shells git runs a hook through: a script's #!/bin/sh, and
// the sh -c of a config-defined hook.
var lcShells = []string{"sh", "bash", "dash", "zsh", "ksh", "mksh", "ash", "busybox"}

// lcMaxHops bounds the walk from this process to the git that ran the hook: a
// shell or two sit between them.
const lcMaxHops = 6

// lcGitCommand names the git subcommand ("commit", "fetch") of the git process
// that ran this hook: the nearest ancestor that is not a shell, when it is git.
// known is false when the process table could not be read (an OS this reads no
// process table on, or a failed read); "" with known true is a hook git did
// not run, such as a check run by hand.
func lcGitCommand() (cmd string, known bool) {
	pid := os.Getppid()
	for range lcMaxHops {
		if pid <= 1 {
			return "", true
		}
		ppid, name, err := lcProcLookup(pid)
		if err != nil {
			return "", false
		}
		switch base := lcBase(name); {
		case slices.Contains(lcShells, base):
			pid = ppid
		case base == "git" || strings.HasPrefix(base, "git-"):
			argv, err := lcProcArgs(pid)
			if err != nil {
				return "", false
			}
			return lcSubcommand(argv), true
		default:
			return "", true
		}
	}
	return "", true
}

// lcValueOptions are git's options before the subcommand that take their
// value as the next word.
var lcValueOptions = []string{"-C", "-c", "--git-dir", "--work-tree", "--namespace", "--super-prefix", "--config-env", "--attr-source"}

// lcSubcommand reads git's subcommand from its argv: the first word after the
// options git takes before it, or the dashed form's suffix (git-upload-pack).
func lcSubcommand(argv []string) string {
	if len(argv) == 0 {
		return ""
	}
	if base := lcBase(filepath.ToSlash(argv[0])); strings.HasPrefix(base, "git-") {
		return strings.TrimPrefix(base, "git-")
	}
	for i := 1; i < len(argv); i++ {
		a := argv[i]
		switch {
		case !strings.HasPrefix(a, "-"):
			return a
		case slices.Contains(lcValueOptions, a):
			i++
		}
	}
	return ""
}
