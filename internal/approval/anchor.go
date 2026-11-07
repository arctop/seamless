package approval

// The anchor rule (owner decision 2026-11-07): a lease covers its anchor and
// every process the anchor started, so no request may name a process whose
// tree reaches past one agent session. The owner's Touch ID run anchored a
// request from a tmux pane at the tmux server, and one touch would have
// opened the project to every program in every pane, other agents' sessions
// included.
//
// Filing (Service.Request, Grants.RequestGrant) takes an anchor only when the
// kernel's name for its program -- the path, else the kernel's own name
// (access.ProgramOf) -- passes two checks:
//
//   - it is not a broad ancestor (broadWords): a shell, a terminal
//     multiplexer, a terminal emulator or the application that hosts one, a
//     login or remote-login process, or the system's process manager (pid 1);
//   - and it is positively an agent's: an agent program itself (Claude Code,
//     Codex, the Claude app's per-server wrapper), a process with an agent
//     program above it in the connection's kernel-verified chain (its tree
//     lies inside that agent's), or the very process a live session's
//     SessionStart hook stamped when that process is a JavaScript runtime
//     (Claude Code installed with npm runs as node).
//
// The positive half is what keeps a new terminal app from slipping through:
// a deny list can only ever name the terminals someone thought of. An anchor
// the kernel no longer names at all -- it exited between the chain walk and
// the lookup -- is filed as before: its token names a process that is gone,
// so a lease on it can never apply to anything.
//
// The approve core (Service.Approve, Grants.Approve) sees only the stored
// row, so it applies the deny list to the row's program: a request an older
// daemon filed, or any row naming a broad ancestor, is never approved, on any
// path (Touch ID, a passkey, seam approve).

import (
	"errors"
	"fmt"
	"path/filepath"
	"regexp"
	"strings"

	"github.com/arctop/seamless/internal/access"
)

// ErrBroadAnchor is a request whose anchor -- the process a lease would name
// -- is not an agent's: a lease on it would cover programs no agent session
// started. Filing refuses it (no request exists), and the approve core
// refuses a stored one.
var ErrBroadAnchor = errors.New("the process asking is not an agent")

// anchorRemedy is what the caller does instead, once for every surface.
const anchorRemedy = "run the work from an agent session (Claude Code or Codex), which asks for itself; for a one-off, " +
	"the owner approves that session's request with `seam approve` in their own terminal."

// AnchorError is a request refused for its anchor. Program is the kernel's
// name for it (a base name), What says what kind of process it is.
type AnchorError struct {
	Program string
	What    string
	// broad is a process whose tree reaches other sessions (the deny list),
	// as opposed to one no agent started.
	broad bool
}

func (e *AnchorError) Error() string { return e.Rule() + ".\nRemedy: " + e.Remedy() }

// Is makes errors.Is(err, ErrBroadAnchor) hold.
func (e *AnchorError) Is(target error) bool { return target == ErrBroadAnchor }

// Rule is the refusal's first line, without its full stop: what was not
// filed, and why.
func (e *AnchorError) Rule() string {
	if e.broad {
		return "Seamless files no access request for this process: it is " + e.What + ", not an agent, and an " +
			"approval would open the project to every program it started, other agent sessions included"
	}
	return "Seamless files no access request for this process: it is " + e.What + ", which no agent session " +
		"started, and an approval would open the project to every program it started"
}

// Remedy is the refusal's second line: what to do instead.
func (e *AnchorError) Remedy() string { return anchorRemedy }

// programKind is what a program is to the anchor rule.
type programKind int

const (
	// programUnknown: the kernel names no live process (it exited).
	programUnknown programKind = iota
	// programOther: a program Seamless does not know.
	programOther
	// programAgent: Claude Code, Codex, or the Claude app's per-server
	// wrapper -- one agent session's process.
	programAgent
	// programBroad: a broad ancestor (broadWords).
	programBroad
)

// program is one classified program: its kind, the readable agent name
// (Facts.Agent) for an agent, the base name the kernel gave it, and, for a
// broad one, what kind of process it is.
type program struct {
	kind  programKind
	agent string
	base  string
	what  string
}

var (
	// versionRe is a release version as Claude Code and Codex lay them out
	// on disk: 2.1.319, 0.163.0-alpha.10.2.
	versionRe = regexp.MustCompile(`^[0-9]+\.[0-9]+\.[0-9]+([-+.][0-9A-Za-z][0-9A-Za-z.+-]{0,24})?$`)
	// codexTripleRe is the Rust target triple a Codex binary may carry
	// (codex-aarch64-apple-darwin, codex-x86_64-unknown-linux-musl).
	codexTripleRe = regexp.MustCompile(`^codex-[0-9a-z_]+-[0-9a-z_-]*(apple-darwin|linux-gnu|linux-musl|windows-msvc)$`)
)

