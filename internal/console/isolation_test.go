package console

import (
	"bytes"
	"context"
	"database/sql"
	"encoding/json"
	"html/template"
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// postIsolation issues an authenticated (bearer) isolation write against the
// browser path (form body, HTML response).
func postIsolation(mux *http.ServeMux, slug string, form url.Values) *httptest.ResponseRecorder {
	return postForm(mux, "/console/projects/"+slug+"/isolation", form.Encode())
}

// seedIsolationProject registers slug with a fresh console DB.
func seedIsolationProject(t *testing.T, db *sql.DB, slug string) {
	t.Helper()
	_, err := store.EnsureProject(context.Background(), db, slug, slug)
	require.NoError(t, err)
}

func isolationOfSlug(t *testing.T, db *sql.DB, slug string) core.Isolation {
	t.Helper()
	state, err := store.IsolationOf(context.Background(), db, slug)
	require.NoError(t, err)
	return state
}

func TestProjectIsolation_StateAllowlist(t *testing.T) {
	db, mux := newConsole(t)
	seedIsolationProject(t, db, "acme")
	require.NoError(t, store.SetProjectIsolation(context.Background(), db, "acme", core.IsolationConfidential))

	for _, tt := range []struct {
		name string
		form url.Values
		code int
	}{
		{"missing", url.Values{}, http.StatusBadRequest},
		{"unknown state", url.Values{"state": {"private"}}, http.StatusBadRequest},
		{"empty state", url.Values{"state": {""}}, http.StatusBadRequest},
		{"state twice", url.Values{"state": {"open", "sealed"}}, http.StatusBadRequest},
		{"confirm twice", url.Values{"state": {"sealed"}, "confirm": {"1", "1"}}, http.StatusBadRequest},
		{"confirm not 1", url.Values{"state": {"sealed"}, "confirm": {"yes"}}, http.StatusBadRequest},
	} {
		t.Run(tt.name, func(t *testing.T) {
			rr := postIsolation(mux, "acme", tt.form)
			require.Equal(t, tt.code, rr.Code, rr.Body.String())
			require.Equal(t, core.IsolationConfidential, isolationOfSlug(t, db, "acme"),
				"invalid input must never move the fence")
		})
	}

	// An unknown project is a 404, not a 500 and not a silent no-op.
	rr := postIsolation(mux, "nope", url.Values{"state": {"sealed"}})
	require.Equal(t, http.StatusNotFound, rr.Code)
}

func TestProjectIsolation_TightenConfirmsThenApplies(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for _, slug := range []string{"acme", "acme-root", "sibling"} {
		seedIsolationProject(t, db, slug)
	}
	require.NoError(t, store.SetProjectParent(ctx, db, "acme", "acme-root", now))
	_, err := store.SaveProjectFamily(ctx, db, "", "product", []string{"acme", "sibling"})
	require.NoError(t, err)

	// Step one: the tighten does not write, it asks.
	rr := postIsolation(mux, "acme", url.Values{"state": {"confidential"}})
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Equal(t, "/console/projects/acme?tab=overview&isolate=confidential#isolation",
		rr.Header().Get("Location"))
	require.Equal(t, core.IsolationOpen, isolationOfSlug(t, db, "acme"),
		"an unconfirmed tighten must write nothing")

	// The confirm panel states exactly what the apply will change.
	page := getHTMLBody(t, mux, "/console/projects/acme?tab=overview&isolate=confidential")
	require.Contains(t, page, `id="isolation-confirm"`)
	require.Contains(t, page, "Make acme confidential?")
	require.Contains(t, page, "nothing leaves")
	require.Contains(t, page, "Leaves family <b>product</b>")
	require.Contains(t, page, "Detaches from parent")
	require.Contains(t, page, `name="confirm" value="1"`)

	// Step two: confirmed, one transaction, topology detached with it.
	rr = postIsolation(mux, "acme", url.Values{"state": {"confidential"}, "confirm": {"1"}})
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Contains(t, rr.Header().Get("Location"), "notice=")
	require.Equal(t, core.IsolationConfidential, isolationOfSlug(t, db, "acme"))

	families, err := store.ProjectFamilies(ctx, db)
	require.NoError(t, err)
	require.NotContains(t, families["product"], "acme", "a tighten leaves every family")
	p, found, err := store.ProjectBySlug(ctx, db, "acme")
	require.NoError(t, err)
	require.True(t, found)
	require.Empty(t, p.ParentSlug, "a tighten detaches the parent link")

	// Honest states: the page badges the fence and the change shows up in the
	// event feed, and nothing else on the page shouts.
	page = getHTMLBody(t, mux, "/console/projects/acme?tab=overview")
	require.Contains(t, page, `class="kind accent iso-pill"`)
	require.Contains(t, page, "isolation open -&gt; confidential")
	require.NotContains(t, page, `id="isolation-confirm"`)
}

func TestProjectIsolation_BlockedByChildren(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for _, slug := range []string{"acme", "acme-ios"} {
		seedIsolationProject(t, db, slug)
	}
	require.NoError(t, store.SetProjectParent(ctx, db, "acme-ios", "acme", now))

	// The browser lands on the panel that names the children to re-parent.
	rr := postIsolation(mux, "acme", url.Values{"state": {"sealed"}, "confirm": {"1"}})
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Equal(t, "/console/projects/acme?tab=overview&isolate=sealed#isolation",
		rr.Header().Get("Location"))
	require.Equal(t, core.IsolationOpen, isolationOfSlug(t, db, "acme"),
		"a blocked tighten writes nothing")

	page := getHTMLBody(t, mux, "/console/projects/acme?tab=overview&isolate=sealed")
	require.Contains(t, page, "acme cannot become sealed yet.")
	require.Contains(t, page, "Re-parent 1 child project first")
	require.Contains(t, page, `href="/console/projects/acme-ios"`)
	require.NotContains(t, page, `name="confirm" value="1"`,
		"a blocked panel offers no confirm button")

	// A bearer caller gets the refusal as a 409 whose error names the remedy.
	var out isolationResponse
	rr = postIsolationJSON(mux, "acme", url.Values{"state": {"sealed"}, "confirm": {"1"}})
	require.Equal(t, http.StatusConflict, rr.Code)
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
	require.True(t, out.Blocked)
	require.False(t, out.Applied)
	require.Equal(t, core.IsolationOpen, out.State)
	require.Contains(t, out.Error, "Re-parent acme-ios")
	require.NotNil(t, out.Effects)
	require.Equal(t, []string{"acme-ios"}, out.Effects.Children)
}

func TestProjectIsolation_LoosenIsImmediate(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	seedIsolationProject(t, db, "acme")
	require.NoError(t, store.SetProjectIsolation(ctx, db, "acme", core.IsolationSealed))

	// sealed -> confidential and confidential -> open both apply on the click:
	// nothing leaked while the fence was up, so the change is reversible.
	for _, state := range []core.Isolation{core.IsolationConfidential, core.IsolationOpen} {
		rr := postIsolation(mux, "acme", url.Values{"state": {string(state)}})
		require.Equal(t, http.StatusSeeOther, rr.Code)
		require.Contains(t, rr.Header().Get("Location"), "notice=")
		require.Equal(t, state, isolationOfSlug(t, db, "acme"))
	}

	// Re-requesting the state a standalone project already holds is a legal
	// no-op, so a double submit cannot strand the owner on a confirm panel.
	rr := postIsolation(mux, "acme", url.Values{"state": {"confidential"}, "confirm": {"1"}})
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Equal(t, core.IsolationConfidential, isolationOfSlug(t, db, "acme"))
	rr = postIsolation(mux, "acme", url.Values{"state": {"confidential"}})
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Contains(t, rr.Header().Get("Location"), "notice=")
	require.Equal(t, core.IsolationConfidential, isolationOfSlug(t, db, "acme"))
}

// postIsolationJSON drives the route the way `seam` will: bearer key, no body,
// arguments in the query string, JSON out.
func postIsolationJSON(mux *http.ServeMux, slug string, form url.Values) *httptest.ResponseRecorder {
	form.Set("format", "json")
	req := httptest.NewRequest(http.MethodPost, "/console/projects/"+slug+"/isolation?"+form.Encode(), nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	req.Header.Set("Accept", "application/json")
	return do(mux, req)
}

func TestProjectIsolation_JSONBranchServesTheCLI(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	now := time.Now().UTC()
	for _, slug := range []string{"acme", "acme-root"} {
		seedIsolationProject(t, db, slug)
	}
	require.NoError(t, store.SetProjectParent(ctx, db, "acme", "acme-root", now))

	// Unconfirmed tighten: 200 with the consequences to print, nothing written.
	var out isolationResponse
	rr := postIsolationJSON(mux, "acme", url.Values{"state": {"sealed"}})
	require.Equal(t, http.StatusOK, rr.Code)
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
	require.Equal(t, "acme", out.Project)
	require.Equal(t, core.IsolationSealed, out.Requested)
	require.Equal(t, core.IsolationOpen, out.State, "state is what the project holds now")
	require.Equal(t, isolationPromises[core.IsolationOpen], out.Promise)
	require.Equal(t, isolationPromises[core.IsolationSealed], out.RequestedPromise)
	require.True(t, out.ConfirmRequired)
	require.False(t, out.Applied)
	require.False(t, out.Blocked)
	require.Equal(t, 0, out.GlobalMemories, "the audit ran and honestly found nothing to relocate")
	require.Equal(t, -1, isolationProvenanceCount(isolationProvenanceVM{}),
		"an UNCOMPUTED count still reports -1, never a zero the CLI reads as a finding")
	require.NotNil(t, out.Effects)
	require.Equal(t, "acme-root", out.Effects.Parent)
	require.Contains(t, out.Message, "Detached from parent acme-root")
	require.Equal(t, core.IsolationOpen, isolationOfSlug(t, db, "acme"))

	// Confirmed: applied, with the topology it gave up.
	out = isolationResponse{}
	rr = postIsolationJSON(mux, "acme", url.Values{"state": {"sealed"}, "confirm": {"1"}})
	require.Equal(t, http.StatusOK, rr.Code)
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
	require.True(t, out.Applied)
	require.False(t, out.ConfirmRequired)
	require.Equal(t, core.IsolationSealed, out.State)
	require.Equal(t, isolationPromises[core.IsolationSealed], out.Promise)
	require.NotNil(t, out.Effects)
	require.Equal(t, "acme-root", out.Effects.Parent)
	require.Equal(t, core.IsolationSealed, isolationOfSlug(t, db, "acme"))

	// Loosening: applied straight away, no effects to report.
	out = isolationResponse{}
	rr = postIsolationJSON(mux, "acme", url.Values{"state": {"open"}})
	require.Equal(t, http.StatusOK, rr.Code)
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &out))
	require.True(t, out.Applied)
	require.Equal(t, core.IsolationOpen, out.State)
	require.Nil(t, out.Effects)

	// A bad state is a 400 carrying the accepted values, not a default.
	rr = postIsolationJSON(mux, "acme", url.Values{"state": {"private"}})
	require.Equal(t, http.StatusBadRequest, rr.Code)
	require.Contains(t, rr.Body.String(), "valid values are open, confidential, sealed")

	// The read path the CLI prints from without writing anything.
	var summary projectDetail
	getJSON(t, mux, "/console/projects/acme?format=json", &summary)
	require.Equal(t, core.IsolationOpen, summary.Isolation)
	require.Equal(t, isolationPromises[core.IsolationOpen], summary.IsolationPromise)
}

