package main

// seam hook-transcript -- the transcript upload of a flagged session, under
// peer_auth, as a process of its own. `seam hook` starts it, detached from
// the agent (spawnDetached), when the daemon's answer asks for a transcript
// (harvest.TranscriptWantedHeader at a flagged session's end signal,
// harvest.TranscriptPendingHeader for a pending one), and returns at once: a
// transcript runs to hundreds of megabytes, far past the few seconds a hook
// has before its client kills it. This process has
// harvest.TranscriptUploadTimeout instead. No installer wires it, and nobody
// runs it by hand; its stdio is the null device, and what became of the copy
// is the daemon's record (seam sessions --flagged).
//
// The rule of every client-side capture holds (hook_capture.go): it sends
// only to the peer_auth socket. A path the daemon named (--named: a pending
// session's recorded path) is read only when it is a regular *.jsonl file
// inside the agent client's own transcript folder (openNamedTranscript); any
// other is reported, never read, and the daemon stops asking for it.

import (
	"compress/gzip"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"path/filepath"
	"strconv"
	"strings"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/harvest"
)

// hookTranscriptOpts carries the flags for `seam hook-transcript`.
type hookTranscriptOpts struct {
	config    string // --config, as the hook had it
	client    string // --client, as the hook had it; "" is Claude Code
	sessionID string // --session-id: the agent client's own session id
	path      string // --path: the transcript to send
	wanted    int64  // --wanted: send only when the file is larger than this
	named     bool   // --named: the daemon named the path (a pending session)

	// httpClient builds the client the upload is sent through; nil is
	// cfg.HTTPClient(0). Not a flag: tests pass one.
	httpClient func(cfg config.Config) (*http.Client, error)
}

func bindHookTranscript(fs *flag.FlagSet) *hookTranscriptOpts {
	o := &hookTranscriptOpts{}
	fs.StringVar(&o.config, "config", "", "path to seamless.yaml, as the hook was given it")
	fs.Var(&enumValue{val: &o.client, valid: hookClients}, "client",
		"agent CLI the transcript is from (`CLIENT`); valid: "+strings.Join(hookClients, ", ")+"; default Claude Code")
	fs.StringVar(&o.sessionID, "session-id", "", "the agent client's own session id (`ID`)")
	fs.StringVar(&o.path, "path", "", "the transcript file to send (`PATH`)")
	fs.Int64Var(&o.wanted, "wanted", 0, "send only when the file is larger than `BYTES`")
	fs.BoolVar(&o.named, "named", false, "the daemon named the path: read it only inside the client's own transcript folder")
	return o
}

var hookTranscriptCmd = spec("hook-transcript", groupHooks,
	"send a flagged session's transcript, detached (started by seam hook)",
	noArgs(), bindHookTranscript, runHookTranscript).
	withLong(`seam hook starts this, detached from the agent, when the daemon's answer to
a hook asks for a flagged session's transcript; no installer wires it, and it is
not run by hand. It sends the transcript gzip-compressed over the peer_auth
socket only, and gives up after ` + harvest.TranscriptUploadTimeout.String() + `. A path the daemon named
(--named) is read only when it is a regular *.jsonl file inside the agent
client's own transcript folder ($CLAUDE_CONFIG_DIR/projects or
~/.claude/projects for Claude Code, $CODEX_HOME/sessions or ~/.codex/sessions
for Codex); any other is reported to the daemon, never read.`)