// codexCut reports a triple-named Codex binary as Linux's kernel names it,
// cut to 15 bytes ("codex-x86_64-un"): only a bare name of exactly that
// length, an architecture after the dash.
func codexCut(lower string, bare bool) bool {
	if !bare || len(lower) != 15 {
		return false
	}
	rest, ok := strings.CutPrefix(lower, "codex-")
	return ok && (strings.HasPrefix(rest, "x86_64") || strings.HasPrefix(rest, "aarch64") || strings.HasPrefix(rest, "arm64"))
}

// jsRuntimes are the runtimes an agent may run on (Claude Code installed with
// npm is node): the stamped-session half of the rule accepts one only when it
// is exactly the process a SessionStart hook stamped.
var jsRuntimes = map[string]bool{"node": true, "bun": true, "deno": true}

// broadWords are the broad ancestors by the kernel's base name (lowercased,
// without .exe), each with what the refusal calls it. Every one hosts
// programs no single agent session started: a lease on it would cover them
// all.
var broadWords = func() map[string]string {
	m := map[string]string{}
	add := func(what string, names ...string) {
		for _, n := range names {
			m[n] = what
		}
	}
	add("a shell", "sh", "bash", "zsh", "dash", "ksh", "mksh", "ash", "fish", "csh", "tcsh", "busybox", "nu",
		"elvish", "yash", "cmd", "powershell", "pwsh")
	add("a terminal multiplexer", "tmux", "tmate", "screen", "zellij", "byobu", "abduco", "dtach",
		"wezterm-mux-server")
	add("a terminal", "terminal", "iterm2", "ghostty", "alacritty", "kitty", "wezterm", "wezterm-gui", "warp",
		"hyper", "tabby", "rio", "wave", "waveterm", "contour", "foot", "footclient", "konsole", "gnome-terminal",
		"gnome-terminal-server", "kgx", "ptyxis", "ptyxis-agent", "xterm", "uxterm", "urxvt", "urxvtd", "rxvt", "st",
		"tilix", "terminator", "xfce4-terminal", "lxterminal", "mate-terminal", "qterminal", "terminology", "sakura",
		"guake", "yakuake", "tilda", "cool-retro-term", "blackbox", "conhost", "openconsole", "windowsterminal")
	add("an editor that hosts terminals", "code", "code-insiders", "codium", "vscodium", "code-server", "cursor",
		"windsurf", "zed", "zed-editor", "electron", "emacs", "nvim", "vim")
	add("a login session", "login", "sshd", "sshd-session", "sshd-auth", "dropbear", "mosh-server", "telnetd",
		"su", "sudo", "doas")
	add("the system's process manager", "launchd", "init", "systemd")
	return m
}()

// broadPrefixes catch the broad ancestors whose names carry a version or a
// role (iTermServer-3.5.0, Code Helper (Plugin)), or that the kernel cuts
// short (Linux keeps 15 bytes: gnome-terminal-).
var broadPrefixes = []struct{ prefix, what string }{
	{"itermserver", "a terminal"},
	{"gnome-terminal", "a terminal"},
	{"code helper", "an editor that hosts terminals"},
	{"code - insiders helper", "an editor that hosts terminals"},
	{"cursor helper", "an editor that hosts terminals"},
	{"electron helper", "an editor that hosts terminals"},
}