func TestProjectWorkspace_BadIsolateParamIs400(t *testing.T) {
	db, mux := newConsole(t)
	seedIsolationProject(t, db, "acme")

	for _, path := range []string{
		"/console/projects/acme?isolate=private",
		"/console/projects/acme?isolate=",
		"/console/projects/acme?isolate=sealed&isolate=open",
	} {
		req := httptest.NewRequest(http.MethodGet, path, nil)
		req.Header.Set("Authorization", "Bearer "+testKey)
		require.Equal(t, http.StatusBadRequest, do(mux, req).Code, "GET %s", path)
	}

	// A state that is no longer a tighten renders no panel: the control below it
	// already shows the truth, and a panel for a change that cannot happen would
	// be exactly the fake result the strict-param policy exists to prevent.
	page := getHTMLBody(t, mux, "/console/projects/acme?isolate=open")
	require.NotContains(t, page, `id="isolation-confirm"`)
}

func TestIsolationControl_RendersEveryPromise(t *testing.T) {
	db, mux := newConsole(t)
	seedIsolationProject(t, db, "acme")

	page := getHTMLBody(t, mux, "/console/projects/acme?tab=overview")
	require.Contains(t, page, `action="/console/projects/acme/isolation"`)
	for _, state := range core.Isolations {
		require.Contains(t, page, `name="state" value="`+string(state)+`"`)
		require.Contains(t, page, promiseHTML(isolationPromises[state]),
			"the %s option must carry its promise line", state)
	}
	require.NotContains(t, page, "iso-pill", "an open project gets no lock pill")
}

