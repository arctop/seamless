package a2a

import (
	"context"
	"crypto/subtle"
	"encoding/json"
	"fmt"
	"log/slog"
	"math"
	"net/http"
	"slices"
	"strings"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/validate"
)

// JSON-RPC error codes: the standard four plus the A2A-specific range. Only
// the codes this server can actually emit are defined -- an unsupported
// operation on a capability the card already declares false is -32004, and the
// task codes exist because tasks/get on an agent that never creates tasks is
// still a well-formed question deserving the spec's answer.
const (
	codeParse          = -32700
	codeInvalidRequest = -32600
	codeMethodNotFound = -32601
	codeInvalidParams  = -32602
	codeInternal       = -32603

	codeTaskNotFound     = -32001
	codePushNotSupported = -32003
	codeUnsupportedOp    = -32004
)

// maxBodyBytes bounds a request body. A recall query is a sentence; a megabyte
// is already three orders of magnitude of slack.
const maxBodyBytes = 1 << 20

// recallMissQueryMax mirrors internal/mcp's bound on the query text stored in
// a recall.miss event: the miss log is telemetry, not a transcript.
const recallMissQueryMax = 500

// Recaller is the one retrieval capability this surface needs;
// *retrieve.Service satisfies it.
type Recaller interface {
	Recall(ctx context.Context, in retrieve.RecallInput) ([]retrieve.Hit, error)
}

// Config wires the A2A server's dependencies.
type Config struct {
	Retrieve Recaller
	Events   *events.Recorder // may be nil (recall demand is then not recorded)
	APIKey   string
	Version  string // build version served in the card; defaults to 0.0.0-dev
	Endpoint string // absolute URL the card advertises, e.g. http://127.0.0.1:8081/api/a2a
	Logger   *slog.Logger
}

// Server hosts the A2A JSON-RPC endpoint and its agent card.
type Server struct {
	cfg    Config
	logger *slog.Logger
	card   []byte
}

func New(cfg Config) (*Server, error) {
	card, err := CardJSON(cfg.Version, cfg.Endpoint)
	if err != nil {
		return nil, fmt.Errorf("a2a.New: %w", err)
	}
	logger := cfg.Logger
	if logger == nil {
		logger = slog.Default()
	}
	return &Server{cfg: cfg, logger: logger, card: card}, nil
}

// CardHandler serves the agent card. Public by design (RFC 8615 discovery, no
// auth): the card carries no secrets -- only the endpoint URL and the name of
// the auth scheme the endpoint itself will demand.
func (s *Server) CardHandler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodGet && r.Method != http.MethodHead {
			w.Header().Set("Allow", "GET, HEAD")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		w.Header().Set("Content-Type", "application/json; charset=utf-8")
		if _, err := w.Write(s.card); err != nil {
			s.logger.Warn("a2a: write card", "error", err)
		}
	})
}

// Handler serves the JSON-RPC endpoint. Auth failures are HTTP-level (401),
// per the A2A spec's transport-security model; everything after the bearer
// check speaks JSON-RPC.
func (s *Server) Handler() http.Handler {
	return http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.Method != http.MethodPost {
			w.Header().Set("Allow", "POST")
			http.Error(w, "method not allowed", http.StatusMethodNotAllowed)
			return
		}
		if !verifyBearer(r, s.cfg.APIKey) {
			w.Header().Set("WWW-Authenticate", "Bearer")
			http.Error(w, "unauthorized", http.StatusUnauthorized)
			return
		}

		var req struct {
			JSONRPC string          `json:"jsonrpc"`
			ID      json.RawMessage `json:"id"`
			Method  string          `json:"method"`
			Params  json.RawMessage `json:"params"`
		}
		if err := json.NewDecoder(http.MaxBytesReader(w, r.Body, maxBodyBytes)).Decode(&req); err != nil {
			s.writeError(w, nil, codeParse, "invalid JSON: "+err.Error())
			return
		}
		if req.JSONRPC != "2.0" {
			s.writeError(w, req.ID, codeInvalidRequest, `jsonrpc must be "2.0"`)
			return
		}

		switch req.Method {
		case "message/send":
			s.handleSend(w, r, req.ID, req.Params)
		case "message/stream", "tasks/resubscribe":
			s.writeError(w, req.ID, codeUnsupportedOp, "streaming is not supported (capabilities.streaming is false)")
		case "tasks/get", "tasks/cancel":
			s.writeError(w, req.ID, codeTaskNotFound, "this agent replies synchronously with a message and never creates tasks")
		case "tasks/pushNotificationConfig/set", "tasks/pushNotificationConfig/get",
			"tasks/pushNotificationConfig/list", "tasks/pushNotificationConfig/delete":
			s.writeError(w, req.ID, codePushNotSupported, "push notifications are not supported (capabilities.pushNotifications is false)")
		default:
			s.writeError(w, req.ID, codeMethodNotFound, "unknown method "+req.Method)
		}
	})
}