func runHookTranscript(ctx context.Context, e *env, o *hookTranscriptOpts, _ []string) error {
	if o.config != "" {
		if err := os.Setenv("SEAMLESS_CONFIG", o.config); err != nil {
			fmt.Fprintln(e.stderr, "seam hook-transcript: set config path:", err)
			return nil
		}
	}
	cfg, err := e.loadConfig()
	if err != nil {
		fmt.Fprintln(e.stderr, "seam hook-transcript: load config:", err)
		return nil
	}
	if !pushesCaptures(cfg) || o.sessionID == "" || o.path == "" || o.wanted < 0 {
		return nil // only to the peer_auth socket, and only what a hook asked for
	}
	ctx, cancel := context.WithTimeout(ctx, harvest.TranscriptUploadTimeout)
	defer cancel()
	newClient := o.httpClient
	if newClient == nil {
		newClient = func(cfg config.Config) (*http.Client, error) { return cfg.HTTPClient(0) }
	}
	client, err := newClient(cfg)
	if err != nil {
		fmt.Fprintln(e.stderr, "seam hook-transcript:", err)
		return nil
	}
	q := url.Values{harvest.TranscriptSessionParam: {o.sessionID}}
	if o.client != "" {
		q.Set(clientQueryParam, o.client)
	}
	if host := config.Hostname(); host != "" {
		q.Set("host", host)
	}
	var (
		f    *os.File
		size int64
	)
	if o.named {
		var refusal string
		if f, size, refusal = openNamedTranscript(o.path, transcriptRoots(o.client)); refusal != "" {
			// Said, never read: the daemon records why and stops asking.
			q.Set(harvest.TranscriptRefusedParam, refusal)
			if _, answer, err := postTranscript(ctx, client, cfg, harvest.TranscriptEndpoint+"?"+q.Encode(), nil, 0); err != nil {
				fmt.Fprintln(e.stderr, "seam hook-transcript: report not sent:", err)
			} else if answer.Error != "" {
				fmt.Fprintln(e.stderr, "seam hook-transcript: report not taken:", answer.Error)
			}
			return nil
		}
	} else {
		if f, err = os.Open(o.path); err != nil { //nolint:gosec // the agent's own transcript, as its end signal named it
			fmt.Fprintln(e.stderr, "seam hook-transcript: transcript not sent:", err)
			return nil
		}
		fi, err := f.Stat()
		if err != nil || !fi.Mode().IsRegular() || fi.Size() <= o.wanted {
			_ = f.Close()
			return nil // nothing new since the copy kept, or nothing to send
		}
		size = fi.Size()
	}
	defer func() { _ = f.Close() }()
	status, answer, err := postTranscript(ctx, client, cfg, harvest.TranscriptEndpoint+"?"+q.Encode(), f, size)
	switch {
	case err != nil:
		fmt.Fprintln(e.stderr, "seam hook-transcript: transcript not sent:", err)
	case status != http.StatusOK:
		fmt.Fprintln(e.stderr, "seam hook-transcript: transcript not kept:", answer.Error)
	}
	return nil
}

// transcriptAnswer is the daemon's JSON answer to an upload, a report, or the
// owner's keep request: what became of the copy, why not, and -- to the
// owner's keep request under peer_auth -- the transcript to send.
type transcriptAnswer struct {
	Copy  string `json:"copy"`
	Error string `json:"error"`
	Send  *struct {
		Path   string `json:"path"`
		Host   string `json:"host"`
		Client string `json:"client"`
	} `json:"send"`
}

