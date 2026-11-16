package approval

import (
	"context"
	"fmt"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// lockEvents lists the console lock events recorded so far, oldest first, as
// "kind project item payload".
func lockEvents(t *testing.T, f *fixture) []string {
	t.Helper()
	rows, err := f.db.QueryContext(context.Background(),
		`SELECT kind, project_slug, item_id, payload FROM events WHERE kind LIKE 'access.console_lock_%' ORDER BY rowid`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	var out []string
	for rows.Next() {
		var kind, project, item, payload string
		require.NoError(t, rows.Scan(&kind, &project, &item, &payload))
		out = append(out, strings.Join([]string{kind, project, item, payload}, " "))
	}
	require.NoError(t, rows.Err())
	return out
}

// A ceremony that began and then failed is recorded under its own project
// and direction, with why; a finish naming no ceremony records nothing, since
// it has nothing but the caller's text. A change made is recorded as the
// console's.
func TestLockChange_FailuresAreRecorded(t *testing.T) {
	f, key := lockFixture(t)
	ctx := context.Background()
	cred := f.enrollWithCode(t, key, "key")
	begin := func() (Begun, []byte) {
		t.Helper()
		b, err := f.svc.BeginLockChange(ctx, "example-conf", LockActionAdd)
		require.NoError(t, err)
		resp, err := key.Get(b.Options, origin)
		require.NoError(t, err)
		return b, resp
	}

	b, resp := begin()
	_, err := f.svc.FinishLockChange(ctx, b.ID, "example-conf", LockActionRemove, resp)
	require.ErrorIs(t, err, ErrNoLockChange, "the other direction")
	b, resp = begin()
	_, err = f.svc.FinishLockChange(ctx, b.ID, "example-open", LockActionAdd, resp)
	require.ErrorIs(t, err, ErrNoLockChange, "another project")
	_, err = f.svc.FinishLockChange(ctx, "no-such-id", "example-named-by-caller", LockActionAdd, resp)
	require.ErrorIs(t, err, ErrNoLockChange, "no ceremony")
	b, _ = begin()
	_, err = f.svc.FinishLockChange(ctx, b.ID, "example-conf", LockActionAdd, []byte(`{"id":"x"}`))
	require.Error(t, err, "garbage is not an assertion")
	b, resp = begin()
	f.now = f.now.Add(lockChangeTTL + time.Second)
	_, err = f.svc.FinishLockChange(ctx, b.ID, "example-conf", LockActionAdd, resp)
	require.ErrorIs(t, err, ErrNoLockChange, "an expired ceremony is no ceremony")

	// A counter that does not move on: the stored one is ahead of the key's.
	_, err = f.db.ExecContext(ctx, `UPDATE approver_credentials SET sign_count = 1000 WHERE id = ?`, cred.ID)
	require.NoError(t, err)
	b, resp = begin()
	_, err = f.svc.FinishLockChange(ctx, b.ID, "example-conf", LockActionAdd, resp)
	require.Error(t, err, "a counter that did not increase")
	_, err = f.db.ExecContext(ctx, `UPDATE approver_credentials SET sign_count = 0 WHERE id = ?`, cred.ID)
	require.NoError(t, err)

	b, resp = begin()
	_, err = f.svc.FinishLockChange(ctx, b.ID, "example-conf", LockActionAdd, resp)
	require.NoError(t, err)

	require.Equal(t, []string{
		`access.console_lock_failed example-conf  {"action":"lock","reason":"mismatch"}`,
		`access.console_lock_failed example-conf  {"action":"lock","reason":"mismatch"}`,
		`access.console_lock_failed example-conf  {"action":"lock","reason":"assertion"}`,
		`access.console_lock_failed example-conf  {"action":"lock","reason":"counter"}`,
		`access.console_lock_changed example-conf ` + cred.ID + ` {"action":"lock","by":"console","proof":"passkey"}`,
	}, lockEvents(t, f), "the ceremony's own project, never the one a finish names")
}

// A change the project no longer allows by the time its passkey answers is a
// failure of its own, under the passkey that answered.
func TestLockChange_StateChangedSinceBegin(t *testing.T) {
	f, key := lockFixture(t)
	ctx := context.Background()
	cred := f.enrollWithCode(t, key, "key")
	b, err := f.svc.BeginLockChange(ctx, "example-conf", LockActionAdd)
	require.NoError(t, err)
	resp, err := key.Get(b.Options, origin)
	require.NoError(t, err)
	require.NoError(t, store.SetProjectIsolation(ctx, f.db, "example-conf", core.IsolationOpen))
	_, err = f.svc.FinishLockChange(ctx, b.ID, "example-conf", LockActionAdd, resp)
	require.ErrorIs(t, err, store.ErrConsoleLockUnfenced)
	require.Equal(t, []string{
		`access.console_lock_failed example-conf ` + cred.ID + ` {"action":"lock","reason":"state"}`,
	}, lockEvents(t, f))
}

// A begin never waits for room. Past a project and direction's cap its own
// oldest goes; past the overall cap the oldest of the fullest project and
// direction goes. A loop on other projects never reaches the owner's
// ceremony, and the owner's finishes.
func TestLockChange_PendingCaps(t *testing.T) {
	f, key := lockFixture(t)
	ctx := context.Background()
	f.enrollWithCode(t, key, "key")
	var loop []string
	for i := range maxPending / maxPendingPerProject {
		slug := fmt.Sprintf("example-loop-%d", i)
		loop = append(loop, slug)
		_, err := store.EnsureProject(ctx, f.db, slug, slug)
		require.NoError(t, err)
		require.NoError(t, store.SetProjectIsolation(ctx, f.db, slug, core.IsolationConfidential))
	}
	begin := func(project string) Begun {
		t.Helper()
		f.now = f.now.Add(time.Second)
		b, err := f.svc.BeginLockChange(ctx, project, LockActionAdd)
		require.NoError(t, err, "a begin never waits for room")
		return b
	}
	waiting := func(project string) int {
		f.svc.mu.Lock()
		defer f.svc.mu.Unlock()
		n := 0
		for _, p := range f.svc.lockChanges {
			if project == "" || p.project == project {
				n++
			}
		}
		return n
	}
	held := func(id string) bool {
		f.svc.mu.Lock()
		defer f.svc.mu.Unlock()
		_, ok := f.svc.lockChanges[id]
		return ok
	}

	// One project and direction: the fifth begin displaces the first.
	first := begin(loop[0])
	for range maxPendingPerProject {
		begin(loop[0])
	}
	require.False(t, held(first.ID))
	require.Equal(t, maxPendingPerProject, waiting(loop[0]))

	// Every slot taken across projects: the owner's begin fits, and the
	// fullest project with the oldest ceremony gives one up.
	for _, slug := range loop[1:] {
		for range maxPendingPerProject {
			begin(slug)
		}
	}
	require.Equal(t, maxPending, waiting(""))
	owners := begin("example-conf")
	require.Equal(t, maxPending, waiting(""), "the overall cap holds")
	require.Equal(t, maxPendingPerProject-1, waiting(loop[0]))

	// The loop goes on across its own projects; the owner's ceremony stays.
	for range 3 {
		for _, slug := range loop {
			begin(slug)
		}
	}
	require.True(t, held(owners.ID), "a loop on other projects never displaces the owner's ceremony")
	require.Equal(t, maxPending, waiting(""))
	resp, err := key.Get(owners.Options, origin)
	require.NoError(t, err)
	_, err = f.svc.FinishLockChange(ctx, owners.ID, "example-conf", LockActionAdd, resp)
	require.NoError(t, err)
}
