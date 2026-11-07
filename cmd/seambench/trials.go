// Recording results as research-lab trials: the ONE deliberate live write in
// the whole benchmark.
//
// Everything else seambench touches is throwaway by construction -- its own
// base dir, its own homes, its own data dirs, its own ports. Results are the
// exception because they are durable knowledge: a version-over-version uplift
// number is worth more six months from now than the run tree it came from, and
// Seamless's own research-lab primitive is where durable expected-vs-actual
// records belong (it is also the dogfooding: trial_query and the console read
// these for free).
//
// Three rules keep that exception from becoming a hazard:
//
//  1. results.json is written BEFORE any trial. A run that already cost tokens
//     must never be lost because a live instance was down.
//  2. An unreachable or failing instance is a LOUD WARNING, not an error. The
//     report still prints and still exits 0; the warning says how to retry.
//  3. Recording is idempotent per run: the trial id is stored back into the
//     run's grade.json, so re-reporting the same tree records nothing twice.
//     (--regrade deliberately produces fresh trials: the verdict changed, so
//     it is a new observation.)
//
// The write goes over the ordinary MCP surface (the same tools any agent uses),
// never by opening the live SQLite file.

package main

import (
	"context"
	"encoding/json"
	"flag"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	mcpclient "github.com/mark3labs/mcp-go/client"
	"github.com/mark3labs/mcp-go/client/transport"
	"github.com/mark3labs/mcp-go/mcp"

	"github.com/arctop/seamless/internal/bench"
	"github.com/arctop/seamless/internal/config"
)

// trialLab is the research lab every seambench result lands in.
const trialLab = "seambench"

// defaultTrialProject scopes the trials. Results are knowledge about this repo,
// so they belong to this repo's project rather than to global -- global would
// inject benchmark trivia into every project's briefing forever.
const defaultTrialProject = "seamless"

// trialDialTimeout bounds the handshake with the live instance. A benchmark run
// has already taken minutes; waiting on a dead daemon must not add to that.
const trialDialTimeout = 15 * time.Second

// trialOpts is the report's live-instance target, as flags.
type trialOpts struct {
	url     string
	keyFile string
	project string
}

// addTrialFlags registers the trial-recording flags. The default target is the
// live instance from the ordinary config lookup; the overrides exist so a
// throwaway instance can be named explicitly -- which is how this is tested,
// since a test that wrote fixture trials into the owner's real store would be a
// mishap, not a test.
func addTrialFlags(fs *flag.FlagSet) *trialOpts {
	o := &trialOpts{}
	fs.StringVar(&o.url, "trials-url", "", "base URL of the instance to record trials in (default: the configured live instance)")
	fs.StringVar(&o.keyFile, "trials-key-file", "", "file holding that instance's API key (default: the configured key)")
	fs.StringVar(&o.project, "trials-project", defaultTrialProject, "project the trials are scoped to")
	return o
}

// trialTarget is a resolved instance to write into.
type trialTarget struct {
	url     string
	key     string
	project string
}

// resolve turns the flags into a target, falling back to the configured live
// instance. The key is read from a file rather than taken as a flag value so it
// never lands in a process list.
func (o *trialOpts) resolve() (trialTarget, error) {
	t := trialTarget{url: strings.TrimRight(o.url, "/"), project: strings.TrimSpace(o.project)}
	if t.project == "" {
		return t, fmt.Errorf("--trials-project is empty; trials need a scope (project=global would inject benchmark results into every project)")
	}
	if o.keyFile != "" {
		b, err := os.ReadFile(o.keyFile)
		if err != nil {
			return t, fmt.Errorf("read --trials-key-file: %w", err)
		}
		t.key = strings.TrimSpace(string(b))
	}
	if t.url != "" && t.key != "" {
		return t, nil
	}
	cfg, err := config.Load()
	if err != nil {
		return t, fmt.Errorf("load the Seamless config to find the live instance: %w", err)
	}
	if t.url == "" {
		t.url = cfg.ServerURL()
	}
	if t.key == "" {
		t.key = cfg.MCP.APIKey
	}
	if t.key == "" {
		return t, fmt.Errorf("no API key for %s: set mcp.api_key in the config or pass --trials-key-file", t.url)
	}
	return t, nil
}