// promiseHTML is the promise sentence as html/template escapes it into a page.
func promiseHTML(promise string) string {
	var buf bytes.Buffer
	template.HTMLEscape(&buf, []byte(promise))
	return buf.String()
}

func TestIsolationPill_OneHelperForBothSurfaces(t *testing.T) {
	require.Empty(t, string(isolationPill(core.IsolationOpen)), "open is the absence of a pill")

	confidential := string(isolationPill(core.IsolationConfidential))
	require.Contains(t, confidential, `class="kind accent iso-pill"`)
	require.Contains(t, confidential, `<svg class="ico ico-lock"`)
	require.Contains(t, confidential, "nothing leaves")
	require.NotContains(t, confidential, "<script")

	sealed := string(isolationPill(core.IsolationSealed))
	require.Contains(t, sealed, `class="kind warn iso-pill"`)
	require.Contains(t, sealed, "nothing in or out")
}

func TestProjectsBoard_BadgesIsolatedRows(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	seedIsolationProject(t, db, "acme")
	seedIsolationProject(t, db, "plain")
	require.NoError(t, store.SetProjectIsolation(ctx, db, "acme", core.IsolationSealed))

	page := getHTMLBody(t, mux, "/console/projects?group=flat")
	require.Contains(t, page, `class="kind warn iso-pill"`)
	require.Equal(t, 1, strings.Count(page, "iso-pill"), "only the fenced row is badged")
}

