package main

import (
	"bytes"
	"context"
	"database/sql"
	"os/exec"
	"path/filepath"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

func openRetireTestDB(t *testing.T) *sql.DB {
	t.Helper()
	db, err := store.Open(filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.NoError(t, store.AdoptLocalHost(context.Background(), db, "alpha"))
	return db
}

func requireRetired(t *testing.T, db *sql.DB, slug string, want bool) core.Project {
	t.Helper()
	p, ok, err := store.ProjectBySlug(context.Background(), db, slug)
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, want, p.Retired())
	return p
}

func TestRetireProject_UnknownSlug(t *testing.T) {
	db := openRetireTestDB(t)
	var out bytes.Buffer
	err := retireProject(context.Background(), db, &out, "nope", false, false, time.Now())
	require.ErrorIs(t, err, errProjectNotFound)
	err = retireProject(context.Background(), db, &out, "nope", true, false, time.Now())
	require.ErrorIs(t, err, errProjectNotFound)
}

func TestRetireProject_RefusesWhileInUse(t *testing.T) {
	db := openRetireTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	ts := core.FormatTime(now)

	// Another host's repo registers "app"; this machine maps a path to it too.
	_, _, err := store.RegisterProjectForCWD(ctx, db, store.CWDIdentity{
		Host: "beta", CWD: "/elsewhere/app", RepoRoot: "/elsewhere/app",
	}, "alpha")
	require.NoError(t, err)
	local := t.TempDir()
	require.NoError(t, store.AddRepoMapping(ctx, db, local, "app"))

	// One active memory and an archived one; one note; one open task and a done one.
	for _, m := range []struct {
		id  string
		inv any
	}{{"01MEMLIVE", nil}, {"01MEMGONE", ts}} {
		_, err = db.ExecContext(ctx, `
			INSERT INTO memories_index
			    (id, kind, name, description, project, file_path, tags, valid_from,
			     invalid_at, superseded_by, source_session, content_hash, created_at, updated_at)
			VALUES (?, 'gotcha', ?, 'd', 'app', ?, '[]', ?, ?, NULL, '', 'h', ?, ?)`,
			m.id, m.id, "memory/app/"+m.id+".md", ts, m.inv, ts, ts)
		require.NoError(t, err)
	}
	_, err = db.ExecContext(ctx, `
		INSERT INTO notes_index
		    (id, title, slug, description, project, file_path, tags, source_url,
		     content_hash, created_at, updated_at)
		VALUES ('01NOTE', 't', 'stray', '', 'app', 'notes/app/stray.md', '[]', '', 'h', ?, ?)`, ts, ts)
	require.NoError(t, err)
	for id, status := range map[string]core.TaskStatus{"01TASKOPEN": core.TaskOpen, "01TASKDONE": core.TaskDone} {
		require.NoError(t, store.CreateTask(ctx, db, core.Task{
			ID: id, ProjectSlug: "app", Title: id, Status: status, CreatedAt: now, UpdatedAt: now,
		}))
	}

	// Dry-run or not, every blocker is printed and nothing changes.
	for _, dry := range []bool{true, false} {
		var out bytes.Buffer
		err = retireProject(ctx, db, &out, "app", false, dry, now)
		require.ErrorIs(t, err, errProjectInUse)
		got := out.String()
		require.Contains(t, got, "seamlessd unmap-repo --path "+shellQuoter(runtime.GOOS)(local))
		require.Contains(t, got, "seamlessd unmap-repo --host beta --path /elsewhere/app")
		require.Contains(t, got, "1 active memory, 1 note, 1 open task")
		requireRetired(t, db, "app", false)
	}
}

// The remedy printed for another host's mapping names a --host/--path pair that
// unmap-repo accepts and finds, whichever OS this daemon runs and whichever OS
// the client does: each path is shown as the client sent it, and resolves as
// shown.
func TestRetireProject_RemoteRemediesResolve(t *testing.T) {
	db := openRetireTestDB(t)
	ctx := context.Background()
	sent := map[string]string{
		"beta":   "/elsewhere/app",
		"winbox": `C:\repos\app`,
		"uncbox": `\\srv\share\app`,
	}
	for _, host := range []string{"beta", "winbox", "uncbox"} {
		slug, _, err := store.RegisterProjectForCWD(ctx, db, store.CWDIdentity{
			Host: host, CWD: sent[host], RepoRoot: sent[host],
		}, "alpha")
		require.NoError(t, err)
		require.Equal(t, "app", slug, "the same repo on another device joins the project")
	}

	var out bytes.Buffer
	err := retireProject(ctx, db, &out, "app", false, true, time.Now())
	require.ErrorIs(t, err, errProjectInUse)
	rows, err := store.RepoMapRows(ctx, db)
	require.NoError(t, err)
	require.Len(t, rows, len(sent))
	quote := shellQuoter(runtime.GOOS)
	for _, r := range rows {
		require.Equal(t, sent[r.Host], r.Path, "stored as the client sent it")
		require.Contains(t, out.String(), "seamlessd unmap-repo --host "+quote(r.Host)+" --path "+quote(r.Path)+"`")
		targets, err := unmapTargets(ctx, db, r.Path, false, r.Host)
		require.NoError(t, err, r.Path)
		require.Len(t, targets, 1)
		require.Equal(t, r.Host, targets[0].Host)
	}
}

func TestRetireProject_RetireAndUndo(t *testing.T) {
	db := openRetireTestDB(t)
	ctx := context.Background()
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	_, err := store.EnsureProject(ctx, db, "app-2", "app-2")
	require.NoError(t, err)
	// Sessions are history, not content: they never block a retire.
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: "01SESS", Name: "cc/s", ProjectSlug: "app-2", Status: core.SessionCompleted,
		CreatedAt: now, UpdatedAt: now,
	}))

	run := func(undo, dry bool) string {
		t.Helper()
		var out bytes.Buffer
		require.NoError(t, retireProject(ctx, db, &out, "app-2", undo, dry, now))
		return out.String()
	}

	require.Contains(t, run(false, true), "would retire")
	requireRetired(t, db, "app-2", false)

	require.Contains(t, run(false, false), `retired project "app-2"`)
	p := requireRetired(t, db, "app-2", true)
	require.True(t, p.RetiredAt.Equal(now))

	// Idempotent: a second retire keeps the original stamp.
	require.Contains(t, run(false, false), "already retired")
	p = requireRetired(t, db, "app-2", true)
	require.True(t, p.RetiredAt.Equal(now))

	require.Contains(t, run(true, true), "would un-retire")
	requireRetired(t, db, "app-2", true)

	require.Contains(t, run(true, false), `un-retired project "app-2"`)
	requireRetired(t, db, "app-2", false)
	require.Contains(t, run(true, false), "is not retired")
}