// Message is an A2A message. Only the fields this server reads or writes are
// declared; unknown incoming fields are ignored by encoding/json as usual.
type Message struct {
	Kind      string         `json:"kind"` // always "message"
	MessageID string         `json:"messageId"`
	ContextID string         `json:"contextId,omitempty"`
	Role      string         `json:"role"` // "agent" in replies
	Parts     []Part         `json:"parts"`
	Metadata  map[string]any `json:"metadata,omitempty"`
}

// Part is a message part; kind "text" carries Text, kind "data" carries Data.
type Part struct {
	Kind string         `json:"kind"`
	Text string         `json:"text,omitempty"`
	Data map[string]any `json:"data,omitempty"`
}

// handleSend is the recall skill: the message's text parts are the query, and
// optional metadata keys scope it the way the MCP recall tool's arguments do.
func (s *Server) handleSend(w http.ResponseWriter, r *http.Request, id json.RawMessage, rawParams json.RawMessage) {
	var params struct {
		Message Message `json:"message"`
	}
	if err := json.Unmarshal(rawParams, &params); err != nil {
		s.writeError(w, id, codeInvalidParams, "invalid params: "+err.Error())
		return
	}

	var texts []string
	for _, p := range params.Message.Parts {
		if p.Kind == "text" && strings.TrimSpace(p.Text) != "" {
			texts = append(texts, strings.TrimSpace(p.Text))
		}
	}
	query := strings.Join(texts, "\n")
	if query == "" {
		s.writeError(w, id, codeInvalidParams, "message has no text parts to use as the recall query")
		return
	}

	project, projectPresent, err := metaString(params.Message.Metadata, "project")
	if err != nil {
		s.writeError(w, id, codeInvalidParams, err.Error())
		return
	}
	if projectPresent {
		if project == "" {
			s.writeError(w, id, codeInvalidParams, "metadata.project must be a non-empty project slug or global")
			return
		}
		project, err = validate.Project(project)
		if err != nil {
			s.writeError(w, id, codeInvalidParams, "metadata.project: "+err.Error())
			return
		}
	}
	scope, scopePresent, err := metaString(params.Message.Metadata, "scope")
	if err != nil {
		s.writeError(w, id, codeInvalidParams, err.Error())
		return
	}
	// A configured peer_auth account that names no project recalls in its own
	// project (plus global), the scope every other surface places it in; with
	// no project named anyone else recalls the global scope, as before.
	if !projectPresent {
		if v, ok := access.FromContext(r.Context()); ok && v.Principal().Kind == access.KindUser {
			project = v.Home()
		}
	}
	if scopePresent && !slices.Contains(retrieve.RecallScopes, scope) {
		s.writeError(w, id, codeInvalidParams,
			"metadata.scope must be one of "+strings.Join(retrieve.RecallScopes, "|"))
		return
	}
	limit, _, err := metaInt(params.Message.Metadata, "limit", retrieve.MaxRecallLimit)
	if err != nil {
		s.writeError(w, id, codeInvalidParams, err.Error())
		return
	}

	hits, err := s.cfg.Retrieve.Recall(r.Context(), retrieve.RecallInput{
		Query: query, Project: project, Scope: scope, Limit: limit,
	})
	if err != nil {
		s.logger.Warn("a2a: recall", "error", err)
		s.writeError(w, id, codeInternal, "recall failed: "+err.Error())
		return
	}
	s.recordRecall(r.Context(), project, query, scope, limit, hits)

	reply, err := s.replyMessage(params.Message.ContextID, query, hits)
	if err != nil {
		s.writeError(w, id, codeInternal, err.Error())
		return
	}
	s.writeResult(w, id, reply)
}

