package console

import (
	"context"
	"html/template"
	"regexp"
	"strings"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// Harness + model attribution: every session records the client that ran it
// (ExternalClient) and the model powering it (Model); memories inherit both via
// their source session. agentPill renders that pair as one compact pill --
// "cc · fable-5", "cx · gpt-5.5" -- so lists and headers show at a glance which
// harness and model produced an item. interactions.js mirrors agentPill for the
// live feed's client-built rows.

// Client discriminators as the hooks persist them (see internal/hooks/client.go
// Client.externalIdentity; console must not import the hooks API surface).
const (
	clientClaudeCode = "claude-code"
	clientCodex      = "codex"
)

// harnessOf returns the client discriminator for a session: the stored
// ExternalClient when present, else derived from the ambient name prefix.
// "" means unknown (an explicit session with no recorded client).
func harnessOf(sess core.Session) string {
	if sess.ExternalClient != "" {
		return sess.ExternalClient
	}
	return harnessOfSessionName(sess.Name)
}

// harnessOfSessionName derives the client from a session name's ambient prefix
// (cc/ -> claude-code, cx/ -> codex). It is the memory/provenance path: a
// memory's source_session stores the session name, not the session row.
func harnessOfSessionName(name string) string {
	switch {
	case strings.HasPrefix(name, "cc/"):
		return clientClaudeCode
	case strings.HasPrefix(name, "cx/"):
		return clientCodex
	}
	return ""
}

// sourceSessionResolver returns a memoized resolver for memory provenance
// values, which hold either a session name (ambient stamps: cc/ab12cd34) or a
// session ULID (bound stamps). The stamp's shape picks the single lookup: a
// ULID-shaped value resolves ONLY by id, because session names are
// agent-supplied and unvalidated, so a name lookup on a ULID would let a
// session named after another session's ULID hijack its provenance pill and
// source link (and cost a doomed extra query on every bound stamp). Lookup
// failures resolve to the zero Session -- provenance display is best-effort,
// like sessionNamer -- but a DB error is logged so it stays distinguishable
// from a deleted session.
func (s *Service) sourceSessionResolver(ctx context.Context) func(string) core.Session {
	cache := map[string]core.Session{}
	return func(src string) core.Session {
		if src == "" {
			return core.Session{}
		}
		if sess, ok := cache[src]; ok {
			return sess
		}
		var sess core.Session
		var ok bool
		var err error
		if store.LooksLikeSessionULID(src) {
			sess, ok, err = store.SessionByID(ctx, s.db(ctx), src)
		} else {
			sess, ok, err = store.SessionByName(ctx, s.db(ctx), src)
		}
		if err != nil {
			s.logger.Warn("console: memory source session", "source", src, "error", err)
		}
		if err != nil || !ok {
			sess = core.Session{}
		}
		cache[src] = sess
		return sess
	}
}

// harnessOfSource derives the client for a provenance value: the resolved
// session's identity when the row still exists, else the ambient name prefix.
func harnessOfSource(resolve func(string) core.Session, src string) string {
	if h := harnessOf(resolve(src)); h != "" {
		return h
	}
	return harnessOfSessionName(src)
}

// agentDisplay maps a client discriminator to its pill tone class, short label,
// and full name. An unrecognized non-empty client passes through verbatim in
// the neutral tone -- a future harness must render, not vanish.
func agentDisplay(harness string) (class, short, full string) {
	switch harness {
	case clientClaudeCode:
		return "cc", "cc", "Claude Code"
	case clientCodex:
		return "cx", "cx", "Codex"
	case "":
		return "", "", ""
	}
	return "", harness, harness
}

// modelBuildDate matches a trailing -YYYYMMDD build-date suffix on a model id.
var modelBuildDate = regexp.MustCompile(`-20\d{6}$`)

// modelShort compacts a provider model id for pill display: the build date and
// the claude- vendor prefix drop (claude-haiku-4-5-20251001 -> haiku-4-5),
// anything else stays verbatim. The pill's title keeps the full id.
func modelShort(model string) string {
	m := strings.TrimPrefix(modelBuildDate.ReplaceAllString(model, ""), "claude-")
	if m == "" {
		return model
	}
	return m
}

// agentPill renders the harness+model attribution pill, toned per harness (see
// console.css .agent-pill), with the full client name and verbatim model id in
// the title. Either half may be absent; both absent renders nothing.
func agentPill(harness, model string) template.HTML {
	class, short, full := agentDisplay(harness)
	var text, title []string
	if short != "" {
		text = append(text, short)
	}
	if m := modelShort(model); m != "" {
		text = append(text, m)
	}
	if len(text) == 0 {
		return ""
	}
	if full != "" {
		title = append(title, full)
	}
	if model != "" {
		title = append(title, model)
	}
	cls := "agent-pill"
	if class != "" {
		cls += " " + class
	}
	return template.HTML(`<span class="` + cls + `" title="` +
		template.HTMLEscapeString(strings.Join(title, " · ")) + `">` +
		template.HTMLEscapeString(strings.Join(text, " · ")) + `</span>`)
}
