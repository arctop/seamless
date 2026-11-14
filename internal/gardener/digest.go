package gardener

import (
	"context"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

const digestSystemPrompt = "You are a concise technical writer. Summarize an AI agent's recent work sessions into a short monthly digest: the themes worked on, what was decided or learned, and anything left open. Use compact markdown bullet points. Do not invent details beyond the findings provided."

// proposeDigests rolls the trailing DigestDays of completed sessions into one
// monthly digest proposal per project, summarized by the chat client. It is a
// no-op without a chat client. Each digest is keyed by project + calendar month,
// so a month's digest is proposed at most once (and re-proposed only next month).
func (s *Service) proposeDigests(ctx context.Context, seen seenKeys) (int, error) {
	if s.chat == nil {
		return 0, nil
	}
	since := s.now().UTC().Add(-time.Duration(s.cfg.DigestDays) * 24 * time.Hour)
	sessions, err := store.CompletedSessionsSince(ctx, s.db, since)
	if err != nil {
		return 0, err
	}
	byProject := make(map[string][]core.Session)
	for _, sess := range sessions {
		byProject[sess.ProjectSlug] = append(byProject[sess.ProjectSlug], sess)
	}

	month := s.now().UTC().Format("2006-01")
	created := 0
	for project, group := range byProject {
		if len(group) < minDigestSessions {
			continue
		}
		key := "digest:" + project + ":" + month
		if seen.blocked(key) {
			continue
		}
		body, err := s.chat.Complete(ctx, digestSystemPrompt, digestUserPrompt(group))
		if err != nil {
			s.logger.Warn("gardener: digest completion", "project", project, "error", err)
			continue // one project's digest failing must not block the others
		}
		body = strings.TrimSpace(body)
		if body == "" {
			// Same class of failure as a completion error: the model produced
			// nothing usable, so there is no digest to propose. Logged rather
			// than returned -- one project must not block the others.
			s.logger.Warn("gardener: digest completion empty", "project", project)
			continue
		}
		// The model is recorded at generation time -- the config's chat model can
		// change between proposal and apply, and attribution names the producer.
		payload := map[string]any{
			"project": project, "month": month, "session_count": len(group),
			"title": digestTitle(project, month), "body": body,
			"model": s.chat.Model(),
		}
		// The digest folds its sessions' findings, so the note it writes may
		// contain what they may contain: the union of their provenance stamps,
		// recorded here beside the body it was written from. A digest of
		// unstamped sessions (every one, with isolation.requests off) carries
		// no key at all.
		stamps, err := s.foldedStamps(ctx, core.ItemKindSession, sessionIDs(group), project)
		if err != nil {
			return created, err
		}
		if len(stamps) > 0 {
			payload[payloadStampsKey] = stamps
		}
		if _, err := s.createProposal(ctx, store.ProposalDigest, key, payload, seen); err != nil {
			return created, err
		}
		created++
	}
	return created, nil
}

// digestUserPrompt renders the session findings as the digest's source material,
// newest first, one dated block per session.
func digestUserPrompt(sessions []core.Session) string {
	var b strings.Builder
	b.WriteString("Sessions to summarize:\n\n")
	for _, s := range sessions {
		b.WriteString("- ")
		b.WriteString(s.UpdatedAt.Format("2006-01-02"))
		if s.Name != "" {
			b.WriteString(" (" + s.Name + ")")
		}
		b.WriteString(": ")
		b.WriteString(s.Findings)
		b.WriteString("\n")
	}
	return b.String()
}

// sessionIDs lists the sessions' ids, in order.
func sessionIDs(sessions []core.Session) []string {
	out := make([]string, 0, len(sessions))
	for _, s := range sessions {
		out = append(out, s.ID)
	}
	return out
}

// digestTitle names a digest note for a project + month.
func digestTitle(project, month string) string {
	if project == "" {
		return "Session digest (global) -- " + month
	}
	return "Session digest: " + project + " -- " + month
}