// Every remedy retire-project prints survives the paste: split as the owner's
// shell splits it -- sh, bash and zsh for a POSIX daemon, PowerShell for a
// Windows one -- its arguments are exactly the mapping's host and path, and
// unmap-repo finds that mapping with them. The paths carry what an unquoted
// line loses: backslashes, spaces, an apostrophe, a "$". So does one host.
func TestRetireProject_RemediesSurviveTheShell(t *testing.T) {
	db := openRetireTestDB(t)
	ctx := context.Background()
	remote := map[string]string{
		"winbox":  `C:\Users\Jo Smith\repos\app`,
		"beta":    "/home/me/it's mine/app",
		"unc box": `\\srv\c$\repos\app`,
	}
	for host, root := range remote {
		slug, _, err := store.RegisterProjectForCWD(ctx, db, store.CWDIdentity{
			Host: host, CWD: root, RepoRoot: root,
		}, "alpha")
		require.NoError(t, err)
		require.Equal(t, "app", slug)
	}
	local := filepath.Join(t.TempDir(), "my app")
	require.NoError(t, store.AddRepoMapping(ctx, db, local, "app"))

	for _, goos := range []string{"linux", "windows"} {
		t.Run(goos, func(t *testing.T) {
			split, shells := splitPOSIXWords, posixShells()
			if goos == "windows" {
				split, shells = splitPowerShellWords, nil
			}
			blockers, err := retireBlockers(ctx, db, "app", shellQuoter(goos))
			require.NoError(t, err)
			found := map[string]bool{}
			for _, b := range blockers {
				parts := strings.Split(b, "`")
				for i := 1; i < len(parts); i += 2 {
					line := strings.ReplaceAll(parts[i], "<slug>", "other") // the owner's pick
					argv := split(t, line)
					for _, sh := range shells {
						require.Equal(t, argv[1:], shellArgv(t, sh, line), "%s: %s", sh[0], line)
					}
					require.GreaterOrEqual(t, len(argv), 2, line)
					require.Equal(t, "seamlessd", argv[0], line)
					flags := map[string]string{}
					for j := 2; j < len(argv); j += 2 {
						require.True(t, strings.HasPrefix(argv[j], "--"), line)
						require.Less(t, j+1, len(argv), line)
						flags[argv[j]] = argv[j+1]
					}
					switch argv[1] {
					case "map-repo":
						require.Equal(t, local, flags["--path"], line)
					case "unmap-repo":
						targets, err := unmapTargets(ctx, db, flags["--path"], false, flags["--host"])
						require.NoError(t, err, line)
						require.Len(t, targets, 1, line)
						found[targets[0].Host+" "+targets[0].Path] = true
					default:
						t.Fatalf("unexpected remedy %q", line)
					}
				}
			}
			require.Len(t, found, len(remote)+1, "every mapping has an unmap-repo line that finds it")
		})
	}
}

