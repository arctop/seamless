package console

import (
	"context"
	"database/sql"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"os"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/retrieve"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
	"github.com/arctop/seamless/internal/transcripts"
)

// newFlagConsole is a console keeping transcript copies, over a fresh DB.
func newFlagConsole(t *testing.T) (*sql.DB, *http.ServeMux, *transcripts.Service) {
	t.Helper()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	rec := events.NewRecorder(db)
	svc, err := transcripts.New(transcripts.Config{DataDir: dir, DB: db, Events: rec, LocalHost: "alpha"})
	require.NoError(t, err)
	c, err := New(Config{DB: db, Events: rec, APIKey: testKey, Retrieve: retrieve.New(db, nil, config.Defaults().Budgets, nil),
		Level: "advanced", Transcripts: svc})
	require.NoError(t, err)
	mux := http.NewServeMux()
	c.Register(mux)
	return db, mux, svc
}

// flaggedSession creates a completed session, flags it with verdict, and (when
// content is not "") keeps a copy of a transcript holding content.
func flaggedSession(t *testing.T, db *sql.DB, svc *transcripts.Service, name string, verdict core.SessionVerdict, content string) core.Session {
	t.Helper()
	ctx := context.Background()
	id, err := core.NewID()
	require.NoError(t, err)
	now := time.Now().UTC()
	sess := core.Session{ID: id, Name: name, ProjectSlug: "demo", Status: core.SessionCompleted, Ambient: true,
		ExternalClient: "claude-code", ExternalSessionID: name, Source: "startup", CreatedAt: now, UpdatedAt: now}
	require.NoError(t, store.CreateSession(ctx, db, sess))
	path := filepath.Join(t.TempDir(), "t.jsonl")
	require.NoError(t, os.WriteFile(path, []byte(content), 0o600))
	require.NoError(t, store.SetSessionTranscriptPath(ctx, db, id, path))
	if verdict == "" {
		return sess
	}
	_, err = svc.Flag(ctx, sess, verdict, "because "+name, "agent", true)
	require.NoError(t, err)
	if content != "" {
		svc.Snapshot(ctx, id, path, true)
		svc.Wait()
	}
	sess, _, err = store.SessionByID(ctx, db, id)
	require.NoError(t, err)
	return sess
}

