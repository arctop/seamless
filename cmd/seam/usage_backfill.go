package main

// seam usage backfill -- fill the usage ledger of the sessions whose Claude
// Code transcripts are still on this computer (harvest.UsageBackfillEndpoint;
// internal/hooks usage_backfill.go). This process reads every file, as the
// owner, with the daemon's own parser (harvest.ClaudeUsage, the subagents'
// transcripts beside each one included), and sends each session's rows; the
// daemon opens nothing. It runs over any server_url: the listing it asks for
// first is what tells it the daemon wants the rows, so it needs no guess.

import (
	"bytes"
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
	"strings"
	"time"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/harvest"
)

var usageBackfillCmd = spec("usage backfill", groupObservability,
	"fill the token usage ledger from the transcripts still on this computer",
	noArgs(), bindUsageBackfill, runUsageBackfill).
	withLong(`Fills the usage ledger -- each session's model tokens by quarter-hour, model and
thread -- for your sessions recorded on this computer whose Claude Code
transcript is still here: the sessions from before the ledger existed, and
those whose end never harvested them. Each session's rows come from its whole
transcript and the transcripts of the subagents it launched, read here, as
you, and sent to the daemon, which opens no file for it.

A transcript is the one the session's hooks recorded, read only when it is a
regular *.jsonl file inside Claude Code's own transcript folder
($CLAUDE_CONFIG_DIR/projects or ~/.claude/projects), or else the file there
named after the session's id. A session whose transcript is gone is left as it
is.

It writes nothing for a session that is still live (its own end harvests it), a
Codex session (each Stop already credits its rollout's running total), or one
whose ledger already holds exactly what its transcript says, so a second run
writes nothing. It is yours alone: a restricted account is refused.

--dry-run writes nothing and prints what a run would write.`)

type usageBackfillOpts struct {
	dryRun *bool
	asJSON *bool

	// httpClient builds the client every request goes through; nil is
	// cfg.HTTPClient(0). Not a flag: tests pass one.
	httpClient func(cfg config.Config) (*http.Client, error)
}

func bindUsageBackfill(fs *flag.FlagSet) *usageBackfillOpts {
	return &usageBackfillOpts{
		dryRun: fs.Bool("dry-run", false, "write nothing: print what a run would write"),
		asJSON: jsonFlag(fs, "print every session's outcome as JSON (for jq) instead of the text rendering"),
	}
}

// backfillRequestTimeout bounds one request of the backfill: the listing, or
// one session's rows, at most harvest.MaxUsageRows of them.
const backfillRequestTimeout = time.Minute

// The per-session outcomes the backfill reports beside the daemon's own
// (harvest.BackfillResult's words).
const (
	backfillCodex    = "codex"         // a Codex session: not backfilled
	backfillMissing  = "missing"       // no transcript of it on this computer
	backfillNoUsage  = "no_usage"      // a transcript with no model usage in it
	backfillTooLarge = "too_many_rows" // more rows than one push carries
	backfillRefused  = "refused"       // the daemon refused it, or did not answer
)

// backfillSessionOut is one session's outcome, as --json prints it.
type backfillSessionOut struct {
	ID         string          `json:"id"`
	Name       string          `json:"name"`
	Project    string          `json:"project"`
	Client     string          `json:"client"`
	Status     string          `json:"status"`
	Transcript string          `json:"transcript,omitempty"`
	Result     string          `json:"result"`
	Before     core.TokenUsage `json:"before"`
	After      core.TokenUsage `json:"after"`
	Error      string          `json:"error,omitempty"`
}

// backfillOut is the backfill's whole outcome, as --json prints it.
type backfillOut struct {
	Host       string               `json:"host"`
	DryRun     bool                 `json:"dryRun"`
	OtherHosts int                  `json:"otherHosts"`
	Locked     []string             `json:"locked,omitempty"`
	Sessions   []backfillSessionOut `json:"sessions"`
}

