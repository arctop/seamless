package agentproc

import (
	"errors"
	"runtime"
	"strconv"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// table is a fake process table for the walk.
type table map[int]procInfo

func (tb table) look(pid int) (procInfo, error) {
	info, ok := tb[pid]
	if !ok {
		return procInfo{}, errNoProcess
	}
	return info, nil
}

func TestAnchorFrom(t *testing.T) {
	for _, tc := range []struct {
		name   string
		table  table
		want   string
		wantOK bool
	}{
		{
			// Claude Code runs exec-form hooks and stdio servers as direct children.
			name: "direct child of the agent",
			table: table{
				300: {ppid: 200, name: "seam", start: 30},
				200: {ppid: 100, name: "claude", start: 20},
				100: {ppid: 1, name: "zsh", start: 10},
			},
			want: "200.20", wantOK: true,
		},
		{
			// headersHelper runs through `sh -c`: the shell is skipped.
			name: "through a shell",
			table: table{
				300: {ppid: 250, name: "seam", start: 30},
				250: {ppid: 200, name: "sh", start: 25},
				200: {ppid: 100, name: "claude", start: 20},
				100: {ppid: 1, name: "-zsh", start: 10},
			},
			want: "200.20", wantOK: true,
		},
		{
			// A Windows client may route a hook through Git Bash or cmd.
			name: "windows shells",
			table: table{
				300: {ppid: 260, name: "seam.exe", start: 30},
				260: {ppid: 250, name: "BASH.EXE", start: 26},
				250: {ppid: 200, name: "cmd.exe", start: 25},
				200: {ppid: 100, name: "codex.exe", start: 20},
				100: {ppid: 4, name: "explorer.exe", start: 10},
			},
			want: "200.20", wantOK: true,
		},
		{
			// Run by hand in a terminal: the anchor is whatever owns the terminal,
			// which no hook names, so it can only fail to match.
			name: "hand-run under tmux",
			table: table{
				300: {ppid: 100, name: "seam", start: 30},
				100: {ppid: 50, name: "-zsh", start: 10},
				50:  {ppid: 1, name: "tmux", start: 5},
			},
			want: "50.5", wantOK: true,
		},
		{
			// A git hook runs `seam lock check` below git itself: the walk passes
			// over the hook's shell and git to the agent that ran git.
			name: "a git hook under the agent",
			table: table{
				300: {ppid: 290, name: "seam", start: 30},
				290: {ppid: 280, name: "sh", start: 29},
				280: {ppid: 250, name: "git", start: 28},
				250: {ppid: 200, name: "zsh", start: 25},
				200: {ppid: 100, name: "claude", start: 20},
				100: {ppid: 1, name: "-zsh", start: 10},
			},
			want: "200.20", wantOK: true,
		},
		{
			// `git pull` runs `git merge`, whose hook sits below two gits.
			name: "a hook below git pull's merge",
			table: table{
				300: {ppid: 290, name: "seam", start: 30},
				290: {ppid: 285, name: "sh", start: 29},
				285: {ppid: 280, name: "git", start: 28},
				280: {ppid: 250, name: "/usr/bin/git", start: 27},
				250: {ppid: 200, name: "bash", start: 25},
				200: {ppid: 100, name: "codex", start: 20},
				100: {ppid: 1, name: "-zsh", start: 10},
			},
			want: "200.20", wantOK: true,
		},
		{
			// The owner committing at a terminal: past git, the anchor is what
			// owns the terminal, which no session is stamped with, so the hook's
			// connection binds nothing.
			name: "a git hook at the owner's terminal",
			table: table{
				300: {ppid: 290, name: "seam", start: 30},
				290: {ppid: 280, name: "sh", start: 29},
				280: {ppid: 100, name: "git", start: 28},
				100: {ppid: 50, name: "-zsh", start: 10},
				50:  {ppid: 1, name: "tmux", start: 5},
			},
			want: "50.5", wantOK: true,
		},
		{
			name: "only shells up to init",
			table: table{
				300: {ppid: 100, name: "seam", start: 30},
				100: {ppid: 1, name: "bash", start: 10},
			},
		},
		{
			// A recorded parent that started AFTER its child is a reused pid (the
			// real parent exited): following it would name an unrelated process.
			name: "reused parent pid",
			table: table{
				300: {ppid: 200, name: "seam", start: 30},
				200: {ppid: 100, name: "claude", start: 40},
			},
		},
		{
			name: "parent vanished mid-walk",
			table: table{
				300: {ppid: 200, name: "seam", start: 30},
			},
		},
		{
			name:  "self unreadable",
			table: table{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, ok := anchorFrom(300, tc.table.look)
			require.Equal(t, tc.wantOK, ok)
			require.Equal(t, tc.want, got)
			if ok {
				require.True(t, Valid(got), "anchorFrom must emit what Valid accepts: %q", got)
			}
		})
	}
}

func TestAnchorFrom_DepthIsBounded(t *testing.T) {
	tb := table{300: {ppid: 299, name: "seam", start: 300}}
	for pid := 299; pid > 1; pid-- {
		tb[pid] = procInfo{ppid: pid - 1, name: "sh", start: uint64(pid)}
	}
	tb[2] = procInfo{ppid: 1, name: "agent", start: 2}
	_, ok := anchorFrom(300, tb.look)
	require.False(t, ok, "a walk that has climbed past maxDepth shells has left the client")
}

func TestAnchorFrom_LookupErrorStops(t *testing.T) {
	look := func(pid int) (procInfo, error) {
		if pid == 300 {
			return procInfo{ppid: 200, name: "seam", start: 30}, nil
		}
		return procInfo{}, errors.New("permission denied")
	}
	_, ok := anchorFrom(300, look)
	require.False(t, ok)
}

// Chain climbs on past the first anchor: every ancestor that is neither a shell
// nor git, nearest first, so a git an agent ran through a wrapper of its own
// still reaches the agent.
func TestChainFrom(t *testing.T) {
	for _, tc := range []struct {
		name  string
		table table
		want  []string
	}{
		{
			// fanout-land: the agent runs node from its shell, node runs git, git
			// runs the hook. The wrapper is Anchor's answer; the agent is next.
			name: "git run by node under the agent",
			table: table{
				300: {ppid: 290, name: "seam", start: 30},
				290: {ppid: 280, name: "sh", start: 29},
				280: {ppid: 270, name: "git", start: 28},
				270: {ppid: 250, name: "node", start: 27},
				250: {ppid: 200, name: "zsh", start: 25},
				200: {ppid: 100, name: "claude", start: 20},
				100: {ppid: 50, name: "-zsh", start: 10},
				50:  {ppid: 1, name: "tmux", start: 5},
			},
			want: []string{"270.27", "200.20", "50.5"},
		},
		{
			// `make land` running a script that runs node that runs git: the agent
			// sits 8 processes up, past maxDepth, where Anchor alone stops.
			name: "make, a script and node between the agent and git",
			table: table{
				300: {ppid: 290, name: "seam", start: 30},
				290: {ppid: 285, name: "sh", start: 29},
				285: {ppid: 280, name: "git", start: 28},
				280: {ppid: 275, name: "node", start: 27},
				275: {ppid: 270, name: "bash", start: 26},
				270: {ppid: 265, name: "sh", start: 25},
				265: {ppid: 260, name: "make", start: 24},
				260: {ppid: 200, name: "zsh", start: 23},
				200: {ppid: 1, name: "claude", start: 20},
			},
			want: []string{"280.27", "265.24", "200.20"},
		},
		{
			// The owner at a terminal: the chain is the terminal's owners alone,
			// none of which a session is stamped with.
			name: "a git hook at the owner's terminal",
			table: table{
				300: {ppid: 290, name: "seam", start: 30},
				290: {ppid: 280, name: "sh", start: 29},
				280: {ppid: 100, name: "git", start: 28},
				100: {ppid: 60, name: "-zsh", start: 10},
				60:  {ppid: 50, name: "login", start: 6},
				50:  {ppid: 1, name: "Terminal", start: 5},
			},
			want: []string{"60.6", "50.5"},
		},
		{
			// A process the walk cannot read ends it, keeping what it found.
			name: "an unreadable ancestor ends the chain",
			table: table{
				300: {ppid: 280, name: "seam", start: 30},
				280: {ppid: 270, name: "git", start: 28},
				270: {ppid: 250, name: "node", start: 27},
				250: {ppid: 200, name: "zsh", start: 25},
			},
			want: []string{"270.27"},
		},
		{
			// So does a reused pid: a recorded parent younger than its child.
			name: "a reused parent pid ends the chain",
			table: table{
				300: {ppid: 280, name: "seam", start: 30},
				280: {ppid: 270, name: "git", start: 28},
				270: {ppid: 200, name: "node", start: 27},
				200: {ppid: 1, name: "claude", start: 40},
			},
			want: []string{"270.27"},
		},
		{
			name: "only shells up to init",
			table: table{
				300: {ppid: 100, name: "seam", start: 30},
				100: {ppid: 1, name: "bash", start: 10},
			},
		},
		{
			name:  "self unreadable",
			table: table{},
		},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got := walk(300, tc.table.look, chainDepth, 0)
			require.Equal(t, tc.want, got)
			for _, id := range got {
				require.True(t, Valid(id), "walk must emit what Valid accepts: %q", id)
			}
			anchor, ok := anchorFrom(300, tc.table.look)
			if len(got) > 0 && got[0] == anchor {
				return
			}
			// Anchor and the chain's head differ only where the head lies past
			// maxDepth, where Anchor finds nothing.
			require.False(t, ok, "Anchor named %q, the chain starts %v", anchor, got)
		})
	}
}

