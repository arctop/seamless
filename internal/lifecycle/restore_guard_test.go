package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// lrLookup is a MemoryLookup over a fixed set of memories, counting its calls.
type lrLookup struct {
	mems  map[string]core.Memory
	calls int
	err   error
}

func (l *lrLookup) find(_ context.Context, id string) (core.Memory, bool, error) {
	l.calls++
	if l.err != nil {
		return core.Memory{}, false, l.err
	}
	m, ok := l.mems[id]
	return m, ok, nil
}

func lrMems(ms ...core.Memory) *lrLookup {
	l := &lrLookup{mems: map[string]core.Memory{}}
	for _, m := range ms {
		l.mems[m.ID] = m
	}
	return l
}

// LiveSuccessor follows the chain to its end: the active memory there, or
// none when the chain ends archived or dangling. A loop is an error, never a
// "no successor" a guard would wave through.
func TestLrLiveSuccessor(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	gone := func(id, by string) core.Memory {
		return core.Memory{ID: id, Name: "m-" + id, InvalidAt: &at, SupersededBy: by}
	}
	live := core.Memory{ID: "C", Name: "m-C", Project: "example-ops"}

	for name, tc := range map[string]struct {
		start     core.Memory
		set       *lrLookup
		wantID    string
		wantFound bool
		wantErr   string
	}{
		"the direct successor is active":  {start: gone("A", "C"), set: lrMems(live), wantID: "C", wantFound: true},
		"an active memory two links on":   {start: gone("A", "B"), set: lrMems(gone("B", "C"), live), wantID: "C", wantFound: true},
		"the chain ends archived":         {start: gone("A", "B"), set: lrMems(gone("B", ""))},
		"the successor no longer exists":  {start: gone("A", "B"), set: lrMems()},
		"an archived memory has no chain": {start: gone("A", ""), set: lrMems(live)},
		"a loop is an error":              {start: gone("A", "B"), set: lrMems(gone("B", "A")), wantErr: "loops at A"},
		"a self edge is an error":         {start: gone("A", "A"), set: lrMems(), wantErr: "loops at A"},
	} {
		t.Run(name, func(t *testing.T) {
			got, found, err := LiveSuccessor(ctx, tc.set.find, tc.start)
			if tc.wantErr != "" {
				require.ErrorContains(t, err, tc.wantErr)
				require.False(t, found)
				return
			}
			require.NoError(t, err)
			require.Equal(t, tc.wantFound, found)
			require.Equal(t, tc.wantID, got.ID)
		})
	}

	t.Run("an over-long chain is an error", func(t *testing.T) {
		set := lrMems()
		for i := range maxSupersessionChain + 1 {
			set.mems[fmt.Sprint(i)] = gone(fmt.Sprint(i), fmt.Sprint(i+1))
		}
		_, found, err := LiveSuccessor(ctx, set.find, gone("start", "0"))
		require.ErrorContains(t, err, "runs past")
		require.False(t, found)
		require.Equal(t, maxSupersessionChain, set.calls, "the walk stops at the bound")
	})
	t.Run("a lookup failure is the walk's error", func(t *testing.T) {
		set := lrMems()
		set.err = errors.New("disk on fire")
		_, _, err := LiveSuccessor(ctx, set.find, gone("A", "B"))
		require.ErrorContains(t, err, "disk on fire")
	})
}

// Restore dispatches on how the memory left: an archive comes back through
// Unarchive, a supersession through Unsupersede, and a superseded memory with
// an active replacement is refused with that replacement and nothing written.
func TestLrRestore(t *testing.T) {
	ctx := context.Background()
	at := time.Date(2026, 11, 21, 12, 0, 0, 0, time.UTC)
	later := at.Add(time.Hour)
	body := "the claim\n"

	t.Run("an archived memory", func(t *testing.T) {
		w := &recordingWriter{}
		archived, err := Archive(ctx, w, core.Memory{ID: "A", Name: "old", Body: body}, "", at)
		require.NoError(t, err)
		set := lrMems()
		got, err := Restore(ctx, w, set.find, archived, later)
		require.NoError(t, err)
		require.True(t, got.Active())
		require.Equal(t, body, got.Body)
		require.Equal(t, 0, set.calls, "an archive has no chain to read")
	})

	replacement := core.Memory{ID: "B", Name: "new", Project: "example-ops"}
	superseded, err := Supersede(ctx, &recordingWriter{}, core.Memory{ID: "A", Name: "old", Project: "example-ops", Body: body}, replacement, at)
	require.NoError(t, err)

	t.Run("a superseded memory whose replacement is active", func(t *testing.T) {
		w := &recordingWriter{}
		_, err := Restore(ctx, w, lrMems(replacement).find, superseded, later)
		require.ErrorIs(t, err, ErrLiveSuccessor)
		var se *SuccessorError
		require.True(t, errors.As(err, &se))
		require.Equal(t, "B", se.Successor.ID)
		require.ErrorContains(t, err, "example-ops/new replaced it and is still active")
		require.Empty(t, w.last.ID, "nothing is written")
	})
	t.Run("a superseded memory whose replacement was archived", func(t *testing.T) {
		w := &recordingWriter{}
		retired := replacement
		retired.InvalidAt = &at
		got, err := Restore(ctx, w, lrMems(retired).find, superseded, later)
		require.NoError(t, err)
		require.True(t, got.Active())
		require.Empty(t, got.SupersededBy)
		require.Equal(t, body, got.Body, "the supersession line goes with the edge")
	})
	t.Run("a superseded memory whose replacement is gone", func(t *testing.T) {
		got, err := Restore(ctx, &recordingWriter{}, lrMems().find, superseded, later)
		require.NoError(t, err)
		require.True(t, got.Active())
	})
	t.Run("an active memory", func(t *testing.T) {
		w := &recordingWriter{}
		_, err := Restore(ctx, w, lrMems().find, replacement, later)
		require.ErrorIs(t, err, ErrNotArchived)
		require.Empty(t, w.last.ID)
	})
	t.Run("a chain that cannot be followed", func(t *testing.T) {
		w := &recordingWriter{}
		loop := superseded
		loop.SupersededBy = "A"
		_, err := Restore(ctx, w, lrMems().find, loop, later)
		require.ErrorContains(t, err, "loops at A")
		require.Empty(t, w.last.ID, "an unreadable chain refuses, it never restores")
	})
}
