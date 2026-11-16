package main

// seam lock -- list, declare, acquire and release repo locks (the locks_* MCP
// tools). A lock guards a repository's refs, and is named by the repository's
// checkout: seam resolves the checkout's git identity on this machine (its
// root, its main checkout, its origin, following a side clone's local-path
// origin to the repository it was cloned from) and sends it, so the daemon
// never reads this machine's disk to find it.

import (
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"os"
	"path/filepath"
	"strings"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/gitread"
)

var (
	lockListCmd = spec("lock list", groupLocks, "list the repo locks, or this repository's with --here",
		noArgs(), bindLockList, runLockList)
	lockDeclareCmd = spec("lock declare", groupLocks, "declare this repository's lock, once per repository and name",
		noArgs(), bindLockDeclare, runLockDeclare)
	lockAcquireCmd = spec("lock acquire", groupLocks, "take this repository's lock, or refresh the lease you hold",
		noArgs(), bindLockAcquire, runLockAcquire)
	lockReleaseCmd = spec("lock release", groupLocks, "release a repo lock you hold (--force: the owner's override)",
		noArgs(), bindLockRelease, runLockRelease)
)

// lkReadLocalRepo reports the main checkout root and the origin of the
// repository at path, or "" and "" when path is no checkout:
// core.FollowLocalOrigin's reader, on this machine's disk.
func lkReadLocalRepo(path string) (string, string) {
	if _, err := os.Lstat(filepath.Join(path, ".git")); err != nil {
		return "", ""
	}
	main := gitread.MainWorktreeRoot(path)
	return main, gitread.OriginURL(main)
}

// lkCheckout resolves the checkout at dir ("" = the working directory) to the
// identity the lock tools take. A directory outside any repository is an error
// naming the remedy.
func lkCheckout(dir string) (map[string]any, error) {
	if dir == "" {
		wd, err := os.Getwd()
		if err != nil {
			return nil, err
		}
		dir = wd
	}
	abs, err := filepath.Abs(dir)
	if err != nil {
		return nil, err
	}
	root := gitread.RepoRoot(abs)
	if root == "" {
		return nil, fmt.Errorf("%s is not inside a git repository: run seam lock from the repository's checkout, or pass --cwd", abs)
	}
	main := gitread.MainWorktreeRoot(root)
	args := map[string]any{"cwd": abs, "repo_root": root, "main_worktree_root": main}
	if origin := core.FollowLocalOrigin(gitread.OriginURL(main), main, lkReadLocalRepo); origin != "" {
		args["repo_origin"] = origin
	}
	return args, nil
}

// lkCall calls a lock tool once and returns its answer decoded and as sent
// (for --json). A refusal from a server that does not serve the tool at all --
// repo locks switched off there, or a server that predates them -- is said in
// those words, found by asking tools/list rather than by reading the error.
func lkCall(ctx context.Context, cli *mcpclient.Client, tool string, args map[string]any) (map[string]any, string, error) {
	text, err := callToolRaw(ctx, cli, tool, args)
	if err != nil {
		if off := lkServed(ctx, cli, tool); off != nil {
			return nil, "", off
		}
		return nil, "", err
	}
	// Every answer is a JSON object: unreadable text is something other than
	// seamlessd answering, never an empty result.
	var out map[string]any
	if err := json.Unmarshal([]byte(text), &out); err != nil {
		return nil, "", fmt.Errorf("unreadable %s response from seamlessd: %w", tool, err)
	}
	return out, text, nil
}

// errLkNotServed is a server whose tools/list holds no lock tool.
var errLkNotServed = errors.New("this server does not offer the repo lock tools: repo locks are switched off there " +
	"-- turn them on in its console under Settings -> Features, or with features.repo_locks: true in its config " +
	"(no lock is deleted while they are off) -- or the server predates them")

// lkServed returns errLkNotServed when the server lists no tool named tool, nil
// when it does or when the list cannot be read (the call's own error stands).
func lkServed(ctx context.Context, cli *mcpclient.Client, tool string) error {
	res, err := cli.ListTools(ctx, mcp.ListToolsRequest{})
	if err != nil {
		return nil
	}
	for _, t := range res.Tools {
		if t.Name == tool {
			return nil
		}
	}
	return fmt.Errorf("%w (no %s)", errLkNotServed, tool)
}

// lkLine renders one lock as the human line every lock command prints:
// "LOCK seamless main", its state, and its refs.
func lkLine(l map[string]any) string {
	state := "free"
	switch {
	case str(l["state"]) == string(core.RepoLockRequested):
		state = "waits for the owner's approval"
	case l["held"] == true:
		state = "held"
		if who := str(l["holder_session"]); who != "" {
			state += " by " + who
		}
		if until := lkWhen(str(l["until"])); until != "" {
			state += " until " + until
		}
	case l["lapsed"] == true:
		state = "free (a lapsed hold)"
	}
	return fmt.Sprintf("LOCK %s  %s  %s", str(l["label"]), state, strings.Join(strs(l["refs"]), ","))
}

