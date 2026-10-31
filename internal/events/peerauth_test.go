package events

import (
	"context"
	"fmt"
	"path/filepath"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

func peerPolicy(t *testing.T) *access.Policy {
	t.Helper()
	accounts := map[string]access.Account{
		"katata":          {UID: 501, Name: "katata"},
		"_arctop_finance": {UID: 470, Name: "_arctop_finance"},
	}
	p, err := access.New(config.PeerAuth{Owner: "katata", Users: map[string]config.PeerUser{
		"_arctop_finance": {Sealed: []string{"arctop-finance"}},
	}}, func(name string) (access.Account, error) {
		if a, ok := accounts[name]; ok {
			return a, nil
		}
		return access.Account{}, fmt.Errorf("no %s", name)
	})
	require.NoError(t, err)
	return p
}

// A configured account's event never lands outside its own projects: an
// unattributed one (the global scope) or one naming another project is filed
// under its home project. The owner's events, and events for no caller, keep
// the project they were given.
func TestRecord_RefilesAConfiguredAccountsEvents(t *testing.T) {
	r := newRecorder(t)
	p := peerPolicy(t)
	fin, ok := p.PeerView(470)
	require.True(t, ok)

	for _, tc := range []struct {
		name    string
		ctx     context.Context
		project string
		want    string
	}{
		{"an account's unattributed event", access.WithView(context.Background(), fin), "", "arctop-finance"},
		{"an account's event naming another project", access.WithView(context.Background(), fin), "arctop-ops", "arctop-finance"},
		{"an account's event in its own project", access.WithView(context.Background(), fin), "arctop-finance", "arctop-finance"},
		{"the owner's global event", access.WithView(context.Background(), p.OwnerView()), "", ""},
		{"an event for no caller", context.Background(), "", ""},
	} {
		t.Run(tc.name, func(t *testing.T) {
			id, err := r.Record(tc.ctx, core.Event{Kind: core.EventToolCall, ProjectSlug: tc.project, Payload: map[string]any{"tool": "recall"}})
			require.NoError(t, err)
			e, found, err := r.ByID(context.Background(), id)
			require.NoError(t, err)
			require.True(t, found)
			require.Equal(t, tc.want, e.ProjectSlug)

			onceID, recorded, err := r.RecordOnce(tc.ctx, core.Event{Kind: core.EventMemoryFirstReuse, ItemID: "item-" + id, ProjectSlug: tc.project})
			require.NoError(t, err)
			require.True(t, recorded)
			e, _, err = r.ByID(context.Background(), onceID)
			require.NoError(t, err)
			require.Equal(t, tc.want, e.ProjectSlug, "RecordOnce files the same way")
		})
	}
}

// ReadingFrom answers reads from the filtered handle and shares everything else:
// an event recorded through either reaches both's subscribers, and the reads
// see only what the filtered handle shows.
func TestReadingFrom_ReadsThroughTheViewAndSharesTheHub(t *testing.T) {
	dir := t.TempDir()
	dbPath := filepath.Join(dir, "seam.db")
	db, err := storetest.OpenFresh(t, dbPath)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	view, err := store.OpenView(dbPath, []string{"arctop-finance"})
	require.NoError(t, err)
	t.Cleanup(func() { _ = view.Close() })

	main := NewRecorder(db)
	reading := main.ReadingFrom(view)
	ch, unsubscribe := reading.Subscribe()
	defer unsubscribe()

	ctx := context.Background()
	hidden, err := main.Record(ctx, core.Event{Kind: core.EventToolCall, ProjectSlug: "arctop-finance"})
	require.NoError(t, err)
	shown, err := reading.Record(ctx, core.Event{Kind: core.EventToolCall, ProjectSlug: "arctop-ops"})
	require.NoError(t, err)

	got := []string{(<-ch).ID, (<-ch).ID}
	require.Equal(t, []string{hidden, shown}, got, "both recorders publish to one hub")

	_, found, err := reading.ByID(ctx, hidden)
	require.NoError(t, err)
	require.False(t, found, "a read through the view does not see the hidden project")
	_, found, err = reading.ByID(ctx, shown)
	require.NoError(t, err)
	require.True(t, found)
	_, found, err = main.ByID(ctx, hidden)
	require.NoError(t, err)
	require.True(t, found, "the main recorder still reads everything")
	recent, err := reading.RecentExcluding(ctx, 10)
	require.NoError(t, err)
	require.Len(t, recent, 1)
}
