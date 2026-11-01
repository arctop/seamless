// Package feed is write screening's metadata feed: a subscriber on the event
// log that turns every event into a metadata frame -- kind, time, session,
// agent process, project (an isolated or sealed one as a keyed hash), item
// kind, sizes and verdict codes -- and delivers the frames to a configured
// Unix socket, spooling them on disk while the consumer is down.
//
// It never writes an event's arguments, results, bodies, prompts, queries or
// names: a frame is built from an allowlist of metadata keys (frame.go), and
// nothing else in an event reaches it.
//
// The wire format is a small generic ingest contract: HTTP/1.1 on the socket,
// POST /v1/batch with an NDJSON body whose first line is the header
// {"v":1,"batch":"<id>","frames":<n>,"dropped":<n>}, then one frame per line;
// GET /v1/limits for the consumer's limits; and a JSON receipt whose action is
// settle, retry or discard. A batch is resent unchanged, byte for byte, under
// its id until a receipt settles or discards it.
package feed

import (
	"encoding/json"
	"errors"
	"fmt"
	"regexp"
	"strconv"
)

// WireVersion is the header's and every receipt's "v".
const WireVersion = 1

// The two requests a consumer answers.
const (
	BatchPath  = "/v1/batch"
	LimitsPath = "/v1/limits"
	NDJSON     = "application/x-ndjson"
)

// batchIDRe is a batch id: 1 to 64 of A-Z a-z 0-9 . _ : -, starting with a
// letter or a digit.
var batchIDRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:-]{0,63}$`)

// Limits are the consumer's limits (GET /v1/limits), with the contract's
// defaults until a consumer has said otherwise.
type Limits struct {
	FrameBytes  int `json:"frame_bytes"`
	BatchFrames int `json:"batch_frames"`
	BatchBytes  int `json:"batch_bytes"`
	HeartbeatS  int `json:"heartbeat_s"`
}

// DefaultLimits are the contract's defaults.
var DefaultLimits = Limits{FrameBytes: 16384, BatchFrames: 500, BatchBytes: 1 << 20, HeartbeatS: 300}

// parseLimits reads a GET /v1/limits answer. A value missing or out of range
// keeps its default: limits only ever narrow what is sent.
func parseLimits(body []byte) (Limits, error) {
	var raw struct {
		V           int `json:"v"`
		FrameBytes  int `json:"frame_bytes"`
		BatchFrames int `json:"batch_frames"`
		BatchBytes  int `json:"batch_bytes"`
		HeartbeatS  int `json:"heartbeat_s"`
	}
	if err := json.Unmarshal(body, &raw); err != nil {
		return DefaultLimits, fmt.Errorf("feed: limits: %w", err)
	}
	if raw.V != WireVersion {
		return DefaultLimits, fmt.Errorf("feed: limits: version %d, want %d", raw.V, WireVersion)
	}
	l := DefaultLimits
	if raw.FrameBytes > 0 {
		l.FrameBytes = raw.FrameBytes
	}
	if raw.BatchFrames > 0 {
		l.BatchFrames = raw.BatchFrames
	}
	if raw.BatchBytes > 0 {
		l.BatchBytes = raw.BatchBytes
	}
	if raw.HeartbeatS > 0 {
		l.HeartbeatS = raw.HeartbeatS
	}
	return l, nil
}

// encodeBatch is one batch's body: the header line, then each frame line
// (already JSON), every line ending in "\n".
func encodeBatch(batch string, dropped int, lines []string) ([]byte, error) {
	if !batchIDRe.MatchString(batch) {
		return nil, fmt.Errorf("feed: batch id %q is not 1 to 64 of A-Z a-z 0-9 . _ : - starting with a letter or digit", batch)
	}
	if dropped < 0 {
		return nil, errors.New("feed: dropped is a whole number")
	}
	header := `{"v":1,"batch":` + strconv.Quote(batch) + `,"frames":` + strconv.Itoa(len(lines)) +
		`,"dropped":` + strconv.Itoa(dropped) + `}`
	size := len(header) + 1
	for _, l := range lines {
		size += len(l) + 1
	}
	out := make([]byte, 0, size)
	out = append(out, header...)
	out = append(out, '\n')
	for _, l := range lines {
		out = append(out, l...)
		out = append(out, '\n')
	}
	return out, nil
}

// Receipt actions.
const (
	ActionSettle  = "settle"
	ActionRetry   = "retry"
	ActionDiscard = "discard"
)

// batchRules are the whole-batch refusals a consumer may send, each with its
// HTTP status and action.
var batchRules = map[string]struct {
	status int
	action string
}{
	"sender":  {403, ActionRetry},
	"path":    {404, ActionRetry},
	"method":  {405, ActionRetry},
	"type":    {415, ActionRetry},
	"size":    {413, ActionDiscard},
	"newline": {400, ActionDiscard},
	"header":  {400, ActionDiscard},
	"version": {400, ActionRetry},
	"frames":  {400, ActionDiscard},
	"store":   {503, ActionRetry},
}

// Receipt is the producer's reading of a reply to one batch.
type Receipt struct {
	Action        string
	Status        string // stored or duplicate, for a settle
	Accepted      int
	RefusedCount  int
	Dropped       int // the drop count the consumer recorded, for a settle
	Rule          string
	RetryAfterSec int
}

// readReceipt is what the producer does with a reply to batch: the receipt's
// own action decides, and a reply that is no receipt (no JSON, another
// version, another batch's id, numbers missing, or a status that does not fit
// the action) is a retry -- only a receipt settles or discards a batch.
func readReceipt(status int, body []byte, batch string) Receipt {
	retry := func(rule string) Receipt { return Receipt{Action: ActionRetry, Rule: rule} }
	var r struct {
		V            int             `json:"v"`
		Action       string          `json:"action"`
		Status       string          `json:"status"`
		Batch        *string         `json:"batch"`
		Frames       *int            `json:"frames"`
		Accepted     *int            `json:"accepted"`
		RefusedCount *int            `json:"refused_count"`
		Refused      json.RawMessage `json:"refused"`
		Dropped      *int            `json:"dropped"`
		Rule         string          `json:"rule"`
		RetryAfterS  *int            `json:"retry_after_s"`
	}
	if err := json.Unmarshal(body, &r); err != nil || r.V != WireVersion {
		return retry("http_" + strconv.Itoa(status))
	}
	whole := func(p *int) bool { return p != nil && *p >= 0 }
	switch r.Action {
	case ActionSettle:
		isList := len(r.Refused) > 0 && r.Refused[0] == '['
		ok := status == 200 && r.Batch != nil && *r.Batch == batch && (r.Status == "stored" || r.Status == "duplicate") &&
			whole(r.Frames) && whole(r.Accepted) && whole(r.RefusedCount) && whole(r.Dropped) && isList
		if !ok {
			return retry("receipt")
		}
		return Receipt{Action: ActionSettle, Status: r.Status, Accepted: *r.Accepted, RefusedCount: *r.RefusedCount, Dropped: *r.Dropped}
	case ActionRetry, ActionDiscard:
		known, ok := batchRules[r.Rule]
		if !ok || known.action != r.Action || known.status != status || (r.Batch != nil && *r.Batch != batch) {
			return retry("receipt")
		}
		out := Receipt{Action: r.Action, Rule: r.Rule}
		if r.Action == ActionRetry && whole(r.RetryAfterS) {
			out.RetryAfterSec = *r.RetryAfterS
		}
		return out
	}
	return retry("http_" + strconv.Itoa(status))
}
