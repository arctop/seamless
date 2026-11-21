package harvest

// The pushed capture: what `seam hook` adds to a hook body when it reads the
// agent's files itself, because the daemon may not (peer_auth runs it as its
// own OS account, and it opens no path a hook names). It rides in the body,
// beside the agent client's own fields, under Field.
//
// The client pushes only to the peer_auth Unix socket (cmd/seam
// hook_capture.go), and the daemon reads Field only on a request that carries
// a peer_auth View (internal/hooks pushed.go): with peer_auth off neither side
// touches it, and every hook is exactly what it was. On the daemon a push is
// the caller's own claim -- the same claim as session_update's findings -- and
// lands only through the fences every other write meets: the session the hook
// names must be the caller's to write, and a locked project's session takes
// none of it until the owner approves a lease for the pushing process's tree.
//
// The format is additive: a field may be added, never renamed, retyped or
// repurposed. A reader ignores fields it does not know; a field it knows that
// does not validate refuses the whole push (DecodePush), never a part of it.

import (
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/validate"
)

// Field is the hook-body key a pushed capture rides under.
const Field = "seamless_capture"

// PushVersion is the push format this release writes, and the only one it
// reads.
const PushVersion = 1

// RefusedHeader is the response header a daemon sets on a hook whose pushed
// capture it refused at a lock: one line, the rule and the remedy. `seam hook`
// prints it on stderr. A hook whose event can carry context (PostToolUse)
// gets the same line as its context too.
const RefusedHeader = "X-Seamless-Capture-Refused"

// maxModelBytes bounds a pushed model id: provider ids are a few dozen bytes.
const maxModelBytes = 200

// MaxFindingsRunes bounds pushed findings: what Findings and CodexStopFindings
// produce at most (the prefix, which is ASCII, and MaxRunes of message).
const MaxFindingsRunes = len(Prefix) + MaxRunes

// ErrInvalidPush marks a pushed capture that does not decode or validate.
var ErrInvalidPush = errors.New("invalid pushed capture")

// Push is one hook's pushed capture. Every field but Version is optional: an
// absent one is "nothing found", and the daemon treats it exactly as it
// treats its own read finding nothing.
type Push struct {
	// Version is PushVersion.
	Version int `json:"version"`
	// Model is the last main-thread model of a Claude Code transcript
	// (TranscriptModel): session-start, user-prompt-submit and session-end.
	Model string `json:"model,omitempty"`
	// Findings is the harvested summary: a Claude Code transcript's last
	// assistant message at session-end (Findings), a Codex turn's at stop
	// (CodexStopFindings).
	Findings string `json:"findings,omitempty"`
	// Tokens is the session's absolute token totals (ClaudeTokens at
	// session-end, its subagents' transcripts included; CodexTokens at stop),
	// normalized.
	Tokens *core.TokenUsage `json:"tokens,omitempty"`
	// Usage is the session's usage ledger rows (ClaudeUsage at session-end),
	// at most MaxUsageRows, which sum to Tokens. Absent, the daemon credits
	// Tokens' growth since the ledger last counted it (a Codex stop, an older
	// client, a transcript past the cap).
	Usage []core.UsageBucket `json:"usage,omitempty"`
	// GitHead is the commit checked out at the hook's cwd (a full hex id), the
	// stamp on a captured plan or subagent note.
	GitHead string `json:"git_head,omitempty"`
	// Plan is the plan file a Write/Edit/MultiEdit or ExitPlanMode named.
	Plan *PlanFile `json:"plan,omitempty"`
	// Prompt is a subagent's spawn prompt (subagent-start, subagent-stop);
	// Report its final report (subagent-stop).
	Prompt string `json:"prompt,omitempty"`
	Report string `json:"report,omitempty"`
}

// PlanFile is a plan file as the client read it: its basename (the
// correlation key, PlanBasename) and its content ("" when unreadable).
type PlanFile struct {
	Basename string `json:"basename"`
	Content  string `json:"content,omitempty"`
}

// DecodePush reads the pushed capture out of a hook body. ok is false when the
// body carries none: no Field, a JSON null, or a body that is not a JSON
// object at all (the hook's own decode is as tolerant). A capture that is
// present but does not decode or validate is an error wrapping
// ErrInvalidPush, never a partial push.
func DecodePush(body []byte) (Push, bool, error) {
	var env map[string]json.RawMessage
	if err := json.Unmarshal(body, &env); err != nil {
		return Push{}, false, nil
	}
	raw, present := env[Field]
	if !present || string(raw) == "null" {
		return Push{}, false, nil
	}
	var p Push
	if err := json.Unmarshal(raw, &p); err != nil {
		return Push{}, false, fmt.Errorf("harvest.DecodePush: %w: %w", ErrInvalidPush, err)
	}
	if err := p.Validate(); err != nil {
		return Push{}, false, err
	}
	return p, true, nil
}

