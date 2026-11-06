package main

// Authenticated HTTP to the console's JSON surface, for the owner-only actions
// that live there rather than on the MCP tool surface (force-releasing a task
// lock, approving a plan). The MCP path is dial + callTool in main.go.

import (
	"bytes"
	"encoding/json"
	"fmt"
	"io"
	"net/http"
	"time"

	"github.com/arctop/seamless/internal/config"
)

// consoleTimeout bounds a console JSON request. The console renders from local
// SQLite, so anything slower than this is a wedged daemon rather than a big page.
const consoleTimeout = 5 * time.Second

// consoleJSON fetches a console page as JSON, authenticating with the bearer key.
func consoleJSON(cfg config.Config, path string, v any) error {
	body, err := consoleRequest(cfg, http.MethodGet, path)
	if err != nil {
		return err
	}
	return decodeConsole(path, body, v)
}

// consoleRaw fetches a console page's JSON as the daemon sent it, for --json:
// printing these bytes rather than a struct decoded from them is what keeps a
// field this CLI does not know about from being dropped on the way to stdout.
// A body that is not JSON is refused here, as consoleJSON refuses it, so a pipe
// into jq never receives something other than seamlessd's answer.
func consoleRaw(cfg config.Config, path string) ([]byte, error) {
	return consoleRawRequest(cfg, http.MethodGet, path)
}

// consolePOSTRaw is consolePOST for --json: the action's response as sent.
func consolePOSTRaw(cfg config.Config, path string) ([]byte, error) {
	return consoleRawRequest(cfg, http.MethodPost, path)
}

func consoleRawRequest(cfg config.Config, method, path string) ([]byte, error) {
	body, err := consoleRequest(cfg, method, path)
	if err != nil {
		return nil, err
	}
	if !json.Valid(body) {
		return nil, fmt.Errorf("unreadable console response from %s: not JSON", path)
	}
	return body, nil
}

// consoleRequest performs one authenticated console request and returns the
// body of a 200. Every other answer is an error: a 404 is "not found", and any
// other status carries the handler's own message when it sent one.
func consoleRequest(cfg config.Config, method, path string) ([]byte, error) {
	req, err := http.NewRequest(method, cfg.ServerURL()+path, nil)
	if err != nil {
		return nil, err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.MCP.APIKey)
	req.Header.Set("Accept", "application/json")
	client, err := cfg.HTTPClient(consoleTimeout)
	if err != nil {
		return nil, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return nil, fmt.Errorf("console unreachable at %s: %w", cfg.DaemonAddress(), err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode == http.StatusNotFound {
		return nil, fmt.Errorf("not found")
	}
	if resp.StatusCode != http.StatusOK {
		// Surface the handler's message when it sent one. Without this the
		// console's 400 body is thrown away: it takes the trouble to write
		// `invalid status "bogus": valid values are ...` and the caller reads
		// `console returned 400 Bad Request`, which names neither the parameter
		// nor the values -- a loud server answer degraded into a useless client
		// one. A POST refusal (the task is not claimed) reads the same way.
		return nil, fmt.Errorf("%s", consoleError(resp))
	}
	body, err := io.ReadAll(resp.Body)
	if err != nil {
		return nil, fmt.Errorf("unreadable console response from %s: %w", path, err)
	}
	return body, nil
}

// decodeConsole decodes a console body into v. A Decoder rather than Unmarshal,
// as the streaming decode this replaced was: the first JSON value is the answer.
func decodeConsole(path string, body []byte, v any) error {
	if err := json.NewDecoder(bytes.NewReader(body)).Decode(v); err != nil {
		return fmt.Errorf("unreadable console response from %s: %w", path, err)
	}
	return nil
}

// consoleError renders a non-OK response: the handler's JSON error message when
// there is one, else the bare status. Shared by consoleJSON and consolePOST so
// the two cannot drift apart again.
func consoleError(resp *http.Response) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.NewDecoder(resp.Body).Decode(&e) == nil && e.Error != "" {
		return e.Error
	}
	return "console returned " + resp.Status
}

// consolePOST performs an authenticated POST to a console action endpoint and
// decodes the JSON response into v (v may be nil to ignore the body). It backs
// owner-only overrides (e.g. force-releasing a task lock) that live on the
// console surface rather than the MCP tools.
func consolePOST(cfg config.Config, path string, v any) error {
	body, err := consoleRequest(cfg, http.MethodPost, path)
	if err != nil {
		return err
	}
	if v == nil {
		return nil
	}
	return decodeConsole(path, body, v)
}
