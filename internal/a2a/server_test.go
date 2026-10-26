package a2a

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"errors"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store/storetest"
)

const testKey = "test-key"

// stubRecaller records the input it saw and returns canned hits or an error.
type stubRecaller struct {
	in    retrieve.RecallInput
	hits  []retrieve.Hit
	err   error
	calls int
}

func (s *stubRecaller) Recall(_ context.Context, in retrieve.RecallInput) ([]retrieve.Hit, error) {
	s.calls++
	s.in = in
	return s.hits, s.err
}

func newTestServer(t *testing.T, rec *stubRecaller, ev *events.Recorder) *httptest.Server {
	t.Helper()
	srv, err := New(Config{
		Retrieve: rec, Events: ev, APIKey: testKey,
		Version: "1.2.3", Endpoint: "http://127.0.0.1:8081/api/a2a",
	})
	require.NoError(t, err)
	ts := httptest.NewServer(srv.Handler())
	t.Cleanup(ts.Close)
	return ts
}

// rpc posts one JSON-RPC request and decodes the envelope.
func rpc(t *testing.T, url, key, body string) (json.RawMessage, *rpcError) {
	t.Helper()
	req, err := http.NewRequest(http.MethodPost, url, strings.NewReader(body))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/json")
	if key != "" {
		req.Header.Set("Authorization", "Bearer "+key)
	}
	resp, err := http.DefaultClient.Do(req)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)

	var env struct {
		JSONRPC string          `json:"jsonrpc"`
		Result  json.RawMessage `json:"result"`
		Error   *rpcError       `json:"error"`
	}
	require.NoError(t, json.NewDecoder(resp.Body).Decode(&env))
	require.Equal(t, "2.0", env.JSONRPC)
	return env.Result, env.Error
}

func sendBody(query string, metadata map[string]any) string {
	msg := map[string]any{
		"role":      "user",
		"kind":      "message",
		"messageId": "m1",
		"parts":     []map[string]any{{"kind": "text", "text": query}},
	}
	if metadata != nil {
		msg["metadata"] = metadata
	}
	raw, _ := json.Marshal(map[string]any{
		"jsonrpc": "2.0", "id": 1, "method": "message/send",
		"params": map[string]any{"message": msg},
	})
	return string(raw)
}

func TestHandler_AuthIsHTTPLevel(t *testing.T) {
	ts := newTestServer(t, &stubRecaller{}, nil)

	for _, key := range []string{"", "wrong-key"} {
		req, err := http.NewRequest(http.MethodPost, ts.URL, strings.NewReader(sendBody("q", nil)))
		require.NoError(t, err)
		if key != "" {
			req.Header.Set("Authorization", "Bearer "+key)
		}
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusUnauthorized, resp.StatusCode, "key %q", key)
		require.Equal(t, "Bearer", resp.Header.Get("WWW-Authenticate"))
	}
}

func TestHandler_MethodRouting(t *testing.T) {
	ts := newTestServer(t, &stubRecaller{}, nil)

	tests := []struct {
		method   string
		wantCode int
	}{
		{"message/stream", codeUnsupportedOp},
		{"tasks/resubscribe", codeUnsupportedOp},
		{"tasks/get", codeTaskNotFound},
		{"tasks/cancel", codeTaskNotFound},
		{"tasks/pushNotificationConfig/set", codePushNotSupported},
		{"tasks/pushNotificationConfig/get", codePushNotSupported},
		{"no/such/method", codeMethodNotFound},
	}
	for _, tt := range tests {
		t.Run(tt.method, func(t *testing.T) {
			body := `{"jsonrpc":"2.0","id":7,"method":"` + tt.method + `","params":{}}`
			_, rpcErr := rpc(t, ts.URL, testKey, body)
			require.NotNil(t, rpcErr)
			require.Equal(t, tt.wantCode, rpcErr.Code)
		})
	}
}