// replyMessage packs hits into a completed agent message: a text summary for
// consumers that only render text, and a data part carrying the same
// {"hits": [...]} payload the MCP recall tool returns, so a client integrating
// both surfaces parses one shape.
func (s *Server) replyMessage(contextID, query string, hits []retrieve.Hit) (Message, error) {
	msgID, err := core.NewID()
	if err != nil {
		return Message{}, fmt.Errorf("a2a: message id: %w", err)
	}
	if contextID == "" {
		// The server mints the context id when the client did not start one.
		if contextID, err = core.NewID(); err != nil {
			return Message{}, fmt.Errorf("a2a: context id: %w", err)
		}
	}
	return Message{
		Kind:      "message",
		MessageID: msgID,
		ContextID: contextID,
		Role:      "agent",
		Parts: []Part{
			{Kind: "text", Text: renderHits(query, hits)},
			{Kind: "data", Data: map[string]any{"hits": hits}},
		},
	}, nil
}

// renderHits is the text-part rendering: one line per hit, the same
// name-plus-description surface the briefing and recall indexes lead with.
func renderHits(query string, hits []retrieve.Hit) string {
	if len(hits) == 0 {
		return fmt.Sprintf("no hits for %q", query)
	}
	var b strings.Builder
	fmt.Fprintf(&b, "%d hits for %q:\n", len(hits), query)
	for i, h := range hits {
		fmt.Fprintf(&b, "%d. [%s] %s -- %s", i+1, h.Kind, h.Name, h.Description)
		if h.Project != "" {
			fmt.Fprintf(&b, " (%s)", h.Project)
		}
		b.WriteString("\n")
	}
	return strings.TrimRight(b.String(), "\n")
}

// recordRecall mirrors the MCP recall handler's telemetry: a hit records
// retrieval.injected (source "recall" -- an A2A recall is the same query-gated
// demand the utility score counts), a zero-hit records recall.miss (the demand
// signal the gardener's memory-wanted pass clusters). The extra transport key
// attributes the caller without forking the source vocabulary.
func (s *Server) recordRecall(ctx context.Context, project, query, scope string, limit int, hits []retrieve.Hit) {
	if s.cfg.Events == nil {
		return
	}
	var kind core.EventKind
	var payload map[string]any
	if len(hits) > 0 {
		ids := make([]string, len(hits))
		scores := make([]float64, len(hits))
		for i, h := range hits {
			ids[i] = h.ID
			scores[i] = h.Score
		}
		kind = core.EventInjected
		payload = map[string]any{
			"query": query, "item_ids": ids, "item_scores": scores,
			"source": "recall", "transport": "a2a",
		}
	} else {
		kind = core.EventRecallMiss
		payload = map[string]any{
			"query": events.Truncate(query, recallMissQueryMax), "scope": scope, "limit": limit,
			"source": "recall", "transport": "a2a",
		}
	}
	if _, err := s.cfg.Events.Record(ctx, core.Event{
		Kind: kind, ProjectSlug: project, Payload: payload,
	}); err != nil {
		s.logger.Warn("a2a: record event", "kind", kind, "error", err)
	}
}