func TestIsolationConfirmPanel_ProvenanceLine(t *testing.T) {
	svc, err := New(Config{APIKey: testKey})
	require.NoError(t, err)

	vm := newIsolationConfirmVM(store.IsolationTightenEffects{
		Project: "acme", From: core.IsolationOpen, To: core.IsolationConfidential,
	}, isolationProvenanceVM{Known: true, Count: 4})

	var buf bytes.Buffer
	require.NoError(t, svc.pages["projectdetail"].ExecuteTemplate(&buf, "isolation-confirm", vm))
	panel := buf.String()
	require.Contains(t, panel, "4 global memories originated in this project&rsquo;s sessions, "+
		"so relocation proposals will be filed in the "+
		`<a href="/console/gardener">Gardener inbox</a>.`)

	// A PROVEN zero is a finding worth stating -- but never with the filing
	// promise, which would name proposals that will not exist.
	buf.Reset()
	vm = newIsolationConfirmVM(store.IsolationTightenEffects{
		Project: "acme", From: core.IsolationOpen, To: core.IsolationConfidential,
	}, isolationProvenanceVM{Known: true})
	require.NoError(t, svc.pages["projectdetail"].ExecuteTemplate(&buf, "isolation-confirm", vm))
	require.Contains(t, buf.String(), "No global memories originated in this project&rsquo;s sessions")
	require.NotContains(t, buf.String(), "will be filed")

	// A count that could not be PROVED (here: no database to ask) omits the line
	// entirely rather than claiming either answer.
	require.Equal(t, isolationProvenanceVM{},
		svc.isolationGlobalProvenance(context.Background(), "acme"))
	buf.Reset()
	vm = newIsolationConfirmVM(store.IsolationTightenEffects{
		Project: "acme", From: core.IsolationOpen, To: core.IsolationConfidential,
	}, svc.isolationGlobalProvenance(context.Background(), "acme"))
	require.NoError(t, svc.pages["projectdetail"].ExecuteTemplate(&buf, "isolation-confirm", vm))
	require.NotContains(t, buf.String(), "originated in this project")
	require.Contains(t, buf.String(), "Make acme confidential?", "the rest of the panel still renders")
}

