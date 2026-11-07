package console

import (
	"errors"
	"fmt"
	"net/http"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/screening"
	"github.com/arctop/seamless/internal/store"
)

// The Held screen's part-2 sections (write screening): the lockdown switch,
// the review's flagged items with a per-item Mark safe, the files in drift
// quarantine with Accept, and Re-run review. Records are read through the
// console's (possibly filtered) handle, so an item hidden from this console
// cannot be listed or acted on from here; file text is read only for a record
// that handle returned.

// heldInjectedLimit bounds the sessions listed under a flagged or drifted item.
const heldInjectedLimit = 5

// lockdownView is the lockdown panel.
type lockdownView struct {
	On       bool   `json:"on"`
	Console  bool   `json:"console"`
	File     bool   `json:"file"`
	FilePath string `json:"filePath,omitempty"`
	Source   string `json:"source,omitempty"`
}

// reviewView is the review panel's summary.
type reviewView struct {
	Armed      bool       `json:"armed"`
	Baseline   *time.Time `json:"baseline,omitempty"`
	Reviewed   *time.Time `json:"reviewed,omitempty"`
	Items      int        `json:"items"`
	Safe       int        `json:"safe"`
	Flagged    int        `json:"flagged"`
	Unreviewed int        `json:"unreviewed"`
	Drifted    int        `json:"drifted"`
}

// corpusRow is one flagged or drifted file.
type corpusRow struct {
	Path     string       `json:"path"`
	ItemKind string       `json:"itemKind"`
	ItemID   string       `json:"itemId,omitempty"`
	Project  string       `json:"project"`
	Link     string       `json:"link,omitempty"`
	Change   string       `json:"change,omitempty"`
	At       *time.Time   `json:"at,omitempty"`
	Codes    []heldReason `json:"codes"`
	Preview  string       `json:"preview,omitempty"`
	Missing  bool         `json:"missing,omitempty"`
	// Injected lists the sessions that received the item, newest first: who
	// read it while it was in the corpus.
	Injected      []injectedRow `json:"injected"`
	InjectedTotal int           `json:"injectedTotal"`
}

type injectedRow struct {
	SessionID string    `json:"sessionId,omitempty"`
	Name      string    `json:"name"`
	Times     int       `json:"times"`
	Last      time.Time `json:"last"`
}

// corpusSections fills the part-2 sections of the Held screen.
func (s *Service) corpusSections(r *http.Request, data *heldPageData) error {
	ctx := r.Context()
	if s.cfg.Screening != nil {
		st := s.cfg.Screening.Lockdown(ctx)
		data.Lockdown = lockdownView{On: st.On, Console: st.Console, File: st.File, FilePath: st.FilePath, Source: st.Source()}
	}
	armed, err := store.CorpusArmed(ctx, s.db(ctx))
	if err != nil {
		return err
	}
	counts, err := store.CountCorpus(ctx, s.db(ctx))
	if err != nil {
		return err
	}
	data.Review = reviewView{Armed: armed, Items: counts.Items, Safe: counts.Safe, Flagged: counts.Flagged,
		Unreviewed: counts.Unreviewed, Drifted: counts.Drifted}
	if t, ok, terr := store.SettingTime(ctx, s.db(ctx), store.SettingCorpusBaseline); terr != nil {
		return terr
	} else if ok {
		data.Review.Baseline = &t
	}
	if t, ok, terr := store.SettingTime(ctx, s.db(ctx), store.SettingCorpusReviewed); terr != nil {
		return terr
	} else if ok {
		data.Review.Reviewed = &t
	}
	flagged, err := store.FlaggedCorpusRecords(ctx, s.db(ctx))
	if err != nil {
		return err
	}
	for _, rec := range flagged {
		data.Flagged = append(data.Flagged, s.corpusRowOf(r, rec, rec.ReviewVerdicts, rec.ReviewedAt))
	}
	drifted, err := store.DriftedCorpusRecords(ctx, s.db(ctx))
	if err != nil {
		return err
	}
	for _, rec := range drifted {
		row := s.corpusRowOf(r, rec, rec.DriftVerdicts, rec.DriftedAt)
		row.Change = rec.DriftChange
		data.Drifted = append(data.Drifted, row)
	}
	return nil
}

func (s *Service) corpusRowOf(r *http.Request, rec store.CorpusRecord, codes []string, at *time.Time) corpusRow {
	row := corpusRow{Path: rec.FilePath, ItemKind: rec.ItemKind, ItemID: rec.ItemID, Project: rec.Project, At: at,
		Codes: []heldReason{}, Injected: []injectedRow{}}
	if rec.ItemID != "" {
		row.Link = itemLink(rec.ItemKind, rec.ItemID)
	}
	for _, c := range codes {
		row.Codes = append(row.Codes, heldReason{Code: c, Words: screening.Describe(screening.Code(c))})
	}
	if s.cfg.Files != nil {
		raw, err := s.cfg.Files.Store().ReadFile(rec.FilePath)
		if err != nil {
			row.Missing = true
		} else {
			row.Preview = heldPreview(store.HeldWrite{Content: raw})
		}
	}
	if rec.ItemKind == core.ItemKindMemory && rec.ItemID != "" {
		sessions, err := store.InjectedSessions(r.Context(), s.db(r.Context()), rec.ItemID, 0)
		if err != nil {
			s.logger.Warn("console: injected sessions", "id", rec.ItemID, "error", err)
		}
		row.InjectedTotal = len(sessions)
		for i, x := range sessions {
			if i == heldInjectedLimit {
				break
			}
			name := x.Name
			if name == "" {
				name = "(no session)"
			}
			row.Injected = append(row.Injected, injectedRow{SessionID: x.SessionID, Name: name, Times: x.Times, Last: x.Last})
		}
	}
	return row
}