// lkWhen renders a stored timestamp to the second, in UTC; "" for none.
func lkWhen(ts string) string {
	t, err := core.ParseTime(ts)
	if err != nil || t.IsZero() {
		return ts
	}
	return t.UTC().Format(time.RFC3339)
}

// --- lock list ---

type lockListOpts struct {
	here   *bool
	cwd    *string
	asJSON *bool
}

func bindLockList(fs *flag.FlagSet) *lockListOpts {
	return &lockListOpts{
		here:   fs.Bool("here", false, "list only the locks of the repository the working directory (or --cwd) is in"),
		cwd:    fs.String("cwd", "", "the checkout `DIR` --here names (default: the working directory)"),
		asJSON: jsonFlag(fs, jsonUsage),
	}
}

func runLockList(ctx context.Context, e *env, o *lockListOpts, _ []string) error {
	args := map[string]any{}
	if *o.here || *o.cwd != "" {
		checkout, err := lkCheckout(*o.cwd)
		if err != nil {
			return err
		}
		args = checkout
	}
	cli, _, err := e.dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = cli.Close() }()
	out, text, err := lkCall(ctx, cli, "locks_list", args)
	if err != nil {
		return err
	}
	if *o.asJSON {
		return writeRaw(e.stdout, []byte(text))
	}
	locks, _ := out["locks"].([]any)
	if len(locks) == 0 {
		fmt.Fprintln(e.stdout, "(no repo locks)")
		return nil
	}
	for _, l := range locks {
		m, _ := l.(map[string]any)
		fmt.Fprintln(e.stdout, lkLine(m))
	}
	return nil
}

// --- lock declare ---

type lockDeclareOpts struct {
	name     *string
	refs     *string
	projects *string
	session  *string
	cwd      *string
	asJSON   *bool
}

func bindLockDeclare(fs *flag.FlagSet) *lockDeclareOpts {
	return &lockDeclareOpts{
		name:     fs.String("name", "", "the lock's `NAME` on its repository (default: main)"),
		refs:     fs.String("refs", "", "comma-separated guarded `REFS`: full ref names or one-'*' patterns (default: refs/heads/<name>)"),
		projects: fs.String("projects", "", "comma-separated `SLUGS` whose restricted accounts may hold it (default: the checkout's project)"),
		session:  actorSessionFlag(fs),
		cwd:      fs.String("cwd", "", "the checkout `DIR` (default: the working directory)"),
		asJSON:   jsonFlag(fs, jsonUsage),
	}
}

func runLockDeclare(ctx context.Context, e *env, o *lockDeclareOpts, _ []string) error {
	args, err := lkCheckout(*o.cwd)
	if err != nil {
		return err
	}
	for key, v := range map[string]string{"name": *o.name, "refs": *o.refs, "projects": *o.projects, "session": *o.session} {
		if strings.TrimSpace(v) != "" {
			args[key] = v
		}
	}
	cli, _, err := e.dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = cli.Close() }()
	out, text, err := lkCall(ctx, cli, "locks_declare", args)
	if err != nil {
		return err
	}
	if *o.asJSON {
		return writeRaw(e.stdout, []byte(text))
	}
	l, _ := out["lock"].(map[string]any)
	refs := strings.Join(strs(l["refs"]), ",")
	switch {
	case str(l["state"]) == string(core.RepoLockRequested):
		fmt.Fprintf(e.stdout, "requested LOCK %s (%s): it waits for the owner's approval on the console's Locks page\n", str(l["label"]), refs)
	case out["declared"] == true:
		fmt.Fprintf(e.stdout, "declared LOCK %s (%s)\n", str(l["label"]), refs)
	default:
		fmt.Fprintf(e.stdout, "LOCK %s was already declared (%s)\n", str(l["label"]), refs)
	}
	return nil
}

// --- lock acquire ---

type lockAcquireOpts struct {
	name    *string
	id      *string
	lease   *int
	session *string
	cwd     *string
	asJSON  *bool
}

func bindLockAcquire(fs *flag.FlagSet) *lockAcquireOpts {
	return &lockAcquireOpts{
		name: fs.String("name", "", "the lock's `NAME` on this repository (default: main)"),
		id:   fs.String("id", "", "name the lock by its `ID` instead of the checkout"),
		// posIntFlag: 0 can only mean absent, so the server's default applies.
		lease:   posIntFlag(fs, "lease", 0, "lease `SECONDS` before the hold lapses (default: the server's 900)"),
		session: actorSessionFlag(fs),
		cwd:     fs.String("cwd", "", "the checkout `DIR` (default: the working directory)"),
		asJSON:  jsonFlag(fs, jsonUsage),
	}
}