func TestMessageSend_MetadataRejectsEveryMalformedPresentValue(t *testing.T) {
	tests := []struct {
		name string
		meta map[string]any
		want string
	}{
		{"scope boolean", map[string]any{"scope": false}, "metadata.scope must be a string"},
		{"scope null", map[string]any{"scope": nil}, "metadata.scope must be a string"},
		{"scope number", map[string]any{"scope": 1}, "metadata.scope must be a string"},
		{"scope array", map[string]any{"scope": []any{"all"}}, "metadata.scope must be a string"},
		{"scope object", map[string]any{"scope": map[string]any{}}, "metadata.scope must be a string"},
		{"scope empty", map[string]any{"scope": ""}, "metadata.scope must be one of"},
		{"project boolean", map[string]any{"project": false}, "metadata.project must be a string"},
		{"project null", map[string]any{"project": nil}, "metadata.project must be a string"},
		{"project number", map[string]any{"project": 1}, "metadata.project must be a string"},
		{"project array", map[string]any{"project": []any{"demo"}}, "metadata.project must be a string"},
		{"project object", map[string]any{"project": map[string]any{}}, "metadata.project must be a string"},
		{"project empty", map[string]any{"project": ""}, "metadata.project must be a non-empty"},
		{"project traversal", map[string]any{"project": "../notes"}, "invalid project"},
		{"project reserved", map[string]any{"project": "all"}, "reserved"},
		{"limit boolean", map[string]any{"limit": false}, "metadata.limit must be an integer"},
		{"limit null", map[string]any{"limit": nil}, "metadata.limit must be an integer"},
		{"limit string", map[string]any{"limit": "5"}, "metadata.limit must be an integer"},
		{"limit array", map[string]any{"limit": []any{5}}, "metadata.limit must be an integer"},
		{"limit object", map[string]any{"limit": map[string]any{}}, "metadata.limit must be an integer"},
		{"limit fraction", map[string]any{"limit": 1.9}, "metadata.limit must be an integer"},
		{"limit zero", map[string]any{"limit": 0}, "metadata.limit must be between"},
		{"limit negative", map[string]any{"limit": -1}, "metadata.limit must be between"},
		{"limit too large", map[string]any{"limit": retrieve.MaxRecallLimit + 1}, "metadata.limit must be between"},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			stub := &stubRecaller{}
			ts := newTestServer(t, stub, nil)
			_, rpcErr := rpc(t, ts.URL, testKey, sendBody("q", tt.meta))
			require.NotNil(t, rpcErr)
			require.Equal(t, codeInvalidParams, rpcErr.Code)
			require.Contains(t, rpcErr.Message, tt.want)
			require.Zero(t, stub.calls, "invalid metadata must not reach retrieval")
		})
	}
}

func TestMessageSend_MetadataDefaultsAndBoundaries(t *testing.T) {
	for _, tc := range []struct {
		name        string
		meta        map[string]any
		wantProject string
		wantLimit   int
	}{
		{"absent defaults", nil, "", 0},
		{"minimum", map[string]any{"limit": 1}, "", 1},
		{"maximum", map[string]any{"limit": retrieve.MaxRecallLimit}, "", retrieve.MaxRecallLimit},
		{"global token", map[string]any{"project": "global"}, "", 0},
		{"named project", map[string]any{"project": "demo"}, "demo", 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			stub := &stubRecaller{}
			ts := newTestServer(t, stub, nil)
			_, rpcErr := rpc(t, ts.URL, testKey, sendBody("q", tc.meta))
			require.Nil(t, rpcErr)
			require.Equal(t, 1, stub.calls)
			require.Equal(t, tc.wantProject, stub.in.Project)
			require.Equal(t, tc.wantLimit, stub.in.Limit)
		})
	}
}