// Validate checks every field a reader of this version knows.
func (p Push) Validate() error {
	bad := func(format string, args ...any) error {
		return fmt.Errorf("harvest.Push.Validate: %w: "+format, append([]any{ErrInvalidPush}, args...)...)
	}
	if p.Version != PushVersion {
		return bad("version %d: valid values are %d", p.Version, PushVersion)
	}
	if p.Model != "" && !ValidModel(p.Model) {
		return bad("model is not a model id")
	}
	if !utf8.ValidString(p.Findings) || utf8.RuneCountInString(p.Findings) > MaxFindingsRunes {
		return bad("findings exceed %d characters or are not UTF-8", MaxFindingsRunes)
	}
	if t := p.Tokens; t != nil {
		if t.Input < 0 || t.Cached < 0 || t.CacheCreation < 0 || t.Output < 0 {
			return bad("negative token count")
		}
		n := *t
		n.Normalize()
		if n.Total != t.Total {
			return bad("token total %d is not the sum of its parts (%d)", t.Total, n.Total)
		}
	}
	if err := validUsage(p.Usage, p.Tokens); err != "" {
		return bad("%s", err)
	}
	if p.GitHead != "" && !ValidGitHead(p.GitHead) {
		return bad("git_head is not a full commit id")
	}
	if p.Plan != nil && validate.Name(p.Plan.Basename) != nil {
		return bad("plan basename is not a safe name")
	}
	return nil
}

// Carries reports whether the push holds anything a capture would record --
// findings, token totals or usage, a plan, a subagent's report -- as opposed
// to the metadata (the model) a locked session still takes.
func (p Push) Carries() bool {
	return p.Findings != "" || p.Tokens != nil || len(p.Usage) > 0 || p.Plan != nil || p.Report != ""
}

// The span a pushed usage row's bucket may fall in: no transcript predates
// the one, and the other is the last year a timestamp is written in.
var (
	usageEarliest = time.Date(2000, 1, 1, 0, 0, 0, 0, time.UTC)
	usageLatest   = time.Date(9999, 12, 31, 23, 45, 0, 0, time.UTC)
)

// validUsage checks pushed usage rows against the totals they ride with: at
// most MaxUsageRows of them, each a quarter-hour's start within the span, a
// known thread, a model id or none, no negative count, a minute mask within
// the bucket's minutes, no two for one bucket, model and thread, and summing
// to tokens, which must be there. It returns the reason, "" for rows that
// hold (none hold trivially).
func validUsage(rows []core.UsageBucket, tokens *core.TokenUsage) string {
	if len(rows) == 0 {
		return ""
	}
	if len(rows) > MaxUsageRows {
		return fmt.Sprintf("%d usage rows exceed the %d a push carries", len(rows), MaxUsageRows)
	}
	if tokens == nil {
		return "usage rows without the token totals they sum to"
	}
	type key struct {
		bucket int64
		model  string
		thread core.UsageThread
	}
	seen := make(map[key]bool, len(rows))
	for _, r := range rows {
		switch {
		case !r.Aligned() || r.Bucket.Before(usageEarliest) || r.Bucket.After(usageLatest):
			return "a usage row's bucket is not a quarter-hour's start"
		case !r.Thread.Valid():
			return fmt.Sprintf("usage thread %q: valid values are %s, %s", r.Thread, core.UsageThreadMain, core.UsageThreadSubagent)
		case r.Model != "" && !ValidModel(r.Model):
			return "a usage row's model is not a model id"
		case r.Input < 0 || r.Cached < 0 || r.CacheCreation < 0 || r.Output < 0 || r.Messages < 0:
			return "negative usage count"
		case r.MinuteMask < 0 || r.MinuteMask > core.UsageMinuteMaskAll:
			return "a usage row's minute mask is past its bucket's minutes"
		}
		k := key{r.Bucket.Unix(), r.Model, r.Thread}
		if seen[k] {
			return "two usage rows for one bucket, model and thread"
		}
		seen[k] = true
	}
	if sum := core.SumUsage(rows); sum != *tokens {
		return fmt.Sprintf("usage rows sum to %d tokens, not the %d of the totals", sum.Total, tokens.Total)
	}
	return ""
}

// ValidModel reports whether s can be a model id as a provider names one: a
// short single line of printable text. The placeholder Claude Code stamps on
// synthetic entries is not one.
func ValidModel(s string) bool {
	if s == "" || len(s) > maxModelBytes || s != strings.TrimSpace(s) || s == syntheticModel || !utf8.ValidString(s) {
		return false
	}
	for _, r := range s {
		if unicode.IsControl(r) {
			return false
		}
	}
	return true
}

// ValidGitHead reports whether s is a full commit id: 40 (SHA-1) or 64
// (SHA-256) lower-case hex digits.
func ValidGitHead(s string) bool {
	if len(s) != 40 && len(s) != 64 {
		return false
	}
	for _, r := range s {
		if (r < '0' || r > '9') && (r < 'a' || r > 'f') {
			return false
		}
	}
	return true
}