// heldLockdown flips the owner's lockdown switch.
func (s *Service) heldLockdown(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Screening == nil {
		s.serverError(w, r, errors.New("console: write screening is not wired on this daemon"))
		return
	}
	var on bool
	switch r.PostFormValue("on") {
	case "1":
		on = true
	case "0":
		on = false
	default:
		heldFlash(w, r, "on must be 1 (lockdown on) or 0 (off)")
		return
	}
	st, err := s.cfg.Screening.SetLockdown(r.Context(), on)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, map[string]any{"on": st.On, "console": st.Console, "file": st.File})
		return
	}
	switch {
	case on:
		heldNotice(w, r, "Lockdown on: only memories you confirmed or the review marked safe are injected, and changes to binding rules wait for you.")
	case st.File:
		heldNotice(w, r, "Your switch is off, but lockdown stays on while the lockdown file exists.")
	default:
		heldNotice(w, r, "Lockdown off.")
	}
}

// heldReview re-runs the review over the whole corpus.
func (s *Service) heldReview(w http.ResponseWriter, r *http.Request) {
	if s.cfg.Screening == nil {
		s.serverError(w, r, errors.New("console: write screening is not wired on this daemon"))
		return
	}
	sum, err := s.cfg.Screening.Review(r.Context())
	if err != nil {
		s.serverError(w, r, fmt.Errorf("console: review: %w", err))
		return
	}
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, map[string]any{"items": sum.Items, "safe": sum.Safe, "flagged": sum.Flagged})
		return
	}
	heldNotice(w, r, fmt.Sprintf("Reviewed %d items: %d safe, %d flagged for you.", sum.Items, sum.Safe, sum.Flagged))
}

// heldCorpusTarget reads the record a corpus action names through the
// console's handle: a record the filtered view hides is not found from here.
func (s *Service) heldCorpusTarget(w http.ResponseWriter, r *http.Request) (store.CorpusRecord, bool) {
	if s.cfg.Screening == nil {
		s.serverError(w, r, errors.New("console: write screening is not wired on this daemon"))
		return store.CorpusRecord{}, false
	}
	path := r.PostFormValue("path")
	if path == "" {
		heldFlash(w, r, "path is required: the memory or note file to act on")
		return store.CorpusRecord{}, false
	}
	rec, ok, err := store.CorpusRecordByPath(r.Context(), s.db(r.Context()), path)
	if err != nil {
		s.serverError(w, r, err)
		return store.CorpusRecord{}, false
	}
	if !ok {
		s.notFound(w, r, "No reviewed or quarantined file at "+path+".")
		return store.CorpusRecord{}, false
	}
	return rec, true
}

// heldMarkSafe is the owner's mark on a flagged item.
func (s *Service) heldMarkSafe(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.heldCorpusTarget(w, r)
	if !ok {
		return
	}
	if err := s.cfg.Screening.MarkSafe(r.Context(), rec.FilePath); err != nil {
		if errors.Is(err, screening.ErrNotCorpus) {
			heldFlash(w, r, "That file is gone or no longer reads as a memory or note.")
			return
		}
		s.serverError(w, r, fmt.Errorf("console: mark safe: %w", err))
		return
	}
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, map[string]any{"path": rec.FilePath, "status": store.ReviewSafe})
		return
	}
	heldNotice(w, r, "Marked safe: "+rec.FilePath+" (as it reads now; any later change lapses the mark).")
}

// heldAcceptDrift accepts a file changed outside Seamless as it stands.
func (s *Service) heldAcceptDrift(w http.ResponseWriter, r *http.Request) {
	rec, ok := s.heldCorpusTarget(w, r)
	if !ok {
		return
	}
	err := s.cfg.Screening.AcceptDrift(r.Context(), rec.FilePath)
	switch {
	case errors.Is(err, screening.ErrNotDrifted):
		heldFlash(w, r, "That file is no longer in drift quarantine.")
		return
	case errors.Is(err, screening.ErrNotCorpus):
		heldFlash(w, r, "That file is gone or no longer reads as a memory or note.")
		return
	case err != nil:
		s.serverError(w, r, fmt.Errorf("console: accept drift: %w", err))
		return
	}
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, map[string]any{"path": rec.FilePath, "accepted": true})
		return
	}
	heldNotice(w, r, "Accepted: "+rec.FilePath+" is recorded as it stands and reaches agents again.")
}