// postTranscript POSTs to path on the daemon: body, gzip-compressed as it is
// read and claimed size bytes, or no body when body is nil. It returns the
// status and the decoded answer (its error the status when the body says
// none).
func postTranscript(ctx context.Context, client *http.Client, cfg config.Config, path string, body io.Reader, size int64) (int, transcriptAnswer, error) {
	var reqBody io.Reader
	var pr *io.PipeReader
	if body != nil {
		// Compressed as it is read: the body is never held whole.
		var pw *io.PipeWriter
		pr, pw = io.Pipe()
		go func() {
			gz, gerr := gzip.NewWriterLevel(pw, gzip.BestSpeed)
			if gerr == nil {
				_, gerr = io.Copy(gz, body)
				gerr = errors.Join(gerr, gz.Close())
			}
			pw.CloseWithError(gerr) //nolint:errcheck // CloseWithError on a pipe writer always returns nil
		}()
		reqBody = pr
		defer func() { _ = pr.Close() }() // ends the compressing goroutine if the daemon answered before reading it all
	}
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, cfg.ServerURL()+path, reqBody)
	if err != nil {
		return 0, transcriptAnswer{}, err
	}
	if key := cfg.MCP.APIKey; key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	if body != nil {
		req.Header.Set("Content-Type", "application/gzip")
		req.Header.Set(harvest.TranscriptSizeHeader, strconv.FormatInt(size, 10))
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, transcriptAnswer{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	var answer transcriptAnswer
	if derr := json.NewDecoder(io.LimitReader(resp.Body, maxErrorBody)).Decode(&answer); derr != nil && resp.StatusCode == http.StatusOK {
		return resp.StatusCode, answer, fmt.Errorf("read answer: %w", derr)
	}
	if resp.StatusCode != http.StatusOK && answer.Error == "" {
		answer.Error = resp.Status
	}
	return resp.StatusCode, answer, nil
}

// transcriptRoots are the folders an agent client keeps its transcripts in,
// as this account's environment names them: Claude Code's projects folder
// under $CLAUDE_CONFIG_DIR, else ~/.claude; Codex's sessions folder under
// $CODEX_HOME, else ~/.codex. client is a --client value ("" is Claude Code).
// None when the folder cannot be named (no home, a relative override).
func transcriptRoots(client string) []string {
	env, def, sub := "CLAUDE_CONFIG_DIR", ".claude", "projects"
	if client == hookClientCodex {
		env, def, sub = "CODEX_HOME", ".codex", "sessions"
	}
	home, err := os.UserHomeDir()
	if err != nil {
		home = ""
	}
	dir := strings.TrimSpace(os.Getenv(env))
	switch {
	case dir == "" && home != "":
		dir = filepath.Join(home, def)
	case dir == "~" && home != "":
		dir = home
	case strings.HasPrefix(dir, "~/") && home != "":
		dir = filepath.Join(home, dir[2:])
	}
	if dir == "" || !filepath.IsAbs(dir) {
		return nil
	}
	return []string{filepath.Join(dir, sub)}
}

// openNamedTranscript opens a transcript path the daemon named, only when it
// is the agent client's own: an absolute, clean path to a *.jsonl file, a
// regular file and not a symlink, whose folder -- symlinks resolved -- lies
// inside one of roots (also resolved). The file opened is the one checked.
// refusal is "" when it opened, else harvest.TranscriptRefusedPath (not the
// client's transcript) or harvest.TranscriptRefusedMissing (not there, or not
// readable).
func openNamedTranscript(path string, roots []string) (f *os.File, size int64, refusal string) {
	if !filepath.IsAbs(path) || filepath.Clean(path) != path || filepath.Ext(path) != ".jsonl" {
		return nil, 0, harvest.TranscriptRefusedPath
	}
	fi, err := os.Lstat(path)
	if err != nil {
		if errors.Is(err, os.ErrNotExist) {
			return nil, 0, harvest.TranscriptRefusedMissing
		}
		return nil, 0, harvest.TranscriptRefusedPath
	}
	if !fi.Mode().IsRegular() {
		return nil, 0, harvest.TranscriptRefusedPath // a symlink, a folder, a device
	}
	dir, err := filepath.EvalSymlinks(filepath.Dir(path))
	if err != nil {
		return nil, 0, harvest.TranscriptRefusedPath
	}
	inside := false
	for _, root := range roots {
		if real, err := filepath.EvalSymlinks(root); err == nil && within(real, dir) {
			inside = true
			break
		}
	}
	if !inside {
		return nil, 0, harvest.TranscriptRefusedPath
	}
	f, err = os.Open(path) //nolint:gosec // checked above: a regular *.jsonl inside the client's own transcript folder
	if err != nil {
		return nil, 0, harvest.TranscriptRefusedMissing
	}
	opened, err := f.Stat()
	if err != nil || !os.SameFile(fi, opened) {
		// Swapped between the check and the open.
		_ = f.Close()
		return nil, 0, harvest.TranscriptRefusedPath
	}
	return f, opened.Size(), ""
}

// within reports whether dir is root or lies inside it (both clean).
func within(root, dir string) bool {
	rel, err := filepath.Rel(root, dir)
	return err == nil && rel != ".." && !strings.HasPrefix(rel, ".."+string(filepath.Separator)) && !filepath.IsAbs(rel)
}