func getPage(t *testing.T, mux *http.ServeMux, path string) string {
	t.Helper()
	req := httptest.NewRequest(http.MethodGet, path, nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := do(mux, req)
	require.Equal(t, http.StatusOK, rr.Code, "GET %s: %s", path, rr.Body.String())
	return rr.Body.String()
}

// The Sessions list: a badge with the verdict and reason on a flagged row, the
// flag filter offered only once a session is flagged, and the filter itself.
func TestSessionsList_FlagBadgeAndFilter(t *testing.T) {
	db, mux, svc := newFlagConsole(t)
	plain := flaggedSession(t, db, svc, "cc/plain", "", "")

	page := getPage(t, mux, "/console/sessions")
	require.NotContains(t, page, `<span class="mv2-sort-label">Flag</span>`, "no flag filter until a session is flagged")
	require.NotContains(t, page, "flag-badge")

	good := flaggedSession(t, db, svc, "cc/good", core.VerdictGood, `{"a":1}`+"\n")
	bad := flaggedSession(t, db, svc, "cc/bad", core.VerdictBad, "")
	page = getPage(t, mux, "/console/sessions")
	require.Contains(t, page, `<span class="mv2-sort-label">Flag</span>`)
	require.Contains(t, page, `data-verdict="good"`)
	require.Contains(t, page, `data-verdict="bad"`)
	require.Contains(t, page, "because cc/good", "the reason shows on the row")

	for _, tc := range []struct {
		verdict string
		want    []string
	}{
		{"", []string{plain.ID, good.ID, bad.ID}},
		{"flagged", []string{good.ID, bad.ID}},
		{"good", []string{good.ID}},
		{"bad", []string{bad.ID}},
		{"notable", nil},
	} {
		var list sessionsData
		getJSON(t, mux, "/console/sessions?format=json&verdict="+tc.verdict, &list)
		var ids []string
		for _, r := range list.Sessions {
			ids = append(ids, r.ID)
		}
		require.ElementsMatch(t, tc.want, ids, "verdict=%q", tc.verdict)
		require.Equal(t, 2, list.Flagged)
	}
	var list sessionsData
	getJSON(t, mux, "/console/sessions?format=json&verdict=good", &list)
	require.Equal(t, "because cc/good", list.Sessions[0].Reason)
	require.True(t, list.Sessions[0].Kept)
	require.NotEmpty(t, list.Sessions[0].TranscriptPath)

	page = getPage(t, mux, "/console/sessions?verdict=bad&status=completed")
	require.Contains(t, page, `name="verdict" value="bad"`, "the search keeps the filter")
	require.Contains(t, page, "&amp;verdict=bad", "the other filters' links keep it too")

	req := httptest.NewRequest(http.MethodGet, "/console/sessions?verdict=great&format=json", nil)
	req.Header.Set("Authorization", "Bearer "+testKey)
	rr := do(mux, req)
	require.Equal(t, http.StatusBadRequest, rr.Code)
	require.Contains(t, rr.Body.String(), `invalid verdict \"great\": valid values are flagged, good, bad, notable`)
}

// The session page: the flag's card with its reason, the transcript path and
// what became of the copy, and the download only when a copy is kept.
func TestSessionDetail_FlagCard(t *testing.T) {
	db, mux, svc := newFlagConsole(t)
	kept := flaggedSession(t, db, svc, "cc/kept", core.VerdictNotable, `{"b":2}`+"\n")
	none := flaggedSession(t, db, svc, "cc/none", core.VerdictBad, "")
	plain := flaggedSession(t, db, svc, "cc/plain", "", "")

	page := getPage(t, mux, "/console/sessions/"+kept.ID)
	require.Contains(t, page, "session-flag-card")
	require.Contains(t, page, "because cc/kept")
	require.Contains(t, page, kept.TranscriptPath)
	require.Contains(t, page, `href="/console/sessions/`+kept.ID+`/transcript" download`)

	page = getPage(t, mux, "/console/sessions/"+none.ID)
	require.Contains(t, page, "session-flag-card")
	require.NotContains(t, page, "/transcript\" download", "no copy, no download")
	require.Contains(t, page, "no copy yet")

	require.NotContains(t, getPage(t, mux, "/console/sessions/"+plain.ID), "session-flag-card")

	var detail sessionDetail
	getJSON(t, mux, "/console/sessions/"+kept.ID+"?format=json", &detail)
	require.NotNil(t, detail.Flag)
	require.Equal(t, "notable", detail.Flag.Verdict)
	require.True(t, detail.Flag.Kept)
	require.Equal(t, "copied", detail.Flag.Copy)
	var plainDetail sessionDetail
	getJSON(t, mux, "/console/sessions/"+plain.ID+"?format=json", &plainDetail)
	require.Nil(t, plainDetail.Flag)
}

// The copy is the owner's: served whole to the key, refused to a restricted
// account and to a request a relay forwarded; an unflagged or copy-less
// session answers 404.
func TestSessionTranscript_OwnerOnly(t *testing.T) {
	db, mux, svc := newFlagConsole(t)
	content := `{"type":"user","message":{"content":"transcript-secret-token"}}` + "\n" + `{"type":"assistant"}` + "\n"
	kept := flaggedSession(t, db, svc, "cc/kept", core.VerdictGood, content)
	none := flaggedSession(t, db, svc, "cc/none", core.VerdictGood, "")
	plain := flaggedSession(t, db, svc, "cc/plain", "", "")

	get := func(id string, ctx context.Context) *httptest.ResponseRecorder {
		req := httptest.NewRequest(http.MethodGet, "/console/sessions/"+id+"/transcript", nil).WithContext(ctx)
		req.Header.Set("Authorization", "Bearer "+testKey)
		req.Header.Set("Accept", "application/json")
		return do(mux, req)
	}
	rr := get(kept.ID, context.Background())
	require.Equal(t, http.StatusOK, rr.Code, rr.Body.String())
	body, err := io.ReadAll(rr.Result().Body)
	require.NoError(t, err)
	require.Equal(t, content, string(body))
	require.Equal(t, "application/x-ndjson", rr.Header().Get("Content-Type"))
	require.Equal(t, "no-store", rr.Header().Get("Cache-Control"))
	require.Empty(t, rr.Result().Trailer.Get(TranscriptErrorTrailer))

	for _, id := range []string{none.ID, plain.ID, "01MNOSUCHAAAAAAAAAAAAAAAAA"} {
		require.Equal(t, http.StatusNotFound, get(id, context.Background()).Code, id)
	}

	policy, err := access.New(config.PeerAuth{Owner: "owner", Restricted: config.BareRestricted("_example_agent"),
		Relays: map[string]config.PeerRelay{"laptop": {OwnerUID: 501}}},
		func(n string) (access.Account, error) {
			if n == "owner" {
				return access.Account{UID: 501, Name: n}, nil
			}
			return access.Account{UID: 470, Name: n}, nil
		})
	require.NoError(t, err)
	restricted, ok := policy.PeerView(470)
	require.True(t, ok)
	relayed, ok := policy.RelayChainViewNamed("01MRELAYAAAAAAAAAAAAAAAAAA", "laptop", 501, nil, nil)
	require.True(t, ok)
	owner, ok := policy.PeerView(501)
	require.True(t, ok)
	for name, v := range map[string]access.View{"restricted account": restricted, "relayed caller": relayed} {
		rr := get(kept.ID, access.WithView(context.Background(), v))
		require.Equal(t, http.StatusForbidden, rr.Code, "%s: %s", name, rr.Body.String())
		require.NotContains(t, rr.Body.String(), "transcript-secret-token", name)
	}
	rr = get(kept.ID, access.WithView(context.Background(), owner))
	require.Equal(t, http.StatusOK, rr.Code, "the owner's own socket account reads it: %s", rr.Body.String())
	require.Equal(t, content, rr.Body.String())

	// A damaged copy is said in the trailer, never passed off as whole.
	path := filepath.Join(svc.Dir(), kept.ID+transcripts.Suffix)
	raw, err := os.ReadFile(path)
	require.NoError(t, err)
	require.NoError(t, os.WriteFile(path, raw[:len(raw)-4], 0o600))
	rr = get(kept.ID, context.Background())
	require.Equal(t, http.StatusOK, rr.Code)
	require.NotEmpty(t, rr.Result().Trailer.Get(TranscriptErrorTrailer), fmt.Sprintf("body: %q", rr.Body.String()))
	require.True(t, strings.HasPrefix(content, rr.Body.String()))
}