func TestChainFrom_DepthIsBounded(t *testing.T) {
	tb := table{300: {ppid: 299, name: "seam", start: 300}}
	for pid := 299; pid > 1; pid-- {
		tb[pid] = procInfo{ppid: pid - 1, name: "node", start: uint64(pid)}
	}
	got := walk(300, tb.look, chainDepth, 0)
	require.Len(t, got, chainDepth, "the walk reads at most chainDepth ancestors")
	require.Equal(t, "299.299", got[0])
}

func TestIsShell(t *testing.T) {
	for name, want := range map[string]bool{
		"sh": true, "bash": true, "zsh": true, "-zsh": true, "/bin/sh": true,
		"fish": true, "dash": true, "cmd.exe": true, "PowerShell.exe": true,
		"pwsh": true, `C:\Program Files\Git\bin\bash.exe`: true,
		"claude": false, "node": false, "codex": false, "2.1.293": false,
		"disclaimer": false, "tmux": false, "": false, "shell": false,
	} {
		require.Equal(t, want, isShell(name), "isShell(%q)", name)
	}
}

func TestIsGit(t *testing.T) {
	for name, want := range map[string]bool{
		"git": true, "/usr/bin/git": true, "GIT.EXE": true, `C:\Program Files\Git\cmd\git.exe`: true,
		"git-receive-pack": true, "git-receive-pac": true,
		"gitk": false, "github": false, "legit": false, "claude": false, "sh": false, "": false,
	} {
		require.Equal(t, want, isGit(name), "isGit(%q)", name)
	}
}

