// The recall path: how a scenario whose signal is the mid-session
// <seam-recall> injection gets measured at all.
//
// Headless `claude -p` fires SessionStart but NOT UserPromptSubmit (memory
// headless-cc-p-skips-userpromptsubmit-hook), so the passive recall injection
// never lands in a -p take: such a run would silently measure the absence of a
// mechanism rather than the mechanism. Two ways out were on the table:
//
//	(a) drive an interactive PTY session, which does fire UserPromptSubmit --
//	    but Claude Code flushes its transcript only on a clean exit, and the
//	    driver has to handle the bypass-permissions dialog and any mid-task
//	    menus. Fragile, and still on the table if a full-agent recall scenario
//	    is ever needed.
//	(b) validate the mechanism at the hook API: POST the arm's live daemon the
//	    same UserPromptSubmit body Claude Code would, and assert the
//	    <seam-recall> payload comes back.
//
// This implements (b), recorded as an ordinary run artifact so the report and
// the grader see one uniform shape. No scenario sets RequiresRecall today, so
// it is deliberately the small version: one POST, one assertion, the exchange
// written to the run's agent.log.

package main

import (
	"bytes"
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"os"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/bench"
)

// recallMarker is the opening tag of the prompt-recall injection.
const recallMarker = "<seam-recall>"

// recallCheck POSTs the arm's UserPromptSubmit hook with the scenario prompt
// and asserts the recall block came back.
func recallCheck(ctx context.Context, a *arm, prompt, logPath string) stepOutcome {
	logFile, err := os.Create(logPath)
	if err != nil {
		return stepOutcome{exitCode: -1, err: fmt.Errorf("create agent log %s: %w", logPath, err)}
	}
	defer func() { _ = logFile.Close() }()

	if !a.seamless {
		// A vanilla arm has no daemon, so there is no injection to check. The
		// cell is recorded as a failed run rather than silently skipped: an
		// absent run dir reads as "the suite lost a cell", which is worse than
		// a run that says exactly why it could not be measured. (A real
		// RequiresRecall scenario may want the report to model this as its own
		// "not applicable" state instead.)
		err := fmt.Errorf("the recall component check needs a Seamless daemon; condition %s has none", a.condition.Name)
		fmt.Fprintf(logFile, "%v\n", err)
		return stepOutcome{exitCode: -1, err: err}
	}

	key, err := a.key()
	if err != nil {
		return stepOutcome{exitCode: -1, err: err}
	}
	body, err := json.Marshal(map[string]string{
		"session_id": "seambench-recall",
		"cwd":        a.repo,
		"prompt":     prompt,
	})
	if err != nil {
		return stepOutcome{exitCode: -1, err: fmt.Errorf("marshal hook body: %w", err)}
	}

	started := time.Now()
	fmt.Fprintf(logFile, "POST %s/api/hooks/user-prompt-submit\n%s\n\n", a.url, body)

	req, err := http.NewRequestWithContext(ctx, http.MethodPost,
		a.url+"/api/hooks/user-prompt-submit", bytes.NewReader(body))
	if err != nil {
		return stepOutcome{exitCode: -1, err: fmt.Errorf("build hook request: %w", err)}
	}
	req.Header.Set("Authorization", "Bearer "+key)
	req.Header.Set("Content-Type", "application/json")

	resp, err := (&http.Client{Timeout: 30 * time.Second}).Do(req)
	if err != nil {
		return stepOutcome{exitCode: -1, err: fmt.Errorf("post user-prompt-submit hook: %w", err)}
	}
	defer func() { _ = resp.Body.Close() }()

	var out struct {
		HookSpecificOutput struct {
			AdditionalContext string `json:"additionalContext"`
		} `json:"hookSpecificOutput"`
	}
	dec := json.NewDecoder(resp.Body)
	decErr := dec.Decode(&out)
	injected := out.HookSpecificOutput.AdditionalContext

	fmt.Fprintf(logFile, "%s\n%s\n", resp.Status, injected)
	metrics := bench.Metrics{DurationMS: time.Since(started).Milliseconds()}

	switch {
	case resp.StatusCode != http.StatusOK:
		return stepOutcome{exitCode: 1, metrics: metrics,
			err: fmt.Errorf("user-prompt-submit hook returned %s", resp.Status)}
	case decErr != nil:
		return stepOutcome{exitCode: 1, metrics: metrics,
			err: fmt.Errorf("parse hook response: %w", decErr)}
	case !strings.Contains(injected, recallMarker):
		return stepOutcome{exitCode: 1, metrics: metrics,
			err: fmt.Errorf("no %s block in the hook response (see %s)", recallMarker, logPath)}
	}
	return stepOutcome{metrics: metrics}
}