// TestProjectIsolation_TightenFilesRelocationProposals is the console half of
// the provenance audit: the confirm panel counts the knowledge that already left
// the project, the confirmed tighten files one gardener proposal per memory, and
// the inbox renders each as its own decision. Nothing moves until the owner
// applies one.
func TestProjectIsolation_TightenFilesRelocationProposals(t *testing.T) {
	ctx, db, mgr, mux := newConsoleWithGardener(t)
	now := time.Date(2026, 8, 1, 10, 0, 0, 0, time.UTC)

	_, err := store.EnsureProject(ctx, db, "acme", "acme")
	require.NoError(t, err)
	require.NoError(t, store.CreateSession(ctx, db, core.Session{
		ID: "01JACMESESSION0000000000", Name: "cc/acme1", ProjectSlug: "acme",
		Status: core.SessionActive, CreatedAt: now, UpdatedAt: now,
	}))
	mem, err := mgr.WriteMemory(ctx, core.Memory{
		ID: "01JLEAKEDMEMORY000000000", Kind: core.KindGotcha, Name: "acme-billing-quirk",
		Description: "the quirk", Project: "", Body: "body", SourceSession: "cc/acme1",
		Created: now, Updated: now, ValidFrom: now,
	})
	require.NoError(t, err)

	// The confirm panel states the count it can prove, singular and specific.
	page := getHTMLBody(t, mux, "/console/projects/acme?tab=overview&isolate=confidential")
	require.Contains(t, page, "1 global memory originated in this project&rsquo;s sessions")
	require.Contains(t, page, `<a href="/console/gardener">Gardener inbox</a>`)

	rr := postIsolation(mux, "acme", url.Values{"state": {"confidential"}, "confirm": {"1"}})
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Contains(t, rr.Header().Get("Location"),
		url.QueryEscape("Filed 1 relocation proposal in the Gardener inbox."))

	pending, err := store.PendingProposals(ctx, db, store.ProposalRelocate)
	require.NoError(t, err)
	require.Len(t, pending, 1)
	require.Equal(t, mem.ID, payloadStr(pending[0].Payload, "id"))

	// The tighten itself moved nothing -- the fence is up, the memory is not.
	idx, found, err := store.MemoryByID(ctx, db, mem.ID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "", idx.Project)

	// The rail shows it as its own decision, and the reader carries the evidence.
	queue := getHTMLBody(t, mux, "/console/gardener")
	require.Contains(t, queue, "Relocate leaked memory")
	require.Contains(t, queue, "acme-billing-quirk: global → acme")

	reader := getHTMLBody(t, mux, "/console/gardener/"+pending[0].ID)
	require.Contains(t, reader, "written by a session bound to acme, which is now confidential")
	require.Contains(t, reader, "Traced by provenance")
	require.Contains(t, reader, "cc/acme1")
	require.Contains(t, reader, "can be undone from Recently decided",
		"a relocate is a move, so the gate promises the undo rather than a confirm")
	require.NotContains(t, reader, "cannot be undone from the console")

	// Applying it is what moves the memory behind the fence.
	rr = post(mux, "/console/gardener/"+pending[0].ID+"/apply")
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Contains(t, rr.Header().Get("Location"), "notice=")
	idx, found, err = store.MemoryByID(ctx, db, mem.ID)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, "acme", idx.Project)

	// And the decision lands in Recently decided with a working undo.
	rr = post(mux, "/console/gardener/"+pending[0].ID+"/undo")
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Contains(t, rr.Header().Get("Location"), "notice=")
	idx, _, err = store.MemoryByID(ctx, db, mem.ID)
	require.NoError(t, err)
	require.Equal(t, "", idx.Project)
}

