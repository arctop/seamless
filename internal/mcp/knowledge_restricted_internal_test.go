package mcp

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/capture"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/files"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

// kaViews returns a restricted account's request context (reaching the open
// project) and the owner's, over one peer_auth policy.
func kaViews(t *testing.T) (agent, owner context.Context) {
	t.Helper()
	policy, err := access.New(config.PeerAuth{Enabled: true, Owner: "owner", Restricted: []string{"_example_agent"}},
		func(name string) (access.Account, error) {
			switch name {
			case "owner":
				return access.Account{UID: 501, Name: "owner"}, nil
			case "_example_agent":
				return access.Account{UID: 502, Name: "_example_agent"}, nil
			}
			return access.Account{}, fmt.Errorf("unknown account %q", name)
		})
	require.NoError(t, err)
	v, ok := policy.PeerView(502)
	require.True(t, ok)
	ctx := context.Background()
	return access.WithView(ctx, v.WithOpenProjects([]string{"example-open"})), access.WithView(ctx, policy.OwnerView())
}

// capture_url's note carries the restricted account's label, and the owner's
// carries none. (The tool itself sits behind the SSRF guard, which refuses
// every test server, so the note it would write is checked here.)
func TestRestricted_CapturedNoteLabeled(t *testing.T) {
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	s := New(Config{DB: db, APIKey: "test-key"})
	agent, owner := kaViews(t)
	page := &capture.URLContent{Title: "Example page", Body: "Readable text.", URL: "https://example.com/page"}

	n, err := s.capturedNote(agent, page, "example-open")
	require.NoError(t, err)
	require.Equal(t, "_example_agent", n.Account)
	require.Equal(t, "example-open", n.Project)
	require.Contains(t, n.Body, "Source: https://example.com/page")

	for _, ctx := range []context.Context{owner, context.Background()} {
		n, err = s.capturedNote(ctx, page, "example-open")
		require.NoError(t, err)
		require.Empty(t, n.Account)
	}
}

// gardener_request's scope argument meets the account's reach before any
// candidate is read: a project it may not reach (or that does not exist) is
// refused with one text, and the whole-machine token stays a token (the
// candidate scan then filters by the same reach, store.CanRead).
func TestRestricted_GardenerRequestScope(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	for _, slug := range []string{"example-open", "example-conf"} {
		_, err := store.EnsureProject(ctx, db, slug, slug)
		require.NoError(t, err)
	}
	require.NoError(t, store.SetProjectIsolation(ctx, db, "example-conf", core.IsolationConfidential))
	s := New(Config{DB: db, APIKey: "test-key"})
	agent, _ := kaViews(t)

	got, err := s.gardenerRequestScope(agent, "example-open")
	require.NoError(t, err)
	require.Equal(t, "example-open", got.Project)
	var texts []string
	for _, slug := range []string{"example-conf", "example-missing"} {
		_, err := s.gardenerRequestScope(agent, slug)
		require.Error(t, err)
		texts = append(texts, err.Error())
	}
	require.Contains(t, texts[0], "is not reachable from account _example_agent")
	require.Equal(t, texts[0], strings.ReplaceAll(texts[1], "example-missing", "example-conf"),
		"a closed project and a missing one are refused with one text")
	all, err := s.gardenerRequestScope(agent, "all")
	require.NoError(t, err)
	require.True(t, all.AllProjects)
}

// A restricted account's supersede that meets a briefing-injected target
// after its write landed -- the target changed kind, or appeared, after the
// call was judged -- is refused, never run as a plain write; the owner's is
// unchanged.
func TestRestricted_SupersedeRaceRefused(t *testing.T) {
	ctx := context.Background()
	dir := t.TempDir()
	db, err := storetest.OpenFresh(t, filepath.Join(dir, "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	mgr, err := files.NewManager(dir, db, nil)
	require.NoError(t, err)
	t.Cleanup(func() { _ = mgr.Close() })
	_, err = store.EnsureProject(ctx, db, "example-open", "example-open")
	require.NoError(t, err)
	s := New(Config{DB: db, Files: mgr, APIKey: "test-key"})
	agent, owner := kaViews(t)

	now := time.Now().UTC()
	write := func(name string, kind core.MemoryKind) core.Memory {
		id, err := core.NewID()
		require.NoError(t, err)
		m, err := mgr.WriteMemory(ctx, core.Memory{ID: id, Kind: kind, Name: name, Project: "example-open",
			Description: name, Body: name + "\n", Created: now, Updated: now, ValidFrom: now})
		require.NoError(t, err)
		return m
	}
	write("owner-rule", core.KindConstraint)
	replacement := write("agent-note", core.KindGotcha)

	_, err = s.supersedeMemory(agent, "example-open", "owner-rule", replacement, now)
	require.ErrorContains(t, err, "is a proposal")
	still, ok, err := store.MemoryByName(ctx, db, "example-open", "owner-rule")
	require.NoError(t, err)
	require.True(t, ok, "the constraint stays active")
	require.True(t, still.Active())

	ref, err := s.supersedeMemory(owner, "example-open", "owner-rule", replacement, now)
	require.NoError(t, err)
	require.Equal(t, "example-open/owner-rule", ref)
}
