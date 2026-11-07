package console

import (
	"context"
	"time"

	"github.com/arctop/seamless/internal/store"
)

// healthFact is one statement on the Home health strip -- the Basic owner's
// first question, "is it working?" -- linked to where it is explained.
type healthFact struct {
	Icon string
	Text string
	Href string
	// Title is the hover detail (the off reason, the exact time).
	Title string
	// Tone is "ok", "warn", or "" (neutral).
	Tone string
}

// healthFacts builds the strip: which agent clients are working with this
// installation, whether semantic recall is on, when the last briefing went out,
// and the version -- with what the update check knows about it (versionFact).
// Every fact is omitted when nothing backs it -- a fresh installation says
// less, never a confident zero -- and a read failure costs the facts it feeds,
// never the page.
func (s *Service) healthFacts(ctx context.Context) []healthFact {
	var out []healthFact
	facts, err := store.GetHealthFacts(ctx, s.db(ctx))
	if err != nil {
		s.logger.Warn("console: health strip", "error", err)
	}
	if len(facts.Clients) > 0 {
		parts := make([]string, 0, len(facts.Clients))
		for i, c := range facts.Clients {
			_, _, name := agentDisplay(c.Client)
			when := agoPhrase(c.LastSeen)
			if i == 0 {
				when = "last active " + when
			}
			parts = append(parts, name+" ("+when+")")
		}
		out = append(out, healthFact{
			Icon: "terminal", Text: "Working with " + joinWithAnd(parts), Href: "/console/sessions",
			Title: "Agent clients that have recorded sessions here", Tone: "ok",
		})
	}
	if emb := s.cfg.Embedding; emb.Enabled || emb.Provider != "" || emb.Reason != "" {
		f := healthFact{Icon: "brain", Href: "/console/settings?s=setup"}
		if emb.Enabled {
			f.Text, f.Tone, f.Title = "semantic recall on", "ok", "Embeddings with "+emb.Model
		} else {
			f.Text, f.Tone = "semantic recall off", "warn"
			f.Title = "Recall is keyword-only"
			if emb.Reason != "" {
				f.Title += ": " + emb.Reason
			}
		}
		out = append(out, f)
	}
	if b := facts.LastBriefing; b != nil {
		out = append(out, healthFact{
			Icon: "arrow-down-to-line", Text: "last briefing delivered " + agoPhrase(b.At),
			Href: "/console/events/" + b.EventID, Title: ts(b.At),
		})
	}
	if f, ok := s.versionFact(time.Now()); ok {
		out = append(out, f)
	}
	return out
}

// agoPhrase is ago() as a phrase: "4m ago", or "just now".
func agoPhrase(v any) string {
	a := ago(v)
	if a == "just now" || a == "now" || a == "-" {
		return a
	}
	return a + " ago"
}