// baseName is a program's base name as the rule compares it: the last path
// element (either separator), Linux's " (deleted)" mark for a binary replaced
// while it runs dropped, a login shell's leading dash dropped, a title a
// process set for itself cut at its colon ("tmux: server"), and Windows'
// .exe suffix dropped. Case is kept; the comparisons lowercase it.
func baseName(p string) string {
	p = strings.TrimSpace(strings.TrimSuffix(strings.TrimSpace(p), " (deleted)"))
	if i := strings.LastIndexAny(p, `/\`); i >= 0 {
		p = p[i+1:]
	}
	p = strings.TrimPrefix(p, "-")
	if name, _, ok := strings.Cut(p, ":"); ok {
		p = name
	}
	p = strings.TrimSpace(p)
	if strings.HasSuffix(strings.ToLower(p), ".exe") {
		p = p[:len(p)-len(".exe")]
	}
	return p
}

// classify names a program from the kernel's path for it, or its name when
// the path is unknown. It is the one classifier behind the anchor rule and
// the readable agent name the approval surfaces show (Facts.Agent). Paths are
// compared by shape, never by any one account's home.
func classify(p string) program {
	p = strings.TrimSpace(p)
	if p == "" {
		return program{kind: programUnknown}
	}
	clean := strings.TrimSuffix(p, " (deleted)")
	base := baseName(clean)
	lower := strings.ToLower(base)
	slashed := filepath.ToSlash(clean)
	dirs := strings.Split(slashed, "/")
	parent := func(n int) string {
		if i := len(dirs) - 1 - n; i >= 0 {
			return dirs[i]
		}
		return ""
	}
	agent := func(name string) program { return program{kind: programAgent, agent: name, base: base} }

	// The Claude app (macOS): its per-server MCP wrapper is one server's
	// process, and nothing else in its bundle is an agent's.
	if strings.HasSuffix(slashed, "/Claude.app/Contents/Helpers/disclaimer") {
		return agent("Claude app")
	}
	switch {
	// Claude Code's native install: .../claude/versions/<version>, the binary
	// named by its version. The kernel's own name for it is that bare
	// version (measured on macOS; Linux's when it runs by that path), and
	// on Linux the daemon's account reads only that name: no other agent
	// names its binary so.
	case versionRe.MatchString(base) && parent(1) == "versions" && strings.EqualFold(parent(2), "claude"),
		versionRe.MatchString(base) && len(dirs) == 1:
		return agent("Claude Code " + base)
	case base == "claude":
		// Any other claude binary: on PATH, the Claude app's bundled
		// runtime (.../claude-code/<version>/<hash>/claude.app/Contents/
		// MacOS/claude), an editor extension's, or Linux's kernel name for
		// the native install run through its symlink.
		for i := 0; i+1 < len(dirs)-1; i++ {
			if dirs[i] == "claude-code" && versionRe.MatchString(dirs[i+1]) {
				return agent("Claude Code " + dirs[i+1])
			}
		}
		return agent("Claude Code")
	case lower == "codex" || codexTripleRe.MatchString(lower) || codexCut(lower, len(dirs) == 1):
		// A version only where the path carries one (a Homebrew cask:
		// .../Caskroom/codex/<version>/codex-<triple>).
		if v := parent(1); versionRe.MatchString(v) {
			return agent("Codex " + v)
		}
		return agent("Codex")
	}

	if what, ok := broadWords[lower]; ok {
		return program{kind: programBroad, base: base, what: base + ", " + what}
	}
	for _, b := range broadPrefixes {
		if strings.HasPrefix(lower, b.prefix) {
			return program{kind: programBroad, base: base, what: base + ", " + b.what}
		}
	}
	// A macOS application's own executable (anything in an app bundle's
	// Contents/MacOS, helpers included): a GUI app hosts windows, terminals
	// and plugins no one agent session started. The agents' own bundles
	// matched above.
	if strings.Contains(slashed, ".app/Contents/MacOS/") {
		return program{kind: programBroad, base: base, what: base + ", an application"}
	}
	return program{kind: programOther, base: base}
}

// agentName is the readable agent name for a program (Facts.Agent): "Claude
// Code 2.1.319", "Codex", "" for anything that is not an agent's.
func agentName(p string) string {
	if c := classify(p); c.kind == programAgent {
		return c.agent
	}
	return ""
}

// judgeAnchor applies the anchor rule to anchor, the i-th process of chain
// (the peer first), whose program the kernel names name(token). stamped
// reports that anchor is the process a live session's SessionStart hook
// stamped.
func judgeAnchor(chain []access.ProcessToken, i int, name func(access.ProcessToken) string, stamped bool) (string, error) {
	prog := name(chain[i])
	c := classify(prog)
	switch {
	case chain[i].PID == 1:
		return prog, &AnchorError{Program: c.base, What: "pid 1, the system's process manager", broad: true}
	case c.kind == programBroad:
		return prog, &AnchorError{Program: c.base, What: c.what, broad: true}
	case c.kind == programAgent, c.kind == programUnknown:
		return prog, nil
	case stamped && jsRuntimes[strings.ToLower(c.base)]:
		return prog, nil
	}
	for _, above := range chain[i+1:] {
		if classify(name(above)).kind == programAgent {
			return prog, nil
		}
	}
	return prog, &AnchorError{Program: c.base, What: c.base}
}

// storedBroad says what a stored request's process is when it is a broad
// ancestor, or pid 1: the approve core's half of the rule, and the facts'
// anchor_is_shell warning. The row carries no chain, so the positive half
// cannot be judged again here; the filing side judged it. A row with no
// program (an older daemon on Linux, where another account's executable path
// is unreadable) names nothing to judge.
func storedBroad(pid int32, prog string) (string, bool) {
	switch c := classify(prog); {
	case pid == 1:
		return "pid 1, the system's process manager", true
	case c.kind == programBroad:
		return c.what, true
	}
	return "", false
}

// storedAnchorRefusal is the approve core's refusal of a stored request
// whose process is a broad ancestor (storedBroad): never approved, by any
// proof.
func storedAnchorRefusal(pid int32, prog string) error {
	what, broad := storedBroad(pid, prog)
	if !broad {
		return nil
	}
	return fmt.Errorf("%w: it is %s, and approving it would open the project to every program it started, "+
		"other agent sessions included; deny it, and %s", ErrBroadAnchor, what, anchorRemedy)
}