// A loosen never audits: nothing leaked while the fence was up, and there is no
// fence left to pull knowledge behind.
func TestProjectIsolation_LoosenFilesNoProposals(t *testing.T) {
	ctx, db, _, mux := newConsoleWithGardener(t)
	_, err := store.EnsureProject(ctx, db, "acme", "acme")
	require.NoError(t, err)
	require.NoError(t, store.SetProjectIsolation(ctx, db, "acme", core.IsolationSealed))

	rr := postIsolation(mux, "acme", url.Values{"state": {"open"}})
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.NotContains(t, rr.Header().Get("Location"), "relocation")

	pending, err := store.PendingProposals(ctx, db, store.ProposalRelocate)
	require.NoError(t, err)
	require.Empty(t, pending)
}

func TestSettingsFamilySave_RefusesIsolatedMembers(t *testing.T) {
	db, mux := newConsole(t)
	ctx := context.Background()
	for _, slug := range []string{"acme", "sibling"} {
		seedIsolationProject(t, db, slug)
	}
	require.NoError(t, store.SetProjectIsolation(ctx, db, "acme", core.IsolationConfidential))

	rr := postForm(mux, "/console/settings/families/save", url.Values{
		"name":    {"product"},
		"members": {"acme", "sibling"},
	}.Encode())
	require.Equal(t, http.StatusSeeOther, rr.Code)
	location := rr.Header().Get("Location")
	require.Contains(t, location, "error=")
	require.Contains(t, location, url.QueryEscape("acme (confidential)"))
	families, err := store.ProjectFamilies(ctx, db)
	require.NoError(t, err)
	require.Empty(t, families, "the console must not build a family around a fenced project")

	// Open members still save, and removing the fenced one from an existing
	// family is never blocked -- the guard is about joining, not leaving.
	rr = postForm(mux, "/console/settings/families/save", url.Values{
		"name":    {"product"},
		"members": {"sibling"},
	}.Encode())
	require.Equal(t, http.StatusSeeOther, rr.Code)
	require.Contains(t, rr.Header().Get("Location"), "notice=")
	families, err = store.ProjectFamilies(ctx, db)
	require.NoError(t, err)
	require.Equal(t, map[string][]string{"product": {"sibling"}}, families)
}