func TestHandler_MalformedRequests(t *testing.T) {
	ts := newTestServer(t, &stubRecaller{}, nil)

	t.Run("parse-error", func(t *testing.T) {
		_, rpcErr := rpc(t, ts.URL, testKey, `{not json`)
		require.NotNil(t, rpcErr)
		require.Equal(t, codeParse, rpcErr.Code)
	})
	t.Run("wrong-jsonrpc-version", func(t *testing.T) {
		_, rpcErr := rpc(t, ts.URL, testKey, `{"jsonrpc":"1.0","id":1,"method":"message/send"}`)
		require.NotNil(t, rpcErr)
		require.Equal(t, codeInvalidRequest, rpcErr.Code)
	})
	t.Run("no-text-parts", func(t *testing.T) {
		body := `{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"message":{"parts":[{"kind":"data","data":{}}]}}}`
		_, rpcErr := rpc(t, ts.URL, testKey, body)
		require.NotNil(t, rpcErr)
		require.Equal(t, codeInvalidParams, rpcErr.Code)
	})
	t.Run("bad-scope", func(t *testing.T) {
		_, rpcErr := rpc(t, ts.URL, testKey, sendBody("q", map[string]any{"scope": "everything"}))
		require.NotNil(t, rpcErr)
		require.Equal(t, codeInvalidParams, rpcErr.Code)
	})
	t.Run("get-is-405", func(t *testing.T) {
		resp, err := http.Get(ts.URL)
		require.NoError(t, err)
		_ = resp.Body.Close()
		require.Equal(t, http.StatusMethodNotAllowed, resp.StatusCode)
	})
}

func TestMessageSend_RepliesWithHits(t *testing.T) {
	stub := &stubRecaller{hits: []retrieve.Hit{
		{Kind: "memory", ID: "01A", Name: "chroma-boot-race", Description: "boot race gate", Project: "demo", Score: 0.5},
		{Kind: "note", ID: "01B", Name: "retro-notes", Title: "Retro notes", Description: "sprint retro", Score: 0.25},
	}}
	ts := newTestServer(t, stub, nil)

	result, rpcErr := rpc(t, ts.URL, testKey,
		sendBody("boot race", map[string]any{"project": "demo", "scope": "all", "limit": 5}))
	require.Nil(t, rpcErr)

	// The recall was scoped by the metadata.
	require.Equal(t, retrieve.RecallInput{Query: "boot race", Project: "demo", Scope: "all", Limit: 5}, stub.in)

	var msg Message
	require.NoError(t, json.Unmarshal(result, &msg))
	require.Equal(t, "message", msg.Kind)
	require.Equal(t, "agent", msg.Role)
	require.NotEmpty(t, msg.MessageID)
	require.NotEmpty(t, msg.ContextID, "the server mints a context id when the client sent none")
	require.Len(t, msg.Parts, 2)

	require.Equal(t, "text", msg.Parts[0].Kind)
	require.Contains(t, msg.Parts[0].Text, `2 hits for "boot race"`)
	require.Contains(t, msg.Parts[0].Text, "[memory] chroma-boot-race -- boot race gate (demo)")

	require.Equal(t, "data", msg.Parts[1].Kind)
	raw, err := json.Marshal(msg.Parts[1].Data)
	require.NoError(t, err)
	var data struct {
		Hits []retrieve.Hit `json:"hits"`
	}
	require.NoError(t, json.Unmarshal(raw, &data))
	require.Equal(t, stub.hits, data.Hits, "the data part is the MCP recall payload shape")
}

func TestMessageSend_EchoesContextID(t *testing.T) {
	ts := newTestServer(t, &stubRecaller{}, nil)

	body := `{"jsonrpc":"2.0","id":1,"method":"message/send","params":{"message":{` +
		`"contextId":"ctx-42","parts":[{"kind":"text","text":"anything"}]}}}`
	result, rpcErr := rpc(t, ts.URL, testKey, body)
	require.Nil(t, rpcErr)

	var msg Message
	require.NoError(t, json.Unmarshal(result, &msg))
	require.Equal(t, "ctx-42", msg.ContextID)
	require.Contains(t, msg.Parts[0].Text, "no hits")
}

