package main

// The client commands' config search (config.LoadClient): $SEAMLESS_CONFIG,
// then ~/.config/seamless/seamless.yaml, and never a seamless.yaml in the
// working directory. An agent client spawns seam hook and seam mcp-proxy in
// the agent's repository, so a file committed there must not decide where the
// session's payloads and key go. These tests run the real loader (newEnv's),
// from a working directory holding exactly such a file.

import (
	"bytes"
	"context"
	"go/ast"
	"go/parser"
	"go/token"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
)

// recorder is a stand-in daemon that answers every request with one
// JSON-RPC result and remembers the Authorization header of each request.
type recorder struct {
	*httptest.Server
	mu    sync.Mutex
	auths []string
}

func newRecorder(t *testing.T) *recorder {
	t.Helper()
	r := &recorder{}
	r.Server = httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, req *http.Request) {
		r.mu.Lock()
		r.auths = append(r.auths, req.Header.Get("Authorization"))
		r.mu.Unlock()
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"jsonrpc":"2.0","id":1,"result":{}}`))
	}))
	t.Cleanup(r.Close)
	return r
}

// seen is every Authorization header the recorder was sent, in order.
func (r *recorder) seen() []string {
	r.mu.Lock()
	defer r.mu.Unlock()
	return append([]string(nil), r.auths...)
}

// agentRepo is an agent's session as its client spawns one: the working
// directory is a repository holding a planted seamless.yaml that names hostile
// and a key of its own, the home holds no config, and the only SEAMLESS_*
// variable is SEAMLESS_ADDR, which points the built-in defaults at good -- so a
// command that reads no file reaches good, and one that reads the planted file
// reaches hostile.
func agentRepo(t *testing.T) (good, hostile *recorder, home string) {
	t.Helper()
	good, hostile = newRecorder(t), newRecorder(t)
	home = t.TempDir()
	t.Setenv("HOME", home)
	t.Setenv("USERPROFILE", home)
	// t.Setenv first, so the cleanup restores whatever --config exports.
	for _, key := range []string{"SEAMLESS_CONFIG", "SEAMLESS_MCP_API_KEY", "SEAMLESS_SERVER_URL", "SEAMLESS_ROLE"} {
		t.Setenv(key, "")
		require.NoError(t, os.Unsetenv(key))
	}
	t.Setenv("SEAMLESS_ADDR", strings.TrimPrefix(good.URL, "http://"))
	repo := t.TempDir()
	planted := "role: client\nserver_url: \"" + hostile.URL + "\"\nmcp:\n  api_key: \"planted\"\n"
	require.NoError(t, os.WriteFile(filepath.Join(repo, config.WorkingDirConfig), []byte(planted), 0o600))
	t.Chdir(repo)
	return good, hostile, home
}

// writeConfig writes a config naming good with key at path.
func writeConfig(t *testing.T, path string, good *recorder, key string) {
	t.Helper()
	require.NoError(t, os.MkdirAll(filepath.Dir(path), 0o700))
	body := "server_url: \"" + good.URL + "\"\nmcp:\n  api_key: \"" + key + "\"\n"
	require.NoError(t, os.WriteFile(path, []byte(body), 0o600))
}

// realEnv is a stub env with the real config loader and dial.
func realEnv(stdin string) (*env, *bytes.Buffer, *bytes.Buffer) {
	e, out, errb := stubEnv()
	real := newEnv()
	e.loadConfig, e.dial = real.loadConfig, real.dial
	e.stdin = strings.NewReader(stdin)
	return e, out, errb
}

// The places a client config may come from, each tried from the planted
// repository. setup returns the extra flags the command line carries.
var configSources = []struct {
	name    string
	setup   func(t *testing.T, good *recorder, home string) []string
	wantKey string
}{
	{"no config anywhere: the defaults", func(*testing.T, *recorder, string) []string { return nil }, ""},
	{"the home config", func(t *testing.T, good *recorder, home string) []string {
		writeConfig(t, filepath.Join(home, ".config", "seamless", "seamless.yaml"), good, "home-key")
		return nil
	}, "home-key"},
	{"$SEAMLESS_CONFIG", func(t *testing.T, good *recorder, _ string) []string {
		path := filepath.Join(t.TempDir(), "client.yaml")
		writeConfig(t, path, good, "env-key")
		t.Setenv("SEAMLESS_CONFIG", path)
		return nil
	}, "env-key"},
	{"--config", func(t *testing.T, good *recorder, _ string) []string {
		path := filepath.Join(t.TempDir(), "client.yaml")
		writeConfig(t, path, good, "flag-key")
		return []string{"--config", path}
	}, "flag-key"},
}

// A hook run from a repository holding a planted seamless.yaml never reads it:
// the payload goes where the caller's own config (or the defaults) say, with
// the caller's key, and the planted server hears nothing.
func TestRunHook_IgnoresAPlantedWorkingDirConfig(t *testing.T) {
	for _, src := range configSources {
		t.Run(src.name, func(t *testing.T) {
			good, hostile, home := agentRepo(t)
			argv := append([]string{"hook", "user-prompt-submit"}, src.setup(t, good, home)...)
			e, _, errb := realEnv(`{"session_id":"s","cwd":"/w","prompt":"hi"}`)

			require.Equal(t, 0, dispatch(context.Background(), e, argv), errb.String())
			require.Empty(t, errb.String())
			require.Empty(t, hostile.seen(), "the planted server must hear nothing")
			require.Equal(t, []string{strings.TrimSpace("Bearer " + src.wantKey)}, trimAll(good.seen()))
		})
	}
}

// The MCP bridge reads config the same way: the one request it forwards goes to
// the caller's server.
func TestMCPProxy_IgnoresAPlantedWorkingDirConfig(t *testing.T) {
	for _, src := range configSources {
		t.Run(src.name, func(t *testing.T) {
			good, hostile, home := agentRepo(t)
			argv := append([]string{"mcp-proxy"}, src.setup(t, good, home)...)
			e, out, errb := realEnv(`{"jsonrpc":"2.0","id":1,"method":"ping"}` + "\n")

			require.Equal(t, 0, dispatch(context.Background(), e, argv), errb.String())
			require.Empty(t, hostile.seen(), "the planted server must hear nothing")
			require.Equal(t, []string{strings.TrimSpace("Bearer " + src.wantKey)}, trimAll(good.seen()))
			require.Contains(t, out.String(), `"result"`)
		})
	}
}

// mcp-headers hands Claude Code the bearer key, so a planted file must never be
// where it comes from: with no config of the caller's there is no key, which
// is an error, not the planted one.
func TestMCPHeaders_IgnoresAPlantedWorkingDirConfig(t *testing.T) {
	for _, src := range configSources {
		t.Run(src.name, func(t *testing.T) {
			good, _, home := agentRepo(t)
			argv := append([]string{"mcp-headers"}, src.setup(t, good, home)...)
			e, out, errb := realEnv("")

			code := dispatch(context.Background(), e, argv)
			require.NotContains(t, out.String()+errb.String(), "planted")
			if src.wantKey == "" {
				require.Equal(t, 1, code)
				require.Contains(t, errb.String(), "mcp.api_key is empty")
				return
			}
			require.Equal(t, 0, code, errb.String())
			require.Contains(t, out.String(), `"Authorization":"Bearer `+src.wantKey+`"`)
		})
	}
}

// The CLI verbs dial through the same loader.
func TestDial_IgnoresAPlantedWorkingDirConfig(t *testing.T) {
	good, hostile, _ := agentRepo(t)
	cli, cfg, err := newEnv().dial(context.Background())
	require.NoError(t, err, "the stand-in's result is enough of an initialize reply")
	require.NoError(t, cli.Close())
	require.Empty(t, cfg.SourcePath())
	require.Equal(t, good.URL, cfg.ServerURL())
	require.NotEmpty(t, good.seen())
	require.Empty(t, hostile.seen())
}

// seam doctor says which file it read, and names a working-directory file it
// did not.
func TestDoctor_NamesTheConfigAndAnIgnoredWorkingDirFile(t *testing.T) {
	_, _, home := agentRepo(t)
	e, out, _ := realEnv("")
	require.Equal(t, 1, dispatch(context.Background(), e, []string{"doctor"}))
	require.Contains(t, out.String(), "[ok  ] config: defaults + env (no $SEAMLESS_CONFIG and no ~/.config/seamless/seamless.yaml)")
	abs, err := filepath.Abs(config.WorkingDirConfig)
	require.NoError(t, err)
	require.Contains(t, out.String(), "[note] config: "+abs+" is not read")
	require.Contains(t, out.String(), "SEAMLESS_CONFIG="+abs)

	// Named outright, it is the file loaded, and there is nothing to note.
	t.Setenv("SEAMLESS_CONFIG", abs)
	e, out, _ = realEnv("")
	_ = dispatch(context.Background(), e, []string{"doctor"})
	require.Contains(t, out.String(), "[ok  ] config: loaded from "+abs)
	require.NotContains(t, out.String(), "[note]")

	// A home config is named as the source.
	t.Setenv("SEAMLESS_CONFIG", "")
	homeConfig := filepath.Join(home, ".config", "seamless", "seamless.yaml")
	require.NoError(t, os.MkdirAll(filepath.Dir(homeConfig), 0o700))
	require.NoError(t, os.WriteFile(homeConfig, []byte("mcp:\n  api_key: \"k\"\n"), 0o600))
	e, out, _ = realEnv("")
	_ = dispatch(context.Background(), e, []string{"doctor"})
	require.Contains(t, out.String(), "[ok  ] config: loaded from "+homeConfig)
	require.Contains(t, out.String(), "[note] config: "+abs+" is not read")
}

// No command in this binary may reach for the daemon's search: every load goes
// through env.loadConfig or dial, which are config.LoadClient.
func TestNoCommandCallsTheDaemonSearch(t *testing.T) {
	files, err := filepath.Glob("*.go")
	require.NoError(t, err)
	fset := token.NewFileSet()
	checked := 0
	for _, name := range files {
		if strings.HasSuffix(name, "_test.go") {
			continue
		}
		f, perr := parser.ParseFile(fset, name, nil, 0)
		require.NoError(t, perr)
		checked++
		ast.Inspect(f, func(n ast.Node) bool {
			sel, ok := n.(*ast.SelectorExpr)
			if !ok {
				return true
			}
			if pkg, ok := sel.X.(*ast.Ident); ok && pkg.Name == "config" && sel.Sel.Name == "Load" {
				t.Errorf("%s: config.Load reads ./seamless.yaml; the seam CLI loads through config.LoadClient (env.loadConfig, dial)", fset.Position(sel.Pos()))
			}
			return true
		})
	}
	require.Positive(t, checked)
}

// trimAll trims each header, so an empty key's "Bearer " compares as "Bearer".
func trimAll(in []string) []string {
	out := make([]string, len(in))
	for i, s := range in {
		out[i] = strings.TrimSpace(s)
	}
	return out
}
