package console

// The Trials screen is the flat, filterable view over every recorded trial --
// the console twin of the trial_query MCP tool. It is a library screen: a rail
// grouped by lab (newest activity first), filterable by ?lab= and ?outcome=,
// beside a reader showing one trial's full expected-vs-actual record and its
// structured metrics. Outcomes are free-form by design, so ?outcome= is an
// exact-match filter rather than a validated enum; the seg offers the
// conventional values.

import (
	"context"
	"html/template"
	"net/http"
	"net/url"
	"sort"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// trialListLimit caps the rail; QueryTrials returns newest first, so the cut
// drops the oldest.
const trialListLimit = 200

// trialOutcomeSegs are the outcome filters the rail's seg offers: everything,
// then the conventional outcome values. Free-form outcomes remain reachable by
// URL (?outcome=<anything>); the seg simply shows no active entry then.
var trialOutcomeSegs = []string{"", "pass", "fail", "partial", "inconclusive"}

// trialRow is a display projection of one trial for the rail.
type trialRow struct {
	ID       string    `json:"id"`
	Lab      string    `json:"lab"`
	Title    string    `json:"title"`
	Outcome  string    `json:"outcome,omitempty"`
	Project  string    `json:"project,omitempty"`
	Favorite bool      `json:"favorite,omitempty"`
	Metrics  int       `json:"metrics,omitempty"` // structured metric count
	Created  time.Time `json:"created"`
}

// trialLabGroup is one lab's trials in the rail, ordered newest first; groups
// themselves follow the newest trial across the filtered list.
type trialLabGroup struct {
	Lab    string     `json:"lab"`
	Count  int        `json:"count"`
	Trials []trialRow `json:"trials"`
}

// trialsData is the payload for the Trials library screen. The Selected/QS
// fields drive the HTML reader pane only.
type trialsData struct {
	Groups  []trialLabGroup `json:"groups"`
	Count   int             `json:"count"`
	Lab     string          `json:"lab,omitempty"`     // active ?lab= filter
	Outcome string          `json:"outcome,omitempty"` // active ?outcome= filter
	// Selected is the trial open in the reader: the requested one on a
	// /console/trials/{id} page, or the newest match on the list URL
	// (SelectedAuto, which the client pins into the URL).
	Selected     *trialDetailData `json:"-"`
	SelectedAuto bool             `json:"-"`
	// QS is the ?lab=&outcome= suffix rail links carry so the active filter
	// survives a selection change ("" when both are unset).
	QS string `json:"-"`
}

// trialDetailData is the single-trial payload: the record's what-changed /
// expected / actual sections rendered as markdown, its structured metrics, and
// its provenance (lab, project, recording session).
type trialDetailData struct {
	ID          string        `json:"id"`
	Lab         string        `json:"lab"`
	Title       string        `json:"title"`
	Outcome     string        `json:"outcome,omitempty"`
	Project     string        `json:"project,omitempty"`
	Favorite    bool          `json:"favorite,omitempty"`
	Changes     template.HTML `json:"-"`
	ChangesText string        `json:"changes,omitempty"`
	Expected    template.HTML `json:"-"`
	ExpText     string        `json:"expected,omitempty"`
	Actual      template.HTML `json:"-"`
	ActText     string        `json:"actual,omitempty"`
	Metrics     []kvPair      `json:"metrics,omitempty"`
	SessionID   string        `json:"sessionId,omitempty"`
	SessionName string        `json:"sessionName,omitempty"`
	Created     time.Time     `json:"created"`
}

// trialsQS renders the ?lab=&outcome= suffix rail links carry so a selection
// change keeps the active filters ("" when both are unset).
func trialsQS(lab, outcome string) string {
	v := url.Values{}
	if lab != "" {
		v.Set("lab", lab)
	}
	if outcome != "" {
		v.Set("outcome", outcome)
	}
	if len(v) == 0 {
		return ""
	}
	return "?" + v.Encode()
}

// trialsPage assembles the lab-grouped trial list for the given filters.
func (s *Service) trialsPage(ctx context.Context, lab, outcome string) (trialsData, error) {
	trials, err := store.QueryTrials(ctx, s.db(ctx), store.TrialFilter{
		Lab: lab, Outcome: outcome, Limit: trialListLimit,
	})
	if err != nil {
		return trialsData{}, err
	}
	data := trialsData{Lab: lab, Outcome: outcome, Count: len(trials), QS: trialsQS(lab, outcome)}
	// Group by lab, preserving the newest-first order both across groups (a
	// group sits where its newest trial does) and within each group.
	idx := map[string]int{}
	for _, tr := range trials {
		row := trialRow{
			ID: tr.ID, Lab: tr.Lab, Title: tr.Title, Outcome: string(tr.Outcome),
			Project: tr.ProjectSlug, Favorite: tr.Favorite,
			Metrics: len(tr.Metrics), Created: tr.CreatedAt,
		}
		i, ok := idx[tr.Lab]
		if !ok {
			i = len(data.Groups)
			idx[tr.Lab] = i
			data.Groups = append(data.Groups, trialLabGroup{Lab: tr.Lab})
		}
		data.Groups[i].Trials = append(data.Groups[i].Trials, row)
		data.Groups[i].Count++
	}
	return data, nil
}

func (s *Service) trialsList(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	lab := r.URL.Query().Get("lab")
	outcome := r.URL.Query().Get("outcome")
	data, err := s.trialsPage(ctx, lab, outcome)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	// The HTML library auto-opens the newest match in the reader.
	if !wantsJSON(r) && len(data.Groups) > 0 {
		d, found, derr := s.trialDetailByID(ctx, data.Groups[0].Trials[0].ID)
		if derr != nil {
			s.serverError(w, r, derr)
			return
		}
		if found {
			data.Selected = &d
			data.SelectedAuto = true
		}
	}
	s.render(w, r, "trials", pageData{Title: "Trials", Active: "trials", Data: data})
}

// trialDetailData projects a trial into its detail payload. The what-changed /
// expected / actual sections get the same markdown treatment as task bodies,
// scoped to the trial's project; the metrics map flattens into sorted rows.
func (s *Service) trialDetailData(ctx context.Context, tr core.Trial) trialDetailData {
	d := trialDetailData{
		ID: tr.ID, Lab: tr.Lab, Title: tr.Title, Outcome: string(tr.Outcome),
		Project: tr.ProjectSlug, Favorite: tr.Favorite,
		SessionID: tr.SessionID, Created: tr.CreatedAt,
		ChangesText: tr.Changes, ExpText: tr.Expected, ActText: tr.Actual,
	}
	if tr.Changes != "" {
		d.Changes = s.renderBody(ctx, tr.Changes, tr.ProjectSlug)
	}
	if tr.Expected != "" {
		d.Expected = s.renderBody(ctx, tr.Expected, tr.ProjectSlug)
	}
	if tr.Actual != "" {
		d.Actual = s.renderBody(ctx, tr.Actual, tr.ProjectSlug)
	}
	keys := make([]string, 0, len(tr.Metrics))
	for k := range tr.Metrics {
		keys = append(keys, k)
	}
	sort.Strings(keys)
	for _, k := range keys {
		if v := stringifyValue(tr.Metrics[k]); v != "" {
			d.Metrics = append(d.Metrics, kvPair{Key: k, Value: v})
		}
	}
	// Provenance: the recording session, resolved best-effort for its name.
	if tr.SessionID != "" {
		if sess, ok, serr := store.SessionByID(ctx, s.db(ctx), tr.SessionID); serr != nil {
			s.logger.Warn("console: trial session", "id", tr.SessionID, "error", serr)
		} else if ok {
			d.SessionName = sess.Name
		}
	}
	return d
}

// trialDetailByID loads a trial and projects it into its detail payload.
// found=false means no such trial.
func (s *Service) trialDetailByID(ctx context.Context, id string) (trialDetailData, bool, error) {
	tr, ok, err := store.TrialByID(ctx, s.db(ctx), id)
	if err != nil || !ok {
		return trialDetailData{}, ok, err
	}
	return s.trialDetailData(ctx, tr), true, nil
}

func (s *Service) trialDetail(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	id := r.PathValue("id")
	d, found, err := s.trialDetailByID(ctx, id)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	if !found {
		s.notFound(w, r, "No trial with id "+id+".")
		return
	}
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, d)
		return
	}
	if r.URL.Query().Get("peek") == "1" {
		s.renderFragment(w, r, "trial", d)
		return
	}
	if r.URL.Query().Get("reader") == "1" {
		s.renderReader(w, r, "trials", "trial-reader", d)
		return
	}
	// Default: the trials library page with this trial open in the reader,
	// keeping whatever filters the URL carries.
	data, err := s.trialsPage(ctx, r.URL.Query().Get("lab"), r.URL.Query().Get("outcome"))
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	data.Selected = &d
	s.render(w, r, "trials", pageData{Title: "Trial " + shortID(id), Active: "trials", Data: data})
}

// outcomeTone maps a trial outcome to a badge tone class: pass green, fail red,
// partial amber; inconclusive and free-form outcomes stay neutral.
func outcomeTone(outcome string) string {
	switch outcome {
	case string(core.OutcomePass):
		return "ok"
	case string(core.OutcomeFail):
		return "danger"
	case string(core.OutcomePartial):
		return "warn"
	default:
		return ""
	}
}

// outcomeSegs builds the Trials rail's outcome selector entries, flagging the
// active one.
type outcomeSeg struct {
	Key    string
	Label  string
	Active bool
}

func outcomeSegs(active string) []outcomeSeg {
	out := make([]outcomeSeg, 0, len(trialOutcomeSegs))
	for _, k := range trialOutcomeSegs {
		label := k
		if k == "" {
			label = "all"
		}
		out = append(out, outcomeSeg{Key: k, Label: label, Active: k == active})
	}
	return out
}