func TestMessageSend_RecallErrorIsInternal(t *testing.T) {
	ts := newTestServer(t, &stubRecaller{err: errors.New("db exploded")}, nil)

	_, rpcErr := rpc(t, ts.URL, testKey, sendBody("q", nil))
	require.NotNil(t, rpcErr)
	require.Equal(t, codeInternal, rpcErr.Code)
	require.Contains(t, rpcErr.Message, "db exploded")
}

// eventRows reads all events of one kind with their decoded payloads.
func eventRows(t *testing.T, db *sql.DB, kind core.EventKind) []map[string]any {
	t.Helper()
	rows, err := db.Query(`SELECT project_slug, payload FROM events WHERE kind = ?`, string(kind))
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var out []map[string]any
	for rows.Next() {
		var project, payload string
		require.NoError(t, rows.Scan(&project, &payload))
		var p map[string]any
		require.NoError(t, json.Unmarshal([]byte(payload), &p))
		p["_project"] = project
		out = append(out, p)
	}
	require.NoError(t, rows.Err())
	return out
}

// TestMessageSend_RecordsRecallTelemetry: the A2A path must leave the same
// demand signals the MCP recall tool leaves -- retrieval.injected on a hit
// (source "recall", so utility scoring counts it as query-gated demand) and
// recall.miss on a zero-hit -- each tagged transport "a2a" for attribution.
func TestMessageSend_RecordsRecallTelemetry(t *testing.T) {
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })

	stub := &stubRecaller{hits: []retrieve.Hit{{Kind: "memory", ID: "01A", Name: "x", Score: 0.5}}}
	ts := newTestServer(t, stub, events.NewRecorder(db))

	_, rpcErr := rpc(t, ts.URL, testKey, sendBody("found query", map[string]any{"project": "demo"}))
	require.Nil(t, rpcErr)

	injected := eventRows(t, db, core.EventInjected)
	require.Len(t, injected, 1)
	require.Equal(t, "recall", injected[0]["source"])
	require.Equal(t, "a2a", injected[0]["transport"])
	require.Equal(t, "demo", injected[0]["_project"])
	require.Equal(t, []any{"01A"}, injected[0]["item_ids"])

	stub.hits = nil
	_, rpcErr = rpc(t, ts.URL, testKey, sendBody("missing query", nil))
	require.Nil(t, rpcErr)

	misses := eventRows(t, db, core.EventRecallMiss)
	require.Len(t, misses, 1)
	require.Equal(t, "missing query", misses[0]["query"])
	require.Equal(t, "recall", misses[0]["source"])
	require.Equal(t, "a2a", misses[0]["transport"])
}

func TestCardHandler_ServesCardUnauthenticated(t *testing.T) {
	srv, err := New(Config{Retrieve: &stubRecaller{}, APIKey: testKey,
		Version: "1.2.3", Endpoint: "http://127.0.0.1:8081/api/a2a"})
	require.NoError(t, err)
	ts := httptest.NewServer(srv.CardHandler())
	t.Cleanup(ts.Close)

	resp, err := http.Get(ts.URL)
	require.NoError(t, err)
	defer func() { _ = resp.Body.Close() }()
	require.Equal(t, http.StatusOK, resp.StatusCode)
	require.Contains(t, resp.Header.Get("Content-Type"), "application/json")

	var buf bytes.Buffer
	_, err = buf.ReadFrom(resp.Body)
	require.NoError(t, err)
	want, err := CardJSON("1.2.3", "http://127.0.0.1:8081/api/a2a")
	require.NoError(t, err)
	require.Equal(t, string(want), buf.String(), "the handler serves CardJSON's bytes verbatim")

	post, err := http.Post(ts.URL, "application/json", strings.NewReader("{}"))
	require.NoError(t, err)
	_ = post.Body.Close()
	require.Equal(t, http.StatusMethodNotAllowed, post.StatusCode)
}
