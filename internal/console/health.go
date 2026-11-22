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
// a warning while backups are missing or old (backupFact), and the version --
// with what the update check knows about it (versionFact).
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
	if f, ok := s.backupFact(time.Now()); ok {
		out = append(out, f)
	}
	if f, ok := s.versionFact(time.Now()); ok {
		out = append(out, f)
	}
	return out
}

// backupFact warns while there is no backup at all, or the newest is older
// than a week (backupOldAfter), linked to Settings > Backups. Backups that are
// current say nothing here: the line is for what to look at. ok is false
// while no backup service runs in this process, and when nothing is wrong.
func (s *Service) backupFact(now time.Time) (healthFact, bool) {
	if s.cfg.Backups == nil {
		return healthFact{}, false
	}
	st := s.cfg.Backups.Status()
	f := healthFact{Icon: "archive", Href: backupsSectionHref, Tone: "warn"}
	switch {
	case st.LastBackup == nil:
		if st.Running != nil {
			return healthFact{}, false
		}
		f.Text, f.Title = "no backup yet", "Seamless has made no backup of this installation"
	case now.Sub(st.LastBackup.At) > backupOldAfter:
		f.Text, f.Title = "newest backup "+ago(st.LastBackup.At)+" old", "The newest backup was made "+ts(st.LastBackup.At)
	default:
		return healthFact{}, false
	}
	return f, true
}

// agoPhrase is ago() as a phrase: "4m ago", or "just now".
func agoPhrase(v any) string {
	a := ago(v)
	if a == "just now" || a == "now" || a == "-" {
		return a
	}
	return a + " ago"
}