// lkTargetArgs names a lock for acquire and release: by id, or by the checkout
// and the name.
func lkTargetArgs(id, name, cwd string) (map[string]any, error) {
	if strings.TrimSpace(id) != "" {
		if strings.TrimSpace(name) != "" || strings.TrimSpace(cwd) != "" {
			return nil, errors.New("pass --id, or the checkout (--cwd) and --name, not both")
		}
		return map[string]any{"id": strings.TrimSpace(id)}, nil
	}
	args, err := lkCheckout(cwd)
	if err != nil {
		return nil, err
	}
	if strings.TrimSpace(name) != "" {
		args["name"] = strings.TrimSpace(name)
	}
	return args, nil
}

func runLockAcquire(ctx context.Context, e *env, o *lockAcquireOpts, _ []string) error {
	args, err := lkTargetArgs(*o.id, *o.name, *o.cwd)
	if err != nil {
		return err
	}
	if *o.lease > 0 {
		args["lease_seconds"] = *o.lease
	}
	if s := strings.TrimSpace(*o.session); s != "" {
		args["session"] = s
	}
	cli, _, err := e.dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = cli.Close() }()
	out, text, err := lkCall(ctx, cli, "locks_acquire", args)
	if err != nil {
		return err
	}
	if *o.asJSON {
		return writeRaw(e.stdout, []byte(text))
	}
	l, _ := out["lock"].(map[string]any)
	verb := "acquired"
	if out["heartbeat"] == true {
		verb = "refreshed"
	}
	line := fmt.Sprintf("%s LOCK %s until %s", verb, str(l["label"]), lkWhen(str(l["until"])))
	if out["reclaimed"] == true {
		line += " (taken over from a lapsed hold)"
	}
	fmt.Fprintln(e.stdout, line)
	return nil
}

// --- lock release ---

type lockReleaseOpts struct {
	name    *string
	id      *string
	force   *bool
	session *string
	cwd     *string
	asJSON  *bool
}

func bindLockRelease(fs *flag.FlagSet) *lockReleaseOpts {
	return &lockReleaseOpts{
		name: fs.String("name", "", "the lock's `NAME` on this repository (default: main)"),
		id:   fs.String("id", "", "name the lock by its `ID` instead of the checkout"),
		force: fs.Bool("force", false,
			"owner override: release the lock whoever holds it (through the console's owner route, which agents cannot reach on the peer_auth socket)"),
		session: actorSessionFlag(fs),
		cwd:     fs.String("cwd", "", "the checkout `DIR` (default: the working directory)"),
		asJSON:  jsonFlag(fs, jsonUsage),
	}
}

func runLockRelease(ctx context.Context, e *env, o *lockReleaseOpts, _ []string) error {
	args, err := lkTargetArgs(*o.id, *o.name, *o.cwd)
	if err != nil {
		return err
	}
	cli, _, err := e.dial(ctx)
	if err != nil {
		return err
	}
	defer func() { _ = cli.Close() }()
	if *o.force {
		return lkForceRelease(ctx, e, cli, args, *o.asJSON)
	}
	if s := strings.TrimSpace(*o.session); s != "" {
		args["session"] = s
	}
	out, text, err := lkCall(ctx, cli, "locks_release", args)
	if err != nil {
		return err
	}
	if *o.asJSON {
		return writeRaw(e.stdout, []byte(text))
	}
	l, _ := out["lock"].(map[string]any)
	fmt.Fprintf(e.stdout, "released LOCK %s\n", str(l["label"]))
	return nil
}

// lkForceRelease is --force: the console's owner route, by the lock's id (read
// with locks_list when the checkout names it).
func lkForceRelease(ctx context.Context, e *env, cli *mcpclient.Client, args map[string]any, asJSON bool) error {
	id := str(args["id"])
	if id == "" {
		name := str(args["name"])
		if name == "" {
			name = core.DefaultRepoLockName
		}
		delete(args, "name")
		out, _, err := lkCall(ctx, cli, "locks_list", args)
		if err != nil {
			return err
		}
		locks, _ := out["locks"].([]any)
		for _, l := range locks {
			if m, _ := l.(map[string]any); str(m["name"]) == name {
				id = str(m["id"])
				break
			}
		}
		if id == "" {
			return fmt.Errorf("no lock named %s is declared for this repository", name)
		}
	}
	cfg, err := e.loadConfig()
	if err != nil {
		return err
	}
	path := "/console/locks/" + id + "/release?format=json"
	if asJSON {
		body, err := consolePOSTRaw(cfg, path)
		if err != nil {
			return err
		}
		return writeRaw(e.stdout, body)
	}
	var out struct {
		Lock struct {
			Label string `json:"label"`
		} `json:"lock"`
		PriorSession string `json:"prior_holder_session"`
		PriorHolder  string `json:"prior_holder"`
	}
	if err := consolePOST(cfg, path, &out); err != nil {
		return err
	}
	prior := out.PriorSession
	if prior == "" {
		prior = out.PriorHolder
	}
	fmt.Fprintf(e.stdout, "force-released LOCK %s (it was held by %s)\n", out.Lock.Label, orDash(prior))
	return nil
}