// awkwardArgs are words a printed command can carry: Windows and POSIX paths
// with every character a shell might act on -- quotes (typographic ones too),
// expansions, globs, operators, whitespace, a leading "=" or "~", non-ASCII --
// plus the plain ones, and the empty string.
var awkwardArgs = []string{
	"/home/me/app", `C:\repos\app`, `\\srv\share\app`, "C:/repos/app", "winbox", "my-mac.local",
	`C:\Users\Jo Smith\repos\app`, "/home/me/my app", "/home/me/it's", `x''y`, "'lead",
	"C:\\it\u2019s\u2018odd\u201a\u201b", "/tmp/\u201cq\u201d\u201e",
	`\\srv\c$\repos`, "/home/$USER/app", "/tmp/$(id)", "/tmp/`id`", `/tmp/a"b`, `/tmp/a\b`,
	"/tmp/*?[x]{a,b}", "/tmp/a;b&c|d<e>f(g)", "=app", "~/app", "/tmp/~x", "/tmp/#x", "!x",
	"/tmp/a^b%c@d+e,f", "/tmp/tab\there", "/tmp/new\nline", "/tmp/na\u00efve", "",
}

// Each quoter's output reads back as the word it quoted: through a model of
// that shell, and for POSIX through every real sh, bash and zsh on this
// machine. A plain path stays bare, so the common remedy prints as typed.
func TestShellQuoters_RoundTrip(t *testing.T) {
	require.Equal(t, "/home/me/app", posixShellArg("/home/me/app"))
	require.Equal(t, `'C:\repos\app'`, posixShellArg(`C:\repos\app`))
	require.Equal(t, `'it'\''s'`, posixShellArg("it's"))
	require.Equal(t, "''", posixShellArg(""))
	require.Equal(t, `C:\repos\app`, powerShellArg(`C:\repos\app`))
	require.Equal(t, "/home/me/app", powerShellArg("/home/me/app"))
	require.Equal(t, `'it''s'`, powerShellArg("it's"))
	require.Equal(t, `'\\srv\c$\repos'`, powerShellArg(`\\srv\c$\repos`))
	require.Equal(t, "''", powerShellArg(""))

	quoted := make([]string, len(awkwardArgs))
	for i, s := range awkwardArgs {
		require.Equal(t, []string{"seamlessd", s}, splitPOSIXWords(t, "seamlessd "+posixShellArg(s)), "POSIX %q", s)
		require.Equal(t, []string{"seamlessd", s}, splitPowerShellWords(t, "seamlessd "+powerShellArg(s)), "PowerShell %q", s)
		quoted[i] = posixShellArg(s)
	}
	for _, sh := range posixShells() {
		require.Equal(t, awkwardArgs, shellArgv(t, sh, "seamlessd "+strings.Join(quoted, " ")), sh[0])
	}
}

