package feed

import (
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"fmt"
	"math"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
)

// A frame is one event's metadata, in the ingest contract's shape: who
// (actor), what (class, activity), where (host, target), when (time), how it
// went (severity, outcome), and a bounded bag of metadata fields.
//
// The no-content rule is enforced by construction: a frame's fields come
// from frameFields, an allowlist of payload keys whose values are codes,
// kinds, counts, flags and ids, each checked against its type's pattern. A
// payload key not listed -- args, result, content, prompt, query, name,
// title, body, description, findings, error -- never reaches a frame, and a
// listed key whose value does not fit its pattern (free text where a code
// belongs) is dropped.

// Frame is one event as the feed sends it.
type Frame struct {
	Time     string         `json:"time"`
	Host     string         `json:"host"`
	Class    string         `json:"class"`
	Activity string         `json:"activity"`
	Severity string         `json:"severity"`
	Outcome  string         `json:"outcome"`
	Actor    *Actor         `json:"actor,omitempty"`
	Target   *Target        `json:"target,omitempty"`
	Fields   map[string]any `json:"fields,omitempty"`
}

// Actor is who acted. Account names the restricted local account
// (peer_auth.restricted) an event was written or attempted by -- the one that
// started its session, or that the request spoke for -- and is absent for the
// owner, so the owner's frames are what they were before it existed.
type Actor struct {
	Kind    string `json:"kind"`
	ID      string `json:"id,omitempty"`
	Session string `json:"session,omitempty"`
	Account string `json:"account,omitempty"`
}

// Target is what was acted on.
type Target struct {
	System    string `json:"system,omitempty"`
	Asset     string `json:"asset,omitempty"`
	PathClass string `json:"path_class,omitempty"`
}

// The contract's vocabularies.
var (
	frameClasses    = []string{"process", "network", "auth", "file", "config", "agent", "web", "dns", "finding", "health"}
	frameSeverities = []string{"unknown", "info", "low", "medium", "high", "critical"}
	frameOutcomes   = []string{"success", "failure", "blocked", "unknown"}
	frameActorKinds = []string{"user", "agent", "service", "system", "remote", "unknown"}
)

// targetSystem names Seamless as the system an event happened in.
const targetSystem = "seamless"

var (
	hostRe  = regexp.MustCompile(`^[a-z0-9](?:[a-z0-9.-]*[a-z0-9])?$`)
	wordRe  = regexp.MustCompile(`^[a-z][a-z0-9_]*$`)
	tokenRe = regexp.MustCompile(`^[a-z0-9][a-z0-9._:/-]{0,63}$`)
	ulidRe  = regexp.MustCompile(`^[0-9A-HJKMNP-TV-Z]{26}$`)
	// sessionNameRe is a session's display name (cc/<id>, cx/<id>, or an
	// explicit session's name).
	sessionNameRe = regexp.MustCompile(`^[A-Za-z0-9][A-Za-z0-9._:/-]{0,127}$`)
	// accountRe is a restricted account's name as a frame carries it: an OS
	// account name, which may start with "_" (a macOS service account's does).
	accountRe = regexp.MustCompile(`^[A-Za-z0-9_][A-Za-z0-9._-]{0,63}$`)
	// accountRefRe is a frame's actor.account: a name on accountRe, or the
	// hash accountRef stands in for a name off it.
	accountRefRe = regexp.MustCompile(`^(?:[A-Za-z0-9_][A-Za-z0-9._-]{0,63}|h:[0-9a-f]{24})$`)
)

// maxSessionName is the contract's longest session name, a restricted
// account's qualified one included.
const maxSessionName = 128

// sessionName is a session's name as a frame carries it: the name itself when
// it is on sessionNameRe, or a restricted account's qualified name,
// "<account>:<name>" (access.QualifiedSessionName), when the session is that
// account's, its account is on accountRe and the rest on sessionNameRe, within
// maxSessionName in all. Anything else is no name a frame carries.
func sessionName(name, account string) (string, bool) {
	if sessionNameRe.MatchString(name) {
		return name, true
	}
	rest, qualified := strings.CutPrefix(name, account+":")
	if account == "" || !qualified || len(name) > maxSessionName || !accountRe.MatchString(account) ||
		!sessionNameRe.MatchString(rest) {
		return "", false
	}
	return name, true
}

