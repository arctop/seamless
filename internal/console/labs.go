package console

// The Labs screen surfaces the research labs (the lab_open / trial_record /
// trial_query surface): one row per lab -- a stable label for a line of
// investigation -- with its outcome tallies and trial history. It is a library
// screen like the other library entities: a rail beside a reader. A lab is not
// a stored entity, only the label its trials carry, so everything here is an
// aggregation over the trials table and there is nothing to write.

import (
	"context"
	"net/http"
	"net/url"
	"time"

	"github.com/arctop/seamless/internal/store"
)

// labTrialLimit caps the trial history a lab reader lists; the count of the
// rest renders alongside, with the Trials screen as the uncapped view.
const labTrialLimit = 100

// labRow is a display projection of one lab's summary.
type labRow struct {
	Lab          string    `json:"lab"`
	Trials       int       `json:"trials"`
	Pass         int       `json:"pass"`
	Fail         int       `json:"fail"`
	Partial      int       `json:"partial"`
	Inconclusive int       `json:"inconclusive"`
	Other        int       `json:"other"` // free-form or empty outcomes
	Projects     []string  `json:"projects"`
	Sessions     int       `json:"sessions"`
	First        time.Time `json:"first"`
	Last         time.Time `json:"last"`
}

// labsData is the payload for the Labs library screen. Selected drives the
// HTML reader pane only; JSON callers get the lean lab list.
type labsData struct {
	Rows      []labRow `json:"labs"`
	Count     int      `json:"count"`
	TrialsSum int      `json:"trials"`
	// Selected is the lab open in the reader: the requested one on a
	// /console/labs/{name} page, or the most recently active one on the list
	// URL (SelectedAuto, which the client pins into the URL).
	Selected     *labDetailData `json:"-"`
	SelectedAuto bool           `json:"-"`
}

// labTrialRef is a compact pointer to one of a lab's trials, for the reader's
// history list.
type labTrialRef struct {
	ID      string    `json:"id"`
	Title   string    `json:"title"`
	Outcome string    `json:"outcome,omitempty"`
	Project string    `json:"project,omitempty"`
	Account string    `json:"account,omitempty"` // restricted account that recorded it ("" = owner)
	Created time.Time `json:"created"`
}

// labDetailData is the single-lab payload: the summary row plus its trial
// history, newest first, capped at labTrialLimit.
type labDetailData struct {
	Row    labRow        `json:"lab"`
	Trials []labTrialRef `json:"trials"`
	More   int           `json:"more"` // trials beyond the cap
}

func toLabRow(l store.LabSummary) labRow {
	return labRow{
		Lab: l.Lab, Trials: l.Trials,
		Pass: l.Pass, Fail: l.Fail, Partial: l.Partial,
		Inconclusive: l.Inconclusive, Other: l.Other,
		Projects: l.Projects, Sessions: l.Sessions,
		First: l.FirstAt, Last: l.LastAt,
	}
}

// labsPage assembles the lab list, most recently active first.
func (s *Service) labsPage(ctx context.Context) (labsData, error) {
	labs, err := store.ListLabs(ctx, s.db(ctx))
	if err != nil {
		return labsData{}, err
	}
	data := labsData{Rows: make([]labRow, 0, len(labs)), Count: len(labs)}
	for _, l := range labs {
		data.Rows = append(data.Rows, toLabRow(l))
		data.TrialsSum += l.Trials
	}
	return data, nil
}

func (s *Service) labsList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	data, err := s.labsPage(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	// The HTML library auto-opens the most recently active lab in the reader.
	if !wantsJSON(r) && len(data.Rows) > 0 {
		d, found, derr := s.labDetailByName(ctx, data.Rows[0].Lab)
		if derr != nil {
			s.serverError(w, r, derr)
			return
		}
		if found {
			data.Selected = &d
			data.SelectedAuto = true
		}
	}
	s.render(w, r, "labs", pageData{Title: "Labs", Active: "labs", Data: data})
}

// labDetailByName assembles a lab's detail payload: its summary row and its
// trial history. found=false means no trial carries the lab label.
func (s *Service) labDetailByName(ctx context.Context, name string) (labDetailData, bool, error) {
	labs, err := store.ListLabs(ctx, s.db(ctx))
	if err != nil {
		return labDetailData{}, false, err
	}
	var d labDetailData
	found := false
	for _, l := range labs {
		if l.Lab == name {
			d.Row = toLabRow(l)
			found = true
			break
		}
	}
	if !found {
		return labDetailData{}, false, nil
	}
	trials, err := store.QueryTrials(ctx, s.db(ctx), store.TrialFilter{Lab: name, Limit: labTrialLimit})
	if err != nil {
		return labDetailData{}, false, err
	}
	for _, tr := range trials {
		d.Trials = append(d.Trials, labTrialRef{
			ID: tr.ID, Title: tr.Title, Outcome: string(tr.Outcome),
			Project: tr.ProjectSlug, Account: tr.Account, Created: tr.CreatedAt,
		})
	}
	if d.Row.Trials > len(d.Trials) {
		d.More = d.Row.Trials - len(d.Trials)
	}
	return d, true, nil
}

func (s *Service) labDetail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	// The {name...} wildcard also matches the bare trailing slash.
	name := r.PathValue("name")
	if name == "" {
		http.Redirect(w, r, "/console/labs", http.StatusSeeOther)
		return
	}
	d, found, err := s.labDetailByName(ctx, name)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !found {
		s.notFound(w, r, "No lab named "+name+".")
		return
	}
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, d)
		return
	}
	if r.URL.Query().Get("reader") == "1" {
		s.renderReader(w, r, "labs", "lab-reader", d)
		return
	}
	// Default: the labs library page with this lab open in the reader.
	data, err := s.labsPage(ctx)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	data.Selected = &d
	s.render(w, r, "labs", pageData{Title: "Lab " + name, Active: "labs", Data: data})
}

// labPath renders a lab's canonical console path, escaping the free-form lab
// name so it survives as one path segment.
func labPath(lab string) string {
	return "/console/labs/" + url.PathEscape(lab)
}