func TestValid(t *testing.T) {
	for s, want := range map[string]bool{
		"12706.1759850172000000": true,
		"1.1":                    true,
		"0.5":                    false, // pid 0 is never an agent
		"12.0":                   false, // a start time of 0 was refused at the source
		"12":                     false,
		"12.":                    false,
		".12":                    false,
		"-1.5":                   false,
		"12.5x":                  false,
		"12.5\n":                 false,
		"12345678901.5":          false, // more digits than any pid
		"":                       false,
	} {
		require.Equal(t, want, Valid(s), "Valid(%q)", s)
	}
}

// TestAnchor_Live exercises the real process table: this test binary is a child
// of `go test` (or whatever ran it), so a supported platform must name an
// ancestor, and the name must be one the daemon accepts.
func TestAnchor_Live(t *testing.T) {
	switch runtime.GOOS {
	case "darwin", "linux", "windows":
	default:
		t.Skip("no process-table reader on " + runtime.GOOS)
	}
	got, ok := Anchor()
	require.True(t, ok, "a supported platform must find a non-shell ancestor of a test binary")
	require.True(t, Valid(got), "Anchor emitted %q", got)

	pid, err := strconv.Atoi(strings.SplitN(got, ".", 2)[0])
	require.NoError(t, err)
	look, err := newLookup()
	require.NoError(t, err)
	info, err := look(pid)
	require.NoError(t, err)
	require.False(t, isShell(info.name), "the anchor is never a shell: %q", info.name)
	again, ok := Anchor()
	require.True(t, ok)
	require.Equal(t, got, again, "the identity is stable for the life of the process")

	chain := Chain()
	require.NotEmpty(t, chain)
	require.Equal(t, got, chain[0], "the chain starts at Anchor's answer")
}