// accountRef is a restricted account as a frame names it: its name, or, for a
// name outside accountRe, "h:" and the first 24 hex digits of the name's
// SHA-256 -- never dropped, so a restricted account's event never reads as the
// owner's. The hash is not keyed: an account name is no secret from the
// monitor, which can compute it from its own configuration, and the commit
// snapshot (which has no key) names such an account the same way. "" is the
// owner.
func accountRef(account string) string {
	if account == "" || accountRe.MatchString(account) {
		return account
	}
	sum := sha256.Sum256([]byte(account))
	return "h:" + hex.EncodeToString(sum[:])[:24]
}

// valueType is how a listed payload key's value is checked.
type valueType int

const (
	tToken   valueType = iota // a short lowercase code: a kind, a tool, a reason
	tNumber                   // a finite number: a size, a count, a duration
	tBool                     // a flag
	tCodes                    // a list of codes, joined with ","
	tULID                     // one id
	tULIDs                    // a list of ids, joined with ","
	tVersion                  // an X.Y.Z version
)

// frameFields is the allowlist: the payload keys a frame may carry, the field
// name each becomes, and how its value is checked. Nothing else in a payload
// reaches a frame.
var frameFields = []struct {
	key   string
	field string
	typ   valueType
}{
	{"kind", "kind", tToken},
	{"item_kind", "item_kind", tToken},
	{"tool", "tool", tToken},
	{"op", "op", tToken},
	{"hook", "hook", tToken},
	{"source", "via", tToken},
	{"via", "via", tToken},
	{"reason", "reason", tToken},
	{"surface", "surface", tToken},
	{"change", "change", tToken},
	{"by", "by", tToken},
	{"action", "action", tToken},
	{"status", "status", tToken},
	{"from", "from", tToken},
	{"to", "to", tToken},
	{"where", "where", tToken},
	{"mode", "mode", tToken},
	{"step", "step", tToken},
	{"outcome", "outcome", tToken},
	{"proof", "proof", tToken},
	{"size", "size", tNumber},
	{"bytes", "bytes", tNumber},
	{"memories", "memories", tNumber},
	{"notes", "notes", tNumber},
	{"projects", "projects", tNumber},
	{"added", "added", tNumber},
	{"failures", "failures", tNumber},
	{"duration_ms", "duration_ms", tNumber},
	{"items", "items", tNumber},
	{"safe", "safe", tNumber},
	{"flagged", "flagged", tNumber},
	{"suppressed", "suppressed", tNumber},
	{"original_item_count", "item_count", tNumber},
	{"emitted_estimated_tokens", "emitted_tokens", tNumber},
	{"held", "held", tBool},
	{"quarantined", "quarantined", tBool},
	{"baseline", "baseline", tBool},
	{"on", "on", tBool},
	{"updated", "updated", tBool},
	{"truncated", "truncated", tBool},
	{"is_error", "is_error", tBool},
	{"ambient", "ambient", tBool},
	{"favorite", "favorite", tBool},
	{"matched", "matched", tBool},
	{"verdicts", "verdicts", tCodes},
	{"reasons", "reasons", tCodes},
	{"hold", "hold_id", tULID},
	{"released", "released_hold_id", tULID},
	{"superseded_by", "superseded_by", tULID},
	{"credential", "credential_id", tULID},
	{"item_ids", "item_ids", tULIDs},
	{"version", "version", tVersion},
	{"current", "current_version", tVersion},
}

var versionRe = regexp.MustCompile(`^v?\d+\.\d+\.\d+$`)

// eventShape is an event kind's class, severity and outcome.
type eventShape struct {
	class    string
	severity string
	outcome  string
}