func runUsageBackfill(ctx context.Context, e *env, o *usageBackfillOpts, _ []string) error {
	cfg, err := e.loadConfig()
	if err != nil {
		return err
	}
	host := config.Hostname()
	if host == "" {
		return errors.New("this computer reports no hostname, so its sessions cannot be told from another computer's: nothing filled")
	}
	newClient := o.httpClient
	if newClient == nil {
		newClient = func(cfg config.Config) (*http.Client, error) { return cfg.HTTPClient(0) }
	}
	client, err := newClient(cfg)
	if err != nil {
		return err
	}
	var list harvest.BackfillList
	if err := backfillGet(ctx, client, cfg, host, &list); err != nil {
		return err
	}
	out := backfillOut{Host: list.Host, DryRun: *o.dryRun, OtherHosts: list.OtherHosts, Locked: list.Locked,
		Sessions: []backfillSessionOut{}}
	index := newTranscriptIndex(transcriptRoots(""))
	failed := 0
	for _, s := range list.Sessions {
		res := backfillSession(ctx, client, cfg, list.Host, s, index, *o.dryRun)
		if res.Result == backfillRefused {
			failed++
		}
		out.Sessions = append(out.Sessions, res)
	}
	if *o.asJSON {
		raw, err := json.MarshalIndent(out, "", "  ")
		if err != nil {
			return err
		}
		fmt.Fprintln(e.stdout, string(raw))
	} else {
		printBackfill(e.stdout, out)
	}
	if failed > 0 {
		return fmt.Errorf("%d sessions were not filled (see above)", failed)
	}
	return nil
}

// backfillSession reads one listed session's transcript and sends its rows:
// what became of it.
func backfillSession(ctx context.Context, client *http.Client, cfg config.Config, host string, s harvest.BackfillSession,
	index *transcriptIndex, dryRun bool) backfillSessionOut {
	out := backfillSessionOut{ID: s.ID, Name: s.Name, Project: s.Project, Client: s.Client, Status: s.Status,
		Before: s.Tokens, After: s.Tokens}
	switch {
	case s.Client == hookClientCodex:
		out.Result = backfillCodex
		return out
	case s.Status == string(core.SessionActive):
		out.Result = harvest.BackfillActive
		return out
	}
	out.Transcript = index.find(s.TranscriptPath, s.SessionID)
	if out.Transcript == "" {
		out.Result = backfillMissing
		return out
	}
	totals, rows, ok := harvest.ClaudeUsage(out.Transcript)
	switch {
	case !ok:
		out.Result = backfillNoUsage
		return out
	case len(rows) > harvest.MaxUsageRows:
		out.Result = backfillTooLarge
		return out
	}
	push := harvest.Push{Version: harvest.PushVersion, Tokens: &totals, Usage: rows}
	if err := push.Validate(); err != nil {
		out.Result, out.Error = backfillRefused, err.Error()
		return out
	}
	res, err := backfillPost(ctx, client, cfg, host, s, push, dryRun)
	if err != nil {
		out.Result, out.Error = backfillRefused, err.Error()
		return out
	}
	out.Result, out.Before, out.After = res.Result, res.Before, res.After
	return out
}

// backfillGet asks the daemon for this computer's sessions.
func backfillGet(ctx context.Context, client *http.Client, cfg config.Config, host string, list *harvest.BackfillList) error {
	status, body, err := backfillDo(ctx, client, cfg, http.MethodGet,
		harvest.UsageBackfillEndpoint+"?"+url.Values{"host": {host}}.Encode(), nil)
	if err != nil {
		return err
	}
	switch status {
	case http.StatusOK:
	case http.StatusNotFound:
		// The route answers every request it serves with JSON and never 404:
		// this daemon has no such route.
		return fmt.Errorf("the daemon at %s has no usage backfill: it is older than this seam", cfg.DaemonAddress())
	default:
		return errors.New(backfillError(status, body))
	}
	if err := json.Unmarshal(body, list); err != nil {
		return fmt.Errorf("unreadable answer from %s: %w", harvest.UsageBackfillEndpoint, err)
	}
	return nil
}