// recordTrialsForReport records every not-yet-recorded run as a trial, and
// reports what happened on the report's own output. Its error return is
// reserved for a cancelled context: an instance that is down, unauthenticated,
// or refusing writes produces a warning and a zero exit, because the results
// this would have recorded are already on disk.
func recordTrialsForReport(ctx context.Context, o *trialOpts, root string, res bench.Results, w io.Writer) error {
	target, err := o.resolve()
	if err != nil {
		warnTrials(w, root, err)
		return nil
	}
	rec, err := dialTrials(ctx, target)
	if err != nil {
		warnTrials(w, root, err)
		return nil
	}
	defer rec.close()

	recorded, skipped, failed := 0, 0, 0
	var firstErr error
	for _, run := range res.Runs {
		if err := ctx.Err(); err != nil {
			return err
		}
		if run.Grade.TrialID != "" {
			skipped++
			continue
		}
		id, err := rec.record(ctx, run, res)
		if err != nil {
			failed++
			if firstErr == nil {
				firstErr = err
			}
			continue
		}
		recorded++
		// Stamp the run dir so a re-report does not record it again. A failure
		// here costs an idempotency marker, not the trial.
		run.Grade.TrialID = id
		dir := filepath.Join(root, filepath.FromSlash(run.Dir))
		if err := bench.WriteGrade(dir, run.Grade); err != nil {
			fmt.Fprintf(w, "trials: WARNING -- recorded trial %s but could not stamp %s: %v\n", id, dir, err)
		}
	}
	fmt.Fprintf(w, "trials: %d recorded in lab %q at %s (project %s)", recorded, trialLab, target.url, target.project)
	if skipped > 0 {
		fmt.Fprintf(w, ", %d already recorded", skipped)
	}
	if failed > 0 {
		fmt.Fprintf(w, ", %d FAILED", failed)
	}
	fmt.Fprintln(w)
	if firstErr != nil {
		warnTrials(w, root, firstErr)
	}
	return nil
}

// warnTrials says loudly that the durable half did not happen, and how to
// repeat just that half.
func warnTrials(w io.Writer, root string, err error) {
	fmt.Fprintf(w, "\ntrials: WARNING -- results were NOT recorded in the live instance: %v\n", err)
	fmt.Fprintf(w, "        results.json is written and every run dir is intact; nothing was lost.\n")
	fmt.Fprintf(w, "        Retry with: seambench report --out %s   (already-recorded runs are skipped)\n", root)
}

// trialRecorder is one MCP connection to the target instance.
type trialRecorder struct {
	cli     *mcpclient.Client
	project string
}

// dialTrials connects, initializes, and opens the lab.
func dialTrials(ctx context.Context, t trialTarget) (*trialRecorder, error) {
	cli, err := mcpclient.NewStreamableHttpClient(t.url+"/api/mcp",
		transport.WithHTTPHeaders(map[string]string{"Authorization": "Bearer " + t.key}),
		transport.WithHTTPBasicClient(&http.Client{CheckRedirect: config.RefuseRedirect}))
	if err != nil {
		return nil, fmt.Errorf("build an MCP client for %s: %w", t.url, err)
	}
	dialCtx, cancel := context.WithTimeout(ctx, trialDialTimeout)
	defer cancel()
	if err := cli.Start(dialCtx); err != nil {
		return nil, fmt.Errorf("connect to %s: %w", t.url, err)
	}
	var init mcp.InitializeRequest
	init.Params.ProtocolVersion = mcp.LATEST_PROTOCOL_VERSION
	init.Params.ClientInfo = mcp.Implementation{Name: "seambench", Version: "0"}
	if _, err := cli.Initialize(dialCtx, init); err != nil {
		_ = cli.Close()
		return nil, fmt.Errorf("connect to seamlessd at %s: %w", t.url, err)
	}
	r := &trialRecorder{cli: cli, project: t.project}
	if _, err := r.call(dialCtx, "lab_open", map[string]any{
		"lab":  trialLab,
		"goal": "agent-scenario benchmark results: with-vs-without uplift per scenario x condition, and version-over-version deltas",
	}); err != nil {
		_ = cli.Close()
		return nil, fmt.Errorf("open the %s lab at %s: %w", trialLab, t.url, err)
	}
	return r, nil
}

func (r *trialRecorder) close() { _ = r.cli.Close() }

// record writes one run as a trial and returns its id.
func (r *trialRecorder) record(ctx context.Context, run bench.RunResult, res bench.Results) (string, error) {
	args := trialArgs(run, res)
	args["lab"] = trialLab
	// The scope is passed explicitly: this connection binds no session, and a
	// durable write with nothing to infer from is rejected rather than guessed.
	args["project"] = r.project

	out, err := r.call(ctx, "trial_record", args)
	if err != nil {
		return "", err
	}
	id, _ := out["id"].(string)
	if id == "" {
		return "", fmt.Errorf("trial_record returned no id for %s", args["title"])
	}
	return id, nil
}