// rpcResponse is the JSON-RPC envelope. Result and Error are mutually
// exclusive by construction: writeResult and writeError each set exactly one.
type rpcResponse struct {
	JSONRPC string          `json:"jsonrpc"`
	ID      json.RawMessage `json:"id"`
	Result  any             `json:"result,omitempty"`
	Error   *rpcError       `json:"error,omitempty"`
}

type rpcError struct {
	Code    int    `json:"code"`
	Message string `json:"message"`
}

func (s *Server) writeResult(w http.ResponseWriter, id json.RawMessage, result any) {
	s.writeResponse(w, rpcResponse{JSONRPC: "2.0", ID: normalizeID(id), Result: result})
}

func (s *Server) writeError(w http.ResponseWriter, id json.RawMessage, code int, message string) {
	s.writeResponse(w, rpcResponse{JSONRPC: "2.0", ID: normalizeID(id), Error: &rpcError{Code: code, Message: message}})
}

// writeResponse always answers HTTP 200: JSON-RPC carries success and failure
// in the envelope, and the only non-200 responses this handler produces happen
// before a well-formed envelope exists (405, 401).
func (s *Server) writeResponse(w http.ResponseWriter, resp rpcResponse) {
	w.Header().Set("Content-Type", "application/json; charset=utf-8")
	if err := json.NewEncoder(w).Encode(resp); err != nil {
		s.logger.Warn("a2a: encode response", "error", err)
	}
}

// normalizeID keeps the echoed id valid JSON: an absent id (a malformed or
// parse-failed request) becomes an explicit null rather than empty bytes,
// which would corrupt the envelope.
func normalizeID(id json.RawMessage) json.RawMessage {
	if len(id) == 0 {
		return json.RawMessage("null")
	}
	return id
}

func metaString(meta map[string]any, key string) (string, bool, error) {
	v, present := meta[key]
	if !present {
		return "", false, nil
	}
	s, ok := v.(string)
	if !ok {
		return "", true, fmt.Errorf("metadata.%s must be a string, got %s", key, metadataType(v))
	}
	return strings.TrimSpace(s), true, nil
}

// metaInt reads a positive integer metadata key. JSON numbers decode as
// float64, so integrality must be checked before conversion; absent remains the
// service's zero-value default, while every malformed present value is loud.
func metaInt(meta map[string]any, key string, max int) (int, bool, error) {
	v, present := meta[key]
	if !present {
		return 0, false, nil
	}
	f, ok := v.(float64)
	if !ok {
		return 0, true, fmt.Errorf("metadata.%s must be an integer, got %s", key, metadataType(v))
	}
	if math.IsNaN(f) || math.IsInf(f, 0) || math.Trunc(f) != f {
		return 0, true, fmt.Errorf("metadata.%s must be an integer, got %v", key, f)
	}
	if f < 1 || f > float64(max) {
		return 0, true, fmt.Errorf("metadata.%s must be between 1 and %d", key, max)
	}
	return int(f), true, nil
}

func metadataType(v any) string {
	switch v.(type) {
	case nil:
		return "null"
	case bool:
		return "boolean"
	case float64:
		return "number"
	case string:
		return "string"
	case []any:
		return "array"
	case map[string]any:
		return "object"
	default:
		return fmt.Sprintf("%T", v)
	}
}

// verifyBearer mirrors internal/mcp's check: constant-time compare, and an
// empty configured key rejects everything rather than waving everything in. A
// peer_auth socket request was authenticated by the kernel and needs no key;
// its recall still runs under that account's reach (retrieve.Recall).
func verifyBearer(r *http.Request, key string) bool {
	if access.PeerAuthenticated(r.Context()) {
		return true
	}
	if key == "" {
		return false
	}
	parts := strings.SplitN(r.Header.Get("Authorization"), " ", 2)
	if len(parts) != 2 || !strings.EqualFold(parts[0], "bearer") {
		return false
	}
	return subtle.ConstantTimeCompare([]byte(parts[1]), []byte(key)) == 1
}