// shapes overrides the default shape (agent, info, success) per kind.
var shapes = map[core.EventKind]eventShape{
	core.EventDeviceCode:              {"auth", "low", "success"},
	core.EventDevicePending:           {"auth", "medium", "success"},
	core.EventDevicePaired:            {"auth", "high", "success"},
	core.EventDeviceRevoked:           {"auth", "medium", "success"},
	core.EventAccessRefused:           {"auth", "medium", "blocked"},
	core.EventCorpusDrift:             {"file", "high", "blocked"},
	core.EventCorpusReviewed:          {"finding", "info", "success"},
	core.EventCorpusConfirmed:         {"agent", "low", "success"},
	core.EventLockdownChanged:         {"config", "high", "success"},
	core.EventMemoryHeld:              {"agent", "medium", "blocked"},
	core.EventMemoryReleased:          {"agent", "low", "success"},
	core.EventMemoryDiscarded:         {"agent", "low", "success"},
	core.EventSessionTainted:          {"agent", "low", "success"},
	core.EventSessionCleared:          {"agent", "low", "success"},
	core.EventFeaturesChanged:         {"config", "medium", "success"},
	core.EventProjectIsolationChanged: {"config", "medium", "success"},
	core.EventHookError:               {"agent", "low", "failure"},
	core.EventUpdateAvailable:         {"config", "info", "success"},
	core.EventUpdateApplied:           {"config", "low", "success"},
	core.EventUpdateStarted:           {"config", "low", "success"},
	core.EventUpdateFailed:            {"config", "medium", "failure"},
	core.EventBackupCreated:           {"file", "info", "success"},
	core.EventBackupFailed:            {"file", "medium", "failure"},
	core.EventBackupDeleted:           {"file", "low", "success"},
	core.EventBackupRestored:          {"file", "high", "success"},
	core.EventBackupRestoreFailed:     {"file", "medium", "failure"},
}

// session is what the feed knows of an event's session.
type session struct {
	name         string
	agentProcess string
	account      string // the restricted account that started it; "" the owner's
}

// mapper turns events into frames.
type mapper struct {
	host string
	key  []byte
	// hashed reports whether a project's slug must not leave in the clear
	// (a confidential or sealed project, or a locked one).
	hashed func(project string) bool
}

// projectRef is a project as a frame names it: its slug, a keyed hash of the
// slug for a project that must not leave in the clear, "global" for the
// global scope.
func (m *mapper) projectRef(slug string) string {
	if slug == "" {
		return "global"
	}
	if m.hashed != nil && m.hashed(slug) {
		mac := hmac.New(sha256.New, m.key)
		mac.Write([]byte(slug))
		return "h:" + hex.EncodeToString(mac.Sum(nil))[:24]
	}
	return slug
}

// frame maps one event. A value that fails its pattern is dropped, never cut
// or escaped into the frame.
func (m *mapper) frame(e core.Event, sess session) Frame {
	shape, ok := shapes[e.Kind]
	if !ok {
		shape = eventShape{"agent", "info", "success"}
	}
	f := Frame{
		Time:     e.TS.UTC().Format("2006-01-02T15:04:05.000Z"),
		Host:     m.host,
		Class:    shape.class,
		Activity: activityOf(e.Kind),
		Severity: shape.severity,
		Outcome:  shape.outcome,
		Target:   &Target{System: targetSystem, Asset: m.projectRef(e.ProjectSlug)},
		Fields:   map[string]any{},
	}
	if ulidRe.MatchString(e.ID) {
		f.Fields["event_id"] = e.ID
	}
	if ulidRe.MatchString(e.ItemID) {
		f.Fields["item_id"] = e.ItemID
	}
	p := e.Payload
	for _, ff := range frameFields {
		if v, ok := fieldValue(p[ff.key], ff.typ); ok {
			f.Fields[ff.field] = v
		}
	}
	if ik, ok := f.Fields["item_kind"].(string); ok {
		f.Target.PathClass = ik
	}
	// Kind-specific metadata that is not a flat payload key.
	switch e.Kind {
	case core.EventAccessRefused:
		if t, ok := p["target"].(string); ok && (t == "" || tokenRe.MatchString(t)) {
			f.Fields["target_project"] = m.projectRef(t)
		}
	case core.EventFeaturesChanged:
		if fs, ok := p["features"].(map[string]any); ok {
			for k, v := range fs {
				if b, ok := v.(bool); ok && wordRe.MatchString(k) && len(k) <= 40 {
					f.Fields["feature_"+k] = b
				}
			}
		}
	case core.EventCorpusDrift:
		if p["change"] == "missing" {
			f.Severity, f.Outcome = "medium", "unknown"
		}
	case core.EventLockdownChanged:
		if on, _ := p["on"].(bool); !on {
			f.Severity = "medium"
		}
	case core.EventMemoryScreened:
		if codes, _ := f.Fields["verdicts"].(string); codes != "" {
			f.Severity = "low"
		}
		if held, _ := f.Fields["held"].(bool); held {
			f.Outcome = "blocked"
		}
	case core.EventToolCall:
		if isErr, _ := f.Fields["is_error"].(bool); isErr {
			f.Outcome = "failure"
		}
	case core.EventUpdateFailed:
		// update.release_signers refused a release the owner did not sign
		// (missing, unlisted, invalid): a release that reached this install
		// without the owner's key is the case the pin exists for, so it reads
		// as blocked, high. Only this kind's "signature" is carried; other
		// kinds use the key for other things.
		if w, ok := fieldValue(p["signature"], tToken); ok {
			f.Fields["signature"] = w
			f.Severity, f.Outcome = "high", "blocked"
		}
	}
	// Who acted: an agent's session, a restricted account's request with no
	// session, the owner at the console, or the daemon. A restricted account is
	// the session's (a session belongs to the account that started it), else
	// the one the event's request spoke for.
	account := sess.account
	if account == "" {
		// The recorder names the account a request spoke for
		// (events.PayloadAccount); it reaches the actor, never the fields.
		account, _ = p[events.PayloadAccount].(string)
	}
	switch {
	case e.SessionID != "":
		a := &Actor{Kind: "agent", Account: accountRef(account)}
		if name, ok := sessionName(sess.name, account); ok {
			a.Session = name
		}
		if tokenRe.MatchString(sess.agentProcess) || sessionNameRe.MatchString(sess.agentProcess) {
			a.ID = sess.agentProcess
		}
		f.Actor = a
	case account != "":
		// Never the owner, whatever the payload says of who acted.
		f.Actor = &Actor{Kind: "agent", Account: accountRef(account)}
	case p["by"] == "owner" || p["by"] == "console":
		f.Actor = &Actor{Kind: "user", ID: "owner"}
	default:
		f.Actor = &Actor{Kind: "system", ID: targetSystem}
	}
	if len(f.Fields) == 0 {
		f.Fields = nil
	}
	return f
}