// backfillPost sends one session's rows: the daemon's answer, or why there is
// none.
func backfillPost(ctx context.Context, client *http.Client, cfg config.Config, host string, s harvest.BackfillSession,
	push harvest.Push, dryRun bool) (harvest.BackfillResult, error) {
	body, err := json.Marshal(map[string]any{"session_id": s.SessionID, "dry_run": dryRun, harvest.Field: push})
	if err != nil {
		return harvest.BackfillResult{}, err
	}
	q := url.Values{"host": {host}}
	if s.Client != "" {
		q.Set(clientQueryParam, s.Client)
	}
	status, raw, err := backfillDo(ctx, client, cfg, http.MethodPost, harvest.UsageBackfillEndpoint+"?"+q.Encode(), body)
	if err != nil {
		return harvest.BackfillResult{}, err
	}
	if status != http.StatusOK {
		return harvest.BackfillResult{}, errors.New(backfillError(status, raw))
	}
	var res harvest.BackfillResult
	if err := json.Unmarshal(raw, &res); err != nil || res.Result == "" {
		return harvest.BackfillResult{}, fmt.Errorf("unreadable answer from %s", harvest.UsageBackfillEndpoint)
	}
	return res, nil
}

// backfillDo makes one request of the backfill and returns the status and the
// body (at most maxErrorBody past a non-2xx status).
func backfillDo(ctx context.Context, client *http.Client, cfg config.Config, method, path string, body []byte) (int, []byte, error) {
	ctx, cancel := context.WithTimeout(ctx, backfillRequestTimeout)
	defer cancel()
	var reader io.Reader
	if body != nil {
		reader = bytes.NewReader(body)
	}
	req, err := http.NewRequestWithContext(ctx, method, cfg.ServerURL()+path, reader)
	if err != nil {
		return 0, nil, err
	}
	if key := cfg.MCP.APIKey; key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	req.Header.Set("Accept", "application/json")
	if body != nil {
		req.Header.Set("Content-Type", "application/json")
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, nil, fmt.Errorf("daemon unreachable at %s: %w", cfg.DaemonAddress(), err)
	}
	defer func() { _ = resp.Body.Close() }()
	limit := int64(64 << 20) // a listing of every session on this computer
	if resp.StatusCode < 200 || resp.StatusCode >= 300 {
		limit = maxErrorBody
	}
	raw, err := io.ReadAll(io.LimitReader(resp.Body, limit))
	if err != nil {
		return resp.StatusCode, nil, fmt.Errorf("read answer: %w", err)
	}
	return resp.StatusCode, raw, nil
}

// backfillError is a refusal's text: the daemon's own words when it sent
// them, else the status.
func backfillError(status int, body []byte) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal(body, &e) == nil && e.Error != "" {
		return e.Error
	}
	if msg := strings.TrimSpace(string(body)); msg != "" && len(msg) < 300 {
		return fmt.Sprintf("seamlessd returned %d: %s", status, msg)
	}
	return fmt.Sprintf("seamlessd returned %d", status)
}