// splitPOSIXWords splits a command line as a POSIX shell does, for what a
// printed command may contain: blanks, single quotes and backslash escapes.
// Anything else a shell would expand or act on outside quotes fails the test,
// so a printed command can never lean on it.
func splitPOSIXWords(t *testing.T, line string) []string {
	t.Helper()
	var words []string
	var cur strings.Builder
	inWord := false
	for i := 0; i < len(line); i++ {
		switch c := line[i]; {
		case c == ' ' || c == '\t' || c == '\n':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		case c == '\'':
			n := strings.IndexByte(line[i+1:], '\'')
			require.GreaterOrEqual(t, n, 0, "unterminated quote in %q", line)
			cur.WriteString(line[i+1 : i+1+n])
			i += n + 1
			inWord = true
		case c == '\\':
			require.Less(t, i+1, len(line), "trailing backslash in %q", line)
			i++
			cur.WriteByte(line[i])
			inWord = true
		case strings.IndexByte("\"`$|&;<>(){}*?[]#~=^!", c) >= 0:
			t.Fatalf("unquoted %q is read by the shell in %q", c, line)
		default:
			cur.WriteByte(c)
			inWord = true
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words
}

// splitPowerShellWords splits a command line as PowerShell does, for what a
// printed command may contain: blanks and whole-word single-quoted strings,
// where PowerShell reads U+2018-U+201B as "'" too and a doubled quote is one
// literal quote. A bare character PowerShell or cmd.exe would act on fails the
// test, as does any bare non-ASCII.
func splitPowerShellWords(t *testing.T, line string) []string {
	t.Helper()
	isQuote := func(r rune) bool { return r == '\'' || (r >= '\u2018' && r <= '\u201b') }
	rs := []rune(line)
	var words []string
	var cur strings.Builder
	inWord := false
	for i := 0; i < len(rs); i++ {
		switch r := rs[i]; {
		case r == ' ' || r == '\t' || r == '\n':
			if inWord {
				words = append(words, cur.String())
				cur.Reset()
				inWord = false
			}
		case isQuote(r):
			require.False(t, inWord, "a quote inside a bare word in %q", line)
			closed := false
			for i++; i < len(rs); i++ {
				if !isQuote(rs[i]) {
					cur.WriteRune(rs[i])
					continue
				}
				if i+1 < len(rs) && isQuote(rs[i+1]) {
					i++
					cur.WriteRune(rs[i])
					continue
				}
				closed = true
				break
			}
			require.True(t, closed, "unterminated quote in %q", line)
			require.True(t, i+1 == len(rs) || rs[i+1] == ' ', "a quoted string runs into a bare word in %q", line)
			inWord = true
		case r >= 0x80 || strings.ContainsRune("\"`$@#,;(){}|&<>%^!", r):
			t.Fatalf("unquoted %q is read by PowerShell or cmd.exe in %q", r, line)
		default:
			cur.WriteRune(r)
			inWord = true
		}
	}
	if inWord {
		words = append(words, cur.String())
	}
	return words
}

// posixShells lists this machine's POSIX shells, each as the argv that runs it
// without reading an rc file. None on Windows, where PowerShell is modeled.
func posixShells() [][]string {
	if runtime.GOOS == "windows" {
		return nil
	}
	var out [][]string
	for _, sh := range [][]string{{"sh"}, {"bash"}, {"zsh", "-f"}} {
		if p, err := exec.LookPath(sh[0]); err == nil {
			out = append(out, append([]string{p}, sh[1:]...))
		}
	}
	return out
}

// shellArgv has a real shell parse line, its first word swapped for a printf
// that echoes each argument back, and returns the arguments the shell passed.
func shellArgv(t *testing.T, shell []string, line string) []string {
	t.Helper()
	_, args, _ := strings.Cut(line, " ")
	cmd := exec.Command(shell[0], append(shell[1:], "-c", `printf '%s\036' `+args)...)
	cmd.Env = []string{} // no ENV or BASH_ENV file to source
	out, err := cmd.Output()
	require.NoError(t, err, "%s -c %q", shell[0], args)
	got := strings.Split(string(out), "\036")
	return got[:len(got)-1]
}