// activityOf is an event kind as a snake_case activity word.
func activityOf(k core.EventKind) string {
	a := strings.NewReplacer(".", "_", "-", "_").Replace(strings.ToLower(string(k)))
	if !wordRe.MatchString(a) || len(a) > 48 {
		return "event"
	}
	return a
}

// fieldValue checks one payload value against its type, returning the value a
// frame carries.
func fieldValue(v any, t valueType) (any, bool) {
	switch t {
	case tToken:
		s, ok := v.(string)
		return s, ok && tokenRe.MatchString(s)
	case tVersion:
		s, ok := v.(string)
		return s, ok && versionRe.MatchString(s)
	case tNumber:
		switch n := v.(type) {
		case float64:
			return n, !math.IsNaN(n) && !math.IsInf(n, 0)
		case int:
			return n, true
		case int64:
			return n, true
		case json.Number:
			f, err := n.Float64()
			return f, err == nil
		}
		return nil, false
	case tBool:
		b, ok := v.(bool)
		return b, ok
	case tULID:
		s, ok := v.(string)
		return s, ok && ulidRe.MatchString(s)
	case tCodes, tULIDs:
		list := stringList(v)
		if list == nil {
			return nil, false
		}
		re := tokenRe
		if t == tULIDs {
			re = ulidRe
		}
		var kept []string
		size := 0
		for _, s := range list {
			if !re.MatchString(s) {
				continue
			}
			if size+len(s)+1 > maxFieldValue {
				break // the first ids that fit; item_count says how many there were
			}
			kept = append(kept, s)
			size += len(s) + 1
		}
		return strings.Join(kept, ","), true
	}
	return nil, false
}

// maxFieldValue is the contract's longest metadata value.
const maxFieldValue = 1024

func stringList(v any) []string {
	switch l := v.(type) {
	case []string:
		return l
	case []any:
		out := make([]string, 0, len(l))
		for _, x := range l {
			if s, ok := x.(string); ok {
				out = append(out, s)
			}
		}
		return out
	}
	return nil
}

// heartbeat is the feed's heartbeat frame.
func (m *mapper) heartbeat(now time.Time, every time.Duration, st SpoolStats) Frame {
	return Frame{
		Time: now.UTC().Format("2006-01-02T15:04:05.000Z"), Host: m.host,
		Class: "health", Activity: "heartbeat", Severity: "info", Outcome: "success",
		Fields: map[string]any{"interval_s": int(every / time.Second), "spooled": st.Events, "dropped_count": st.Dropped},
	}
}