// A proposal whose target was fenced AFTER it was proposed is the stale-evidence
// case: the gardener's proposal-time fences (step 5) stop new ones from spanning
// an isolated project, but a proposal already in the queue when the owner
// tightened still names it. The MCP surface refuses such a proposal outright.
// The console must NOT -- the owner is exempt by design -- so it marks it
// instead, which is what makes applying it an informed choice rather than a
// silent move across a fence raised after the evidence was gathered.
func TestGardenerInbox_MarksProposalsWhoseTargetIsNowFenced(t *testing.T) {
	ctx, db, _, mux := newConsoleWithGardener(t)
	seedIsolationProject(t, db, "vault")
	seedIsolationProject(t, db, "acme")

	fenced, err := store.CreateProposal(ctx, db, store.ProposalDigest, map[string]any{
		"key": "digest:vault:2026-07", "project": "vault", "month": "2026-07",
		"session_count": 3.0, "title": "vault digest 2026-07", "body": "## Findings\n- shipped it",
	})
	require.NoError(t, err)
	open, err := store.CreateProposal(ctx, db, store.ProposalDigest, map[string]any{
		"key": "digest:acme:2026-07", "project": "acme", "month": "2026-07",
		"session_count": 2.0, "title": "acme digest 2026-07", "body": "## Findings\n- shipped it too",
	})
	require.NoError(t, err)

	// The fence goes up after both proposals exist -- that ordering is the point.
	require.NoError(t, store.SetProjectIsolation(ctx, db, "vault", core.IsolationSealed))

	// The owner still sees BOTH: the console is exempt, so the fence marks and
	// never hides. A regression that filtered here would be worse than the leak
	// this whole plan closes.
	queue := getHTMLBody(t, mux, "/console/gardener")
	require.Contains(t, queue, "vault digest 2026-07")
	require.Contains(t, queue, "acme digest 2026-07")
	require.Contains(t, queue, `class="gardener-fenced-chip"`)
	require.Contains(t, queue, "Touches vault (sealed)")

	// The mark lands on the fenced card only.
	fencedReader := getHTMLBody(t, mux, "/console/gardener/"+fenced.ID+"?reader=1")
	require.Contains(t, fencedReader, `class="gardener-fenced-note"`)
	require.Contains(t, fencedReader, "read what it changes before applying")
	require.Contains(t, fencedReader, `href="/console/projects/vault"`)
	require.Contains(t, fencedReader, "(sealed)")

	openReader := getHTMLBody(t, mux, "/console/gardener/"+open.ID+"?reader=1")
	require.NotContains(t, openReader, `class="gardener-fenced-note"`)

	// Loosening clears the mark: nothing leaked while the fence was up, so the
	// proposal goes back to being ordinary.
	require.NoError(t, store.SetProjectIsolation(ctx, db, "vault", core.IsolationOpen))
	require.NotContains(t, getHTMLBody(t, mux, "/console/gardener/"+fenced.ID+"?reader=1"),
		`class="gardener-fenced-note"`)
}

// A relocate proposal is the tighten's own repair -- it pulls a leaked global
// memory back INSIDE the fence -- so it is created by the tighten and its
// evidence can never predate it. Marking it would be both wrong-directional and
// the majority of the queue right after a tighten (one row per leaked memory),
// and a warning on every row is a warning on none.
func TestGardenerInbox_RelocateProposalsAreNotMarkedFenced(t *testing.T) {
	ctx, db, _, mux := newConsoleWithGardener(t)
	seedIsolationProject(t, db, "vault")
	require.NoError(t, store.SetProjectIsolation(ctx, db, "vault", core.IsolationConfidential))

	rel, err := store.CreateProposal(ctx, db, store.ProposalRelocate, map[string]any{
		"key": "relocate:LEAKED", "id": "LEAKEDMEM", "name": "leaked-fact",
		"kind": "reference", "description": "written by a vault session while global",
		"from": "", "to": "vault",
	})
	require.NoError(t, err)

	reader := getHTMLBody(t, mux, "/console/gardener/"+rel.ID+"?reader=1")
	require.NotContains(t, reader, `class="gardener-fenced-note"`)
	// The card still explains the fence relationship in its own assurance line --
	// that is where a relocate's isolation context belongs.
	require.Contains(t, reader, "moving it puts it behind the fence")
}
