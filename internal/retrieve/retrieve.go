// Package retrieve assembles what an agent sees from its memory: the
// session-start briefing, the user-prompt-submit recall block, and the recall
// tool's fused search. It reads the store index and (when an embedder is set)
// the vector store, budgets output by estimated tokens, and sanitizes every
// interpolated field against prompt injection before it reaches an agent.
package retrieve

import (
	"context"
	"database/sql"
	"log/slog"
	"regexp"
	"strconv"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/llm"
)

// Service assembles briefings, prompt recall, and fused recall over one store.
type Service struct {
	db         *sql.DB
	embedder   llm.Embedder     // nil => lexical-only (FTS); recall degrades gracefully
	bodyReader MemoryBodyReader // nil => briefing omits the pinned-stage section
	budgets    config.Budgets
	briefing   config.Briefing // file/env base; console overrides layer on at briefing time
	features   config.Features // file/env base; console overrides layer on the same way
	search     config.Search
	logger     *slog.Logger

	corpus *corpusCache // prompt-matcher IDF corpus, cached per project scope

	// updateNotice supplies the main-session briefing's update-notice line
	// (SetUpdateNotice); nil, the default, renders none.
	updateNotice UpdateNoticeFunc

	// lockdown reports whether write screening's lockdown is on (SetLockdown);
	// nil, the default, is never.
	lockdown func(context.Context) bool
}

// UpdateNoticeFunc returns the one-line Seamless update notice for a briefing
// assembled for a session on host -- "" means the daemon's own machine, otherwise
// the host name BriefingInput.Host carries -- or "" when there is nothing to say.
// It must be cheap: it runs on every briefing assembly.
//
// A local caller names the daemon's machine (config.Hostname, lowercased)
// rather than sending "": the SessionStart hook and MCP session_start both fall
// back to it, and only the console preview passes "". So a provider reads both
// "" and the daemon's own host name as local. Whatever it returns is untrusted
// text to the briefing: it renders only after sanitizeField has flattened it to
// one line, stripped injection phrases, and capped it at updateNoticeMaxRunes.
type UpdateNoticeFunc func(ctx context.Context, host string) string

// New builds a retrieval Service. embedder may be nil, in which case recall uses
// FTS only and the semantic paths are skipped. Briefing knobs start at their
// defaults; SetBriefingConfig overrides them with the loaded file/env values.
func New(db *sql.DB, embedder llm.Embedder, budgets config.Budgets, logger *slog.Logger) *Service {
	if logger == nil {
		logger = slog.Default()
	}
	return &Service{
		db:       db,
		embedder: embedder,
		budgets:  budgets,
		briefing: config.Defaults().Briefing,
		features: config.Defaults().Features,
		search:   config.Defaults().Search,
		logger:   logger,
		corpus:   newCorpusCache(),
	}
}

// SetBriefingConfig sets the file/env briefing knobs the Service starts from.
// The console's runtime override row (store.SettingBriefingConfig) still layers
// on top of these at briefing-assembly time, so a console save takes effect on
// the next session start without a restart.
func (s *Service) SetBriefingConfig(b config.Briefing) { s.briefing = b }

// SetSearchConfig sets the file/env search knobs (Defaults() until called).
func (s *Service) SetSearchConfig(c config.Search) { s.search = c }

// SetFeaturesConfig sets the file/env optional-features base the Service starts
// from. The console's runtime override row still layers on top at assembly time
// (see effectiveFeatures), so a Settings toggle applies to the next briefing
// without a restart.
func (s *Service) SetFeaturesConfig(c config.Features) { s.features = c }

// SetUpdateNotice installs the provider (nil = no notice, the default). Like the
// other setters it is wiring-time configuration: call it before the Service
// assembles briefings.
func (s *Service) SetUpdateNotice(fn UpdateNoticeFunc) { s.updateNotice = fn }

// SetLockdown installs write screening's lockdown check
// (screening.Service.LockdownOn). Wiring-time, like the other setters.
func (s *Service) SetLockdown(fn func(context.Context) bool) { s.lockdown = fn }

// injectionRe strips imperative prompt-injection phrases from any free-prose
// field lifted out of stored content and shown to an agent as trusted context.
// The left boundary rejects hyphen-attached matches so a slug mentioned in
// prose ("fixture-install-hooks-needs-home-override") survives the scrub; the
// consumed prefix character is restored by the $1 in the replacement.
var injectionRe = regexp.MustCompile(`(?i)(^|[^\w-])(?:ignore|disregard|from now on|you must|override)\b[^\n]*`)

// sanitizeField scrubs a single free-prose field for safe interpolation into a
// briefing: newlines flattened, injection phrases removed, whitespace
// collapsed, and the result capped at maxRunes (with an ellipsis). maxRunes <=
// 3 disables the cap. Identifier fields go through sanitizeName instead.
func sanitizeField(s string, maxRunes int) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = injectionRe.ReplaceAllString(s, "$1")
	s = strings.Join(strings.Fields(s), " ")
	// maxRunes <= 3 disables the cap (the historical contract); above it, cut on
	// a word boundary with a trailing ellipsis rather than mid-word.
	if maxRunes > 3 {
		s = core.TruncateWords(s, maxRunes)
	}
	return s
}

// sanitizeName scrubs an identifier field -- a memory/note/plan name, project
// slug, or session name -- for briefing interpolation: newlines flattened,
// whitespace collapsed, capped at maxRunes. It deliberately skips the
// injection scrub: identifiers are validated slugs that cannot carry an
// imperative phrase, and they are lookup keys the reader passes back verbatim
// (memory_read name=<name>), so scrubbing one turns into a not-found error on
// the agent's next call -- memories named "...-override" were briefed with the
// tail silently amputated, and agents faithfully read the mangled name back.
func sanitizeName(s string, maxRunes int) string {
	s = strings.ReplaceAll(s, "\r", " ")
	s = strings.ReplaceAll(s, "\n", " ")
	s = strings.Join(strings.Fields(s), " ")
	if maxRunes > 3 {
		s = core.TruncateWords(s, maxRunes)
	}
	return s
}

// EstimateTokens is the repository-wide cheap token estimate (~4 bytes/token)
// used to budget model-visible context without a tokenizer dependency.
func EstimateTokens(s string) int { return (len(s) + 3) / 4 }

func estTokens(s string) int { return EstimateTokens(s) }

// humanAge renders how long ago t was, compactly (e.g. "3d", "5h", "just now").
func humanAge(t time.Time) string {
	if t.IsZero() {
		return "unknown"
	}
	d := time.Since(t)
	switch {
	case d < time.Minute:
		return "just now"
	case d < time.Hour:
		return strconv.Itoa(int(d.Minutes())) + "m"
	case d < 24*time.Hour:
		return strconv.Itoa(int(d.Hours())) + "h"
	default:
		return strconv.Itoa(int(d.Hours()/24)) + "d"
	}
}