// ---------------------------------------------------------------------------
// The contract's frame rules, checked before a frame is spooled
// ---------------------------------------------------------------------------

// contentWords are the names a frame field may never carry: the contract
// refuses a content-shaped name wherever it stands. A content word followed
// only by measures or identifiers (body_size, item_ids) is metadata.
var contentWords = []string{
	"body", "bodies", "content", "contents", "text", "message", "messages", "prompt", "prompts",
	"response", "responses", "reply", "replies", "output", "outputs", "completion", "completions",
	"transcript", "transcripts", "recording", "recordings", "eeg", "payload", "payloads", "raw",
	"stdin", "stdout", "stderr", "args", "arguments", "argv", "params", "parameters", "input", "inputs",
	"result", "results", "html", "attachment", "attachments", "snippet", "snippets", "excerpt", "excerpts",
	"comment", "comments", "screenshot", "screenshots",
}

var metadataQualifiers = []string{
	"size", "sizes", "bytes", "length", "len", "count", "counts", "hash", "hashes", "sha", "md", "digest", "crc",
	"tokens", "lines", "chars", "words", "id", "ids", "kind", "type", "class", "format", "encoding", "mime",
	"path", "paths", "at", "time", "verdict", "present",
}

// contentShaped reports whether a field name names content rather than
// metadata about it.
func contentShaped(name string) bool {
	parts := strings.Split(name, "_")
	for i, p := range parts {
		if !slices.Contains(contentWords, p) {
			continue
		}
		after := parts[i+1:]
		meta := len(after) > 0
		for _, a := range after {
			if !slices.Contains(metadataQualifiers, a) && strings.Trim(a, "0123456789") != "" {
				meta = false
			}
		}
		if !meta {
			return true
		}
	}
	return false
}

// validate checks a frame against the contract's rules: every vocabulary,
// pattern and bound, no content-shaped name, and the whole frame within 8192
// bytes. The feed drops a frame that fails (it would be refused anyway) and
// counts it.
func (f Frame) validate() error {
	if _, err := time.Parse("2006-01-02T15:04:05.000Z", f.Time); err != nil {
		return fmt.Errorf("time: %w", err)
	}
	if len(f.Host) > 128 || !hostRe.MatchString(f.Host) {
		return fmt.Errorf("host %q", f.Host)
	}
	if !slices.Contains(frameClasses, f.Class) || !slices.Contains(frameSeverities, f.Severity) || !slices.Contains(frameOutcomes, f.Outcome) {
		return fmt.Errorf("class/severity/outcome %s/%s/%s", f.Class, f.Severity, f.Outcome)
	}
	if len(f.Activity) > 48 || !wordRe.MatchString(f.Activity) {
		return fmt.Errorf("activity %q", f.Activity)
	}
	if f.Actor != nil {
		if !slices.Contains(frameActorKinds, f.Actor.Kind) || len(f.Actor.ID) > 128 || len(f.Actor.Session) > maxSessionName ||
			(f.Actor.Account != "" && !accountRefRe.MatchString(f.Actor.Account)) {
			return fmt.Errorf("actor")
		}
	}
	if f.Target != nil {
		if (f.Target.System != "" && !tokenRe.MatchString(f.Target.System)) || len(f.Target.Asset) > 512 ||
			(f.Target.PathClass != "" && !tokenRe.MatchString(f.Target.PathClass)) {
			return fmt.Errorf("target")
		}
	}
	if len(f.Fields) > 32 {
		return fmt.Errorf("fields: %d names", len(f.Fields))
	}
	for k, v := range f.Fields {
		if len(k) > 48 || !wordRe.MatchString(k) || contentShaped(k) {
			return fmt.Errorf("field name %q", k)
		}
		switch x := v.(type) {
		case string:
			if len(x) > maxFieldValue {
				return fmt.Errorf("field %s: too long", k)
			}
		case float64:
			if math.IsNaN(x) || math.IsInf(x, 0) {
				return fmt.Errorf("field %s: not finite", k)
			}
		case int, int64, bool, nil:
		default:
			return fmt.Errorf("field %s: %T", k, v)
		}
	}
	raw, err := json.Marshal(f)
	if err != nil {
		return err
	}
	if len(raw) > 8192 {
		return fmt.Errorf("frame: %d bytes", len(raw))
	}
	return nil
}