// call invokes a tool and decodes its JSON result.
func (r *trialRecorder) call(ctx context.Context, name string, args map[string]any) (map[string]any, error) {
	res, err := r.cli.CallTool(ctx, mcp.CallToolRequest{Params: mcp.CallToolParams{Name: name, Arguments: args}})
	if err != nil {
		return nil, fmt.Errorf("call %s: %w", name, err)
	}
	var text string
	for _, c := range res.Content {
		if tc, ok := c.(mcp.TextContent); ok {
			text = tc.Text
			break
		}
	}
	if res.IsError {
		return nil, fmt.Errorf("%s: %s", name, text)
	}
	var out map[string]any
	if text != "" {
		if err := json.Unmarshal([]byte(text), &out); err != nil {
			return nil, fmt.Errorf("unreadable %s response: %w", name, err)
		}
	}
	return out, nil
}

// trialArgs renders one graded run as trial_record arguments.
//
// The {version, condition, scenario, run} tags go into METRICS rather than into
// the title, because metrics are exactly-matchable: trial_query
// metrics_filter={"scenario":"auth-refresh","condition":"mechanism"} is then a
// native query over the whole history instead of a text search.
func trialArgs(run bench.RunResult, res bench.Results) map[string]any {
	rec := run.Record
	metrics := map[string]any{
		"scenario":  rec.Scenario,
		"condition": rec.Condition.Name,
		"profile":   string(rec.Condition.Profile),
		"client":    string(rec.Condition.Client),
		"version":   rec.Version,
		"run":       rec.Run,
		"status":    string(run.Grade.Status),
		"pass":      run.Grade.Status == bench.StatusGraded && run.Grade.Pass,
	}
	if rec.Model != "" {
		metrics["model"] = rec.Model
	}
	for name, v := range run.Metrics().Fields() {
		metrics[name] = v
	}

	changes := fmt.Sprintf("condition %s (profile %s, client %s) at version %s",
		rec.Condition.Name, rec.Condition.Profile, rec.Condition.Client, rec.Version)
	if rec.Model != "" {
		changes += ", model " + rec.Model
	}
	if res.Control != "" && rec.Condition.Name != res.Control {
		changes += "; control arm is " + res.Control
	}

	return map[string]any{
		"title":   fmt.Sprintf("%s / %s @ %s run %d", rec.Scenario, rec.Condition.Name, rec.Version, rec.Run),
		"changes": changes,
		"expected": fmt.Sprintf("the %s scenario's gating checks pass: the repo-state assertions, "+
			"and (on a Seamless-ful arm) no mechanism defect", rec.Scenario),
		"actual":  trialActual(run),
		"outcome": string(trialOutcome(run)),
		"metrics": metrics,
	}
}

// trialOutcome maps a run's status onto the trial vocabulary. A run that failed
// to run or could not be graded is INCONCLUSIVE, never fail: recording an
// infrastructure flake as a failure is how a later reader mistakes it for a
// regression -- the same rule the pass-rate follows.
func trialOutcome(run bench.RunResult) string {
	switch {
	case run.Grade.Status != bench.StatusGraded:
		return "inconclusive"
	case run.Grade.Pass:
		return "pass"
	default:
		return "fail"
	}
}

// trialActual is the observed result: the verdict plus the gating checks that
// moved it, or why there is no verdict.
func trialActual(run bench.RunResult) string {
	switch run.Grade.Status {
	case bench.StatusFailedToRun:
		return "the run itself failed, so there is no verdict: " + run.Grade.Error
	case bench.StatusUngradeable:
		return "the run left no gradeable evidence: " + run.Grade.Error
	}
	verdict := "FAIL"
	if run.Grade.Pass {
		verdict = "PASS"
	}
	var failed []string
	for _, d := range run.Grade.Details {
		if strings.Contains(d, "/gate:") && strings.Contains(d, "-- FAIL") {
			failed = append(failed, d)
		}
	}
	if len(failed) == 0 {
		return verdict
	}
	if len(failed) > 5 {
		failed = append(failed[:5], fmt.Sprintf("(+%d more)", len(failed)-5))
	}
	return verdict + " -- failing gates: " + strings.Join(failed, " | ")
}