// printBackfill prints the backfill's outcome: the counts, then a line for
// every session it filled (or would), and every refusal.
func printBackfill(w io.Writer, out backfillOut) {
	count := map[string]int{}
	var before, after core.TokenUsage
	for _, s := range out.Sessions {
		count[s.Result]++
		if s.Result == harvest.BackfillWritten || s.Result == harvest.BackfillWouldWrite {
			before, after = addTokens(before, s.Before), addTokens(after, s.After)
		}
	}
	verb, wrote := "filled", count[harvest.BackfillWritten]
	head := "usage backfill on " + out.Host
	if out.DryRun {
		verb, wrote, head = "would fill", count[harvest.BackfillWouldWrite], head+" (dry run: nothing written)"
	}
	found := len(out.Sessions) - count[backfillCodex] - count[harvest.BackfillActive] - count[backfillMissing]
	fmt.Fprintln(w, head)
	fmt.Fprintf(w, "  sessions on this computer: %d", len(out.Sessions))
	if out.OtherHosts > 0 {
		fmt.Fprintf(w, " (%d more recorded on other computers)", out.OtherHosts)
	}
	fmt.Fprintln(w)
	fmt.Fprintf(w, "  with a transcript here:    %d (%d missing)\n", found, count[backfillMissing])
	fmt.Fprintf(w, "  skipped:                   %d live, %d Codex\n", count[harvest.BackfillActive], count[backfillCodex])
	fmt.Fprintf(w, "  already filled:            %d\n", count[harvest.BackfillUnchanged])
	fmt.Fprintf(w, "  %-26s %d\n", verb+":", wrote)
	if n := count[backfillNoUsage] + count[backfillTooLarge]; n > 0 {
		fmt.Fprintf(w, "  not fillable:              %d (%d with no usage, %d past the %d rows a push carries)\n",
			n, count[backfillNoUsage], count[backfillTooLarge], harvest.MaxUsageRows)
	}
	if wrote > 0 {
		fmt.Fprintf(w, "  model tokens of those:     %d before, %d after\n", before.Total, after.Total)
	}
	if len(out.Locked) > 0 {
		fmt.Fprintf(w, "  not listed (locked):       %s\n", strings.Join(out.Locked, ", "))
	}
	for _, s := range out.Sessions {
		switch s.Result {
		case harvest.BackfillWritten, harvest.BackfillWouldWrite:
			fmt.Fprintf(w, "  %s %-24s %d -> %d tokens\n", verb, sessionName(s.Name, s.ID), s.Before.Total, s.After.Total)
		case backfillRefused:
			fmt.Fprintf(w, "  not filled %s: %s\n", sessionName(s.Name, s.ID), s.Error)
		}
	}
}

// addTokens is a plus b, field by field, its total the sum of its parts.
func addTokens(a, b core.TokenUsage) core.TokenUsage {
	a.Input += b.Input
	a.Cached += b.Cached
	a.CacheCreation += b.CacheCreation
	a.Output += b.Output
	a.Normalize()
	return a
}

// transcriptIndex finds a session's Claude Code transcript under the folders
// Claude Code keeps them in (transcriptRoots): the path its hooks recorded
// when it is one of them, else the file named after its session id in one of
// their project folders. The folders are read once, when first needed, and
// nothing is followed: a project folder that is a link, or a transcript that
// is not a regular file, is not looked in or taken.
type transcriptIndex struct {
	roots []string
	byID  map[string][]string // session id -> the transcripts named after it
	built bool
}

func newTranscriptIndex(roots []string) *transcriptIndex { return &transcriptIndex{roots: roots} }

// find is the transcript of the session whose hooks recorded recorded ("" for
// none) and whose agent client's session id is id: "" when there is none
// here, or more than one file is named after it.
func (x *transcriptIndex) find(recorded, id string) string {
	if recorded != "" && x.take(recorded) {
		return recorded
	}
	if !x.built {
		x.build()
	}
	found := ""
	for _, path := range x.byID[id] {
		if !x.take(path) {
			continue
		}
		if found != "" {
			return "" // two transcripts named after one session: neither is taken
		}
		found = path
	}
	return found
}

// take reports whether path is a transcript this account may read for a
// backfill: a regular *.jsonl file inside one of the roots, as
// openNamedTranscript judges a path the daemon named.
func (x *transcriptIndex) take(path string) bool {
	f, _, refusal := openNamedTranscript(path, x.roots)
	if refusal != "" {
		return false
	}
	_ = f.Close()
	return true
}

// build reads every project folder of every root, once.
func (x *transcriptIndex) build() {
	x.built, x.byID = true, map[string][]string{}
	for _, root := range x.roots {
		projects, err := os.ReadDir(root)
		if err != nil {
			continue
		}
		for _, p := range projects {
			if !p.IsDir() { // a link reports itself, not its target, and is no folder
				continue
			}
			dir := filepath.Join(root, p.Name())
			files, err := os.ReadDir(dir)
			if err != nil {
				continue
			}
			for _, f := range files {
				name := f.Name()
				if !f.Type().IsRegular() || filepath.Ext(name) != ".jsonl" {
					continue
				}
				id := strings.TrimSuffix(name, ".jsonl")
				x.byID[id] = append(x.byID[id], filepath.Join(dir, name))
			}
		}
	}
}
