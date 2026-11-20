package main

import (
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
)

// The flag's flags parse, and the combinations that mean nothing are refused
// before anything is asked of the daemon.
func TestSessionsParse_FlagFlags(t *testing.T) {
	p, err := parse(commands(), []string{"sessions", "--flagged"})
	require.NoError(t, err)
	require.True(t, *p.opts.(*sessionsOpts).flagged)
	p, err = parse(commands(), []string{"sessions", "--verdict", "bad"})
	require.NoError(t, err)
	require.Equal(t, "bad", *p.opts.(*sessionsOpts).verdict)
	_, err = parse(commands(), []string{"sessions", "--verdict", "none"})
	require.Error(t, err, "none clears a flag: it lists nothing")
	require.Contains(t, err.Error(), "valid values are good, bad, notable")

	fx := newDaemonFixture(t, nil, map[string]string{}, "")
	for _, argv := range [][]string{
		{"sessions", "--transcript"},
		{"sessions", "01KHHHHHHHHHHHHHHHHHHHHHHH", "--transcript", "--json"},
		{"sessions", "01KHHHHHHHHHHHHHHHHHHHHHHH", "--flagged"},
	} {
		_, stderr, code := fx.run(t, argv...)
		require.Equal(t, 1, code, "%v: %s", argv, stderr)
	}
}

// --flagged lists every flagged session with its verdict and reason, the
// transcript file and whether it is still there -- looked for here, as the
// owner -- and the copy; the plain list marks a flagged row.
func TestSessions_FlaggedList(t *testing.T) {
	here := filepath.Join(t.TempDir(), "here.jsonl")
	require.NoError(t, os.WriteFile(here, []byte("{}\n"), 0o600))
	gone := filepath.Join(t.TempDir(), "gone.jsonl")
	rows := `{"total":3,"active":0,"sessions":[` +
		`{"id":"01KAAAAAAAAAAAAAAAAAAAAAAA","name":"cc/good","project":"example-project","status":"completed","verdict":"good","reason":"clean run","transcriptCopy":"copied","transcriptKept":true,"transcriptPath":"` + here + `","updated":"0001-01-01T00:00:00Z"},` +
		`{"id":"01KBBBBBBBBBBBBBBBBBBBBBBB","name":"cc/bad","project":"","status":"expired","verdict":"bad","transcriptCopy":"unreadable","transcriptPath":"` + gone + `","updated":"0001-01-01T00:00:00Z"},` +
		`{"id":"01KCCCCCCCCCCCCCCCCCCCCCCC","name":"cx/far","project":"example-project","status":"completed","verdict":"notable","host":"example-far-host","transcriptCopy":"remote","transcriptPath":"/far/rollout.jsonl","updated":"0001-01-01T00:00:00Z"}` +
		`]}`
	fx := newDaemonFixture(t, nil, map[string]string{
		"GET /console/sessions?format=json&w=all&verdict=flagged": rows,
		"GET /console/sessions?format=json&w=all&verdict=bad":     `{"total":3,"active":0,"sessions":[]}`,
		"GET /console/sessions?format=json":                       rows,
	}, "")

	out, stderr, code := fx.run(t, "sessions", "--flagged")
	require.Equal(t, 0, code, stderr)
	require.Contains(t, out, "3 flagged sessions\n")
	require.Contains(t, out, "cc/good  [good]  example-project  completed")
	require.Contains(t, out, "  reason: clean run\n")
	require.Contains(t, out, "  transcript: "+here+" (still there)\n")
	require.Contains(t, out, "  copy: kept; read it with `seam sessions 01KAAAAAAAAAAAAAAAAAAAAAAA --transcript`\n")
	require.Contains(t, out, "  transcript: "+gone+" (gone)\n")
	require.Contains(t, out, "  copy: no copy: the transcript file could not be read\n")
	require.Contains(t, out, "  transcript: /far/rollout.jsonl (on example-far-host; not looked for from here)\n")

	out, _, code = fx.run(t, "sessions", "--verdict", "bad")
	require.Equal(t, 0, code)
	require.Equal(t, "0 flagged sessions\n", out)

	out, _, code = fx.run(t, "sessions")
	require.Equal(t, 0, code)
	require.Contains(t, out, "cc/good              example-project completed - [flagged good]\n")
}

// The detail shows the flag; --transcript writes the copy to stdout as the
// daemon streams it.
func TestSessions_DetailAndTranscript(t *testing.T) {
	const id = "01KAAAAAAAAAAAAAAAAAAAAAAA"
	const jsonl = `{"type":"user"}` + "\n" + `{"type":"assistant"}` + "\n"
	fx := newDaemonFixture(t, nil, map[string]string{
		"GET /console/sessions/" + id + "?format=json": `{"session":{"id":"` + id + `","name":"cc/good","projectSlug":"example-project","status":"completed"},` +
			`"toolCalls":1,"flag":{"verdict":"good","reason":"clean run","transcriptPath":"/nowhere/x.jsonl","copy":"copied","kept":true}}`,
		"GET /console/sessions/" + id + "/transcript": jsonl,
	}, "")
	out, stderr, code := fx.run(t, "sessions", id)
	require.Equal(t, 0, code, stderr)
	require.Contains(t, out, "flagged: good -- clean run\n")
	require.Contains(t, out, "transcript: /nowhere/x.jsonl (gone)\n")
	require.Contains(t, out, "copy: kept; read it with `seam sessions "+id+" --transcript`\n")

	out, stderr, code = fx.run(t, "sessions", id, "--transcript")
	require.Equal(t, 0, code, stderr)
	require.Equal(t, jsonl, out, "the JSONL, byte for byte")

	_, stderr, code = fx.run(t, "sessions", "01KZZZZZZZZZZZZZZZZZZZZZZZ", "--transcript")
	require.Equal(t, 1, code)
	require.Contains(t, stderr, "no such route in the fixture", "the daemon's refusal is the error")
}

// A copy the daemon could not read whole is an error, after the part that
// arrived: never a cut-off conversation passed off as the whole one.
func TestSessionTranscript_DamagedCopyFails(t *testing.T) {
	srv := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, _ *http.Request) {
		w.Header().Set("Trailer", transcriptErrorTrailer)
		_, _ = w.Write([]byte(`{"type":"user"}` + "\n"))
		w.Header().Set(transcriptErrorTrailer, "the copy could not be read whole")
	}))
	t.Cleanup(srv.Close)
	cfg := config.Defaults()
	cfg.Addr = strings.TrimPrefix(srv.URL, "http://")
	var out strings.Builder
	err := sessionTranscript(&out, cfg, "01KAAAAAAAAAAAAAAAAAAAAAAA")
	require.Error(t, err)
	require.Contains(t, err.Error(), "the copy could not be read whole")
	require.Equal(t, `{"type":"user"}`+"\n", out.String())
}
