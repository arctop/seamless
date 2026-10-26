package events

import (
	"context"
	"path/filepath"
	"strconv"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store/storetest"
)

func newRecorder(t *testing.T) *Recorder {
	t.Helper()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	return NewRecorder(db)
}

func TestRecord_StampsIDAndTimeAndPayload(t *testing.T) {
	r := newRecorder(t)
	ctx := context.Background()

	id, err := r.Record(ctx, core.Event{
		Kind:        core.EventMemoryWritten,
		ProjectSlug: "seam",
		ItemID:      "gotcha/chroma-boot-race",
		Payload:     map[string]any{"name": "chroma-boot-race", "score": 0.93},
	})
	require.NoError(t, err)
	require.Len(t, id, 26, "id should be a stamped ULID")

	got, err := r.RecentExcluding(ctx, 10)
	require.NoError(t, err)
	require.Len(t, got, 1)

	e := got[0]
	require.Equal(t, id, e.ID)
	require.Equal(t, core.EventMemoryWritten, e.Kind)
	require.Equal(t, "seam", e.ProjectSlug)
	require.Equal(t, "gotcha/chroma-boot-race", e.ItemID)
	require.False(t, e.TS.IsZero())
	require.Equal(t, "chroma-boot-race", e.Payload["name"])
	require.InEpsilon(t, 0.93, e.Payload["score"], 1e-9)
}

func TestRecord_PreservesExplicitIDAndTS(t *testing.T) {
	r := newRecorder(t)
	ctx := context.Background()

	ts := time.Date(2026, 7, 10, 12, 0, 0, 0, time.UTC)
	_, err := r.Record(ctx, core.Event{ID: "01EXPLICITID0000000000000A", TS: ts, Kind: core.EventSessionStarted})
	require.NoError(t, err)

	got, err := r.RecentExcluding(ctx, 10)
	require.NoError(t, err)
	require.Len(t, got, 1)
	require.Equal(t, "01EXPLICITID0000000000000A", got[0].ID)
	require.True(t, ts.Equal(got[0].TS))
}

func TestRecord_EmptyKindRejected(t *testing.T) {
	r := newRecorder(t)
	_, err := r.Record(context.Background(), core.Event{ProjectSlug: "seam"})
	require.Error(t, err)
}

func TestRecent_NewestFirstAndLimit(t *testing.T) {
	r := newRecorder(t)
	ctx := context.Background()

	base := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	for i := range 5 {
		_, err := r.Record(ctx, core.Event{
			TS:   base.Add(time.Duration(i) * time.Minute),
			Kind: core.EventInjected,
			// distinct item so we can assert ordering
			ItemID: string(rune('a' + i)),
		})
		require.NoError(t, err)
	}

	got, err := r.RecentExcluding(ctx, 3)
	require.NoError(t, err)
	require.Len(t, got, 3)
	// Newest first: minute 4, 3, 2.
	require.Equal(t, "e", got[0].ItemID)
	require.Equal(t, "d", got[1].ItemID)
	require.Equal(t, "c", got[2].ItemID)
}

func mustRecord(t *testing.T, r *Recorder, e core.Event) {
	t.Helper()
	_, err := r.Record(context.Background(), e)
	require.NoError(t, err)
}

func TestTruncate(t *testing.T) {
	tests := []struct {
		name string
		in   string
		max  int
		want string
	}{
		{"disabled-zero", "hello", 0, "hello"},
		{"disabled-negative", "hello", -3, "hello"},
		{"under", "hello", 10, "hello"},
		{"exact", "hello", 5, "hello"},
		{"over", "hello world", 5, "hello" + truncationMarker},
		{"multibyte-not-split", "héllo wörld", 5, "héllo" + truncationMarker},
		{"multibyte-exact-runes", "héllo", 5, "héllo"}, // 6 bytes, 5 runes -> unchanged
		{"empty", "", 4, ""},
	}
	for _, tt := range tests {
		t.Run(tt.name, func(t *testing.T) {
			require.Equal(t, tt.want, Truncate(tt.in, tt.max))
		})
	}
}

// interactionKinds used by the query tests below.
var testInteractionKinds = []core.EventKind{core.EventToolCall, core.EventHookPrompt}

// seedTie records four events: two tool.call sharing base's ts (ids 01A<01B), a
// later hook.prompt (01C), and a memory.written (01D) that the interaction
// queries must exclude.
func seedTie(t *testing.T, r *Recorder, base time.Time) {
	t.Helper()
	mustRecord(t, r, core.Event{ID: "01A", TS: base, Kind: core.EventToolCall, ItemID: "t1"})
	mustRecord(t, r, core.Event{ID: "01B", TS: base, Kind: core.EventToolCall, ItemID: "t2"})
	mustRecord(t, r, core.Event{ID: "01C", TS: base.Add(time.Minute), Kind: core.EventHookPrompt, ItemID: "h1"})
	mustRecord(t, r, core.Event{ID: "01D", TS: base.Add(2 * time.Minute), Kind: core.EventMemoryWritten, ItemID: "m1"})
}

func TestByKinds_FilterAndCursorAcrossTie(t *testing.T) {
	r := newRecorder(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	seedTie(t, r, base)

	// Newest first, limit 2: hook.prompt (01C) then the higher-id tie sibling (01B).
	// memory.written (01D) is filtered out entirely.
	page1, err := r.ByKinds(ctx, testInteractionKinds, "", "", 2)
	require.NoError(t, err)
	require.Len(t, page1, 2)
	require.Equal(t, "01C", page1[0].ID)
	require.Equal(t, "01B", page1[1].ID)

	// Page from the last cursor: yields only 01A (the other tie sibling), never
	// re-emitting 01B nor skipping 01A.
	last := page1[len(page1)-1]
	page2, err := r.ByKinds(ctx, testInteractionKinds, core.FormatTime(last.TS), last.ID, 2)
	require.NoError(t, err)
	require.Len(t, page2, 1)
	require.Equal(t, "01A", page2[0].ID)
}

func TestByKinds_EmptyKindsAndDefaultLimit(t *testing.T) {
	r := newRecorder(t)
	ctx := context.Background()
	got, err := r.ByKinds(ctx, nil, "", "", 0)
	require.NoError(t, err)
	require.Nil(t, got)
}

func TestByKindsAfter_BoundsAndPagesNewestFirstAcrossTie(t *testing.T) {
	r := newRecorder(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	seedTie(t, r, base)

	// The lower bound excludes the two tool calls at base. The non-interaction
	// memory at +2m remains excluded by kind, leaving only the hook prompt.
	bounded, err := r.ByKindsAfter(ctx, testInteractionKinds, core.FormatTime(base.Add(time.Minute)), "", "", 10)
	require.NoError(t, err)
	require.Len(t, bounded, 1)
	require.Equal(t, "01C", bounded[0].ID)

	// A bound at base includes the tied rows. Page one is newest-first, and the
	// cursor continues within the same bound without repeating its last row.
	page1, err := r.ByKindsAfter(ctx, testInteractionKinds, core.FormatTime(base), "", "", 2)
	require.NoError(t, err)
	require.Equal(t, []string{"01C", "01B"}, []string{page1[0].ID, page1[1].ID})

	last := page1[len(page1)-1]
	page2, err := r.ByKindsAfter(ctx, testInteractionKinds, core.FormatTime(base), core.FormatTime(last.TS), last.ID, 2)
	require.NoError(t, err)
	require.Len(t, page2, 1)
	require.Equal(t, "01A", page2[0].ID)
}

func TestByKindsSince_StrictlyNewerOldestFirstAcrossTie(t *testing.T) {
	r := newRecorder(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	seedTie(t, r, base)

	// Cursor at the earliest tie sibling (01A, ts=base): strictly-newer means the
	// same-ts higher-id sibling (01B) plus the later hook.prompt (01C), oldest
	// first.
	got, err := r.ByKindsSince(ctx, testInteractionKinds, core.FormatTime(base), "01A", 10)
	require.NoError(t, err)
	require.Len(t, got, 2)
	require.Equal(t, "01B", got[0].ID)
	require.Equal(t, "01C", got[1].ID)
}

func TestRecentExcluding_OmitsGivenKinds(t *testing.T) {
	r := newRecorder(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	mustRecord(t, r, core.Event{ID: "01A", TS: base, Kind: core.EventToolCall})
	mustRecord(t, r, core.Event{ID: "01B", TS: base.Add(time.Minute), Kind: core.EventMemoryWritten})
	mustRecord(t, r, core.Event{ID: "01C", TS: base.Add(2 * time.Minute), Kind: core.EventHookPrompt})
	mustRecord(t, r, core.Event{ID: "01D", TS: base.Add(3 * time.Minute), Kind: core.EventSessionStarted})

	got, err := r.RecentExcluding(ctx, 10, core.EventToolCall, core.EventHookPrompt)
	require.NoError(t, err)
	require.Len(t, got, 2)
	// Newest first, transport kinds omitted.
	require.Equal(t, core.EventSessionStarted, got[0].Kind)
	require.Equal(t, core.EventMemoryWritten, got[1].Kind)

	// No exclusions returns every kind.
	all, err := r.RecentExcluding(ctx, 10)
	require.NoError(t, err)
	require.Len(t, all, 4)
}

func TestPruneKinds_OnlyGivenKindsBeforeCutoff(t *testing.T) {
	r := newRecorder(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	mustRecord(t, r, core.Event{ID: "01A", TS: base, Kind: core.EventToolCall})                    // old transport -> pruned
	mustRecord(t, r, core.Event{ID: "01B", TS: base, Kind: core.EventHookPrompt})                  // old transport -> pruned
	mustRecord(t, r, core.Event{ID: "01C", TS: base, Kind: core.EventMemoryWritten})               // old domain -> kept
	mustRecord(t, r, core.Event{ID: "01D", TS: base.Add(2 * time.Hour), Kind: core.EventToolCall}) // fresh transport -> kept

	cutoff := base.Add(time.Hour)
	n, err := r.PruneKinds(ctx, testInteractionKinds, cutoff)
	require.NoError(t, err)
	require.Equal(t, int64(2), n)

	got, err := r.RecentExcluding(ctx, 10)
	require.NoError(t, err)
	require.Len(t, got, 2)
	kinds := map[core.EventKind]bool{}
	for _, e := range got {
		kinds[e.Kind] = true
	}
	require.True(t, kinds[core.EventMemoryWritten], "domain event must survive")
	require.True(t, kinds[core.EventToolCall], "fresh transport event must survive")

	// Empty kinds / zero cutoff are no-ops.
	n0, err := r.PruneKinds(ctx, nil, cutoff)
	require.NoError(t, err)
	require.Equal(t, int64(0), n0)
	n1, err := r.PruneKinds(ctx, testInteractionKinds, time.Time{})
	require.NoError(t, err)
	require.Equal(t, int64(0), n1)
}

func TestSubscribe_ReceivesPublishedEvent(t *testing.T) {
	r := newRecorder(t)
	ctx := context.Background()

	ch, unsubscribe := r.Subscribe()
	defer unsubscribe()

	id, err := r.Record(ctx, core.Event{Kind: core.EventMemoryWritten, ItemID: "m1"})
	require.NoError(t, err)

	select {
	case e := <-ch:
		require.Equal(t, id, e.ID)
		require.Equal(t, core.EventMemoryWritten, e.Kind)
	case <-time.After(2 * time.Second):
		t.Fatal("did not receive published event")
	}
}

func TestUnsubscribe_StopsDeliveryAndIsIdempotent(t *testing.T) {
	r := newRecorder(t)
	ch, unsubscribe := r.Subscribe()
	unsubscribe()
	unsubscribe() // idempotent -- must not panic

	// Channel is closed; a receive returns the zero value with ok=false.
	_, open := <-ch
	require.False(t, open)

	// Recording after unsubscribe must not block or panic.
	_, err := r.Record(context.Background(), core.Event{Kind: core.EventMemoryRead, ItemID: "m2"})
	require.NoError(t, err)
}

func TestPublish_DropsWhenSubscriberFull(t *testing.T) {
	r := newRecorder(t)
	_, unsubscribe := r.Subscribe() // never drained
	defer unsubscribe()

	// Far more than subBuffer events; publish must never block on the full channel.
	for range subBuffer * 3 {
		_, err := r.Record(context.Background(), core.Event{Kind: core.EventMemoryRead, ItemID: "x"})
		require.NoError(t, err)
	}
}

// Race guard: publishers, subscribers, and unsubscribers all touch the subs map
// and its channels concurrently -- exactly the daemon's shape (tool handlers
// recording while console SSE clients connect and disconnect). Run under -race
// this must show no data race, no send-on-closed panic, and no deadlock.
func TestRecorder_ConcurrentPublishSubscribeUnsubscribe(t *testing.T) {
	r := newRecorder(t)
	ctx := context.Background()

	var wg sync.WaitGroup
	// Writers: the single-conn SQLite serializes the inserts; the fan-out is the
	// contended path.
	for w := range 4 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			for i := range 25 {
				_, err := r.Record(ctx, core.Event{
					Kind: core.EventMemoryRead, ItemID: "w" + strconv.Itoa(w) + "-" + strconv.Itoa(i),
				})
				require.NoError(t, err)
			}
		}()
	}
	// Churning subscribers: some drain a little, all unsubscribe mid-stream.
	for range 8 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			ch, unsubscribe := r.Subscribe()
			for range 5 {
				select {
				case <-ch:
				default:
				}
			}
			unsubscribe()
			unsubscribe() // idempotent under contention too
		}()
	}
	wg.Wait()
}

// --- ByID / BySession / KindTimeline ---------------------------------------
//
// The three readers the console reaches on every event-detail, session-timeline
// and interaction-volume render, and the only Recorder queries that had no
// coverage at all.

func TestByID_FoundMissingAndEmpty(t *testing.T) {
	r := newRecorder(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	mustRecord(t, r, core.Event{
		ID: "01A", TS: base, Kind: core.EventMemoryWritten,
		SessionID: "s1", ProjectSlug: "seam", ItemID: "m1",
		Payload: map[string]any{"name": "chroma-boot-race"},
	})

	got, ok, err := r.ByID(ctx, "01A")
	require.NoError(t, err)
	require.True(t, ok)
	require.Equal(t, "01A", got.ID)
	require.Equal(t, core.EventMemoryWritten, got.Kind)
	require.Equal(t, "s1", got.SessionID)
	require.Equal(t, "seam", got.ProjectSlug)
	require.Equal(t, "m1", got.ItemID)
	require.True(t, got.TS.Equal(base))
	require.Equal(t, "chroma-boot-race", got.Payload["name"])

	// A missing id is ok=false with a NIL error -- the console renders a 404 off
	// the bool, so a not-found must never surface as a failure.
	_, ok, err = r.ByID(ctx, "01NOPE")
	require.NoError(t, err)
	require.False(t, ok)

	// An empty id short-circuits ahead of the query, same shape.
	_, ok, err = r.ByID(ctx, "")
	require.NoError(t, err)
	require.False(t, ok)
}

func TestBySession_ChronologicalScopedAndLimited(t *testing.T) {
	r := newRecorder(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	// Recorded newest-first to prove the query orders rather than the insert.
	mustRecord(t, r, core.Event{ID: "01C", TS: base.Add(2 * time.Minute), Kind: core.EventSessionEnded, SessionID: "s1"})
	mustRecord(t, r, core.Event{ID: "01B", TS: base.Add(time.Minute), Kind: core.EventMemoryWritten, SessionID: "s1"})
	mustRecord(t, r, core.Event{ID: "01A", TS: base, Kind: core.EventSessionStarted, SessionID: "s1"})
	mustRecord(t, r, core.Event{ID: "01X", TS: base, Kind: core.EventSessionStarted, SessionID: "s2"})

	// Oldest first (a timeline reads down the page), and s2 never bleeds in.
	got, err := r.BySession(ctx, "s1", 0)
	require.NoError(t, err)
	require.Len(t, got, 3)
	require.Equal(t, []string{"01A", "01B", "01C"}, []string{got[0].ID, got[1].ID, got[2].ID})

	// The limit keeps the OLDEST rows, because the ASC order applies first.
	head, err := r.BySession(ctx, "s1", 2)
	require.NoError(t, err)
	require.Len(t, head, 2)
	require.Equal(t, []string{"01A", "01B"}, []string{head[0].ID, head[1].ID})

	// An unknown session is empty, not an error; an empty id short-circuits.
	none, err := r.BySession(ctx, "nobody", 0)
	require.NoError(t, err)
	require.Empty(t, none)

	empty, err := r.BySession(ctx, "", 0)
	require.NoError(t, err)
	require.Nil(t, empty)
}

func TestKindTimeline_KindProjectAndSinceWindow(t *testing.T) {
	r := newRecorder(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	mustRecord(t, r, core.Event{ID: "01A", TS: base, Kind: core.EventToolCall, ProjectSlug: "seam"})
	mustRecord(t, r, core.Event{ID: "01B", TS: base.Add(time.Hour), Kind: core.EventHookPrompt, ProjectSlug: "seam"})
	mustRecord(t, r, core.Event{ID: "01C", TS: base.Add(2 * time.Hour), Kind: core.EventToolCall, ProjectSlug: "other"})
	mustRecord(t, r, core.Event{ID: "01D", TS: base.Add(3 * time.Hour), Kind: core.EventMemoryWritten, ProjectSlug: "seam"})

	// Newest first, only the interaction kinds -- memory.written (01D) is out.
	all, err := r.KindTimeline(ctx, testInteractionKinds, "", "", 10)
	require.NoError(t, err)
	require.Len(t, all, 3)
	require.Equal(t, "01C", all[0].ID)
	require.True(t, all[0].TS.Equal(base.Add(2*time.Hour)), "newest first")
	require.Equal(t, string(core.EventToolCall), all[0].Kind)
	require.True(t, all[2].TS.Equal(base), "oldest last")

	// project scopes to one slug.
	seam, err := r.KindTimeline(ctx, testInteractionKinds, "seam", "", 10)
	require.NoError(t, err)
	require.Len(t, seam, 2)

	// sinceTS is inclusive (>=), so an event exactly on the boundary is kept.
	since, err := r.KindTimeline(ctx, testInteractionKinds, "", core.FormatTime(base.Add(time.Hour)), 10)
	require.NoError(t, err)
	require.Len(t, since, 2)

	// The limit cuts from the newest end.
	capped, err := r.KindTimeline(ctx, testInteractionKinds, "", "", 1)
	require.NoError(t, err)
	require.Len(t, capped, 1)
	require.True(t, capped[0].TS.Equal(base.Add(2*time.Hour)))
}

func TestKindTimeline_EmptyKindsOrLimitReturnsNil(t *testing.T) {
	r := newRecorder(t)
	ctx := context.Background()
	base := time.Date(2026, 7, 10, 0, 0, 0, 0, time.UTC)
	mustRecord(t, r, core.Event{ID: "01A", TS: base, Kind: core.EventToolCall})

	// Both guards return (nil, nil) rather than querying: the console asks for a
	// zero-width window while a page is still settling.
	got, err := r.KindTimeline(ctx, nil, "", "", 10)
	require.NoError(t, err)
	require.Nil(t, got)

	got, err = r.KindTimeline(ctx, testInteractionKinds, "", "", 0)
	require.NoError(t, err)
	require.Nil(t, got)
}

// RecordOnce is the once-per-item latch: the guard and the insert are one
// statement, so a second attempt -- same kind, same item -- lands nothing,
// publishes nothing, and reports recorded=false rather than an error.
func TestRecordOnce_LatchesPerItem(t *testing.T) {
	r := newRecorder(t)
	ctx := context.Background()
	ch, unsub := r.Subscribe()
	defer unsub()

	id, recorded, err := r.RecordOnce(ctx, core.Event{
		Kind: core.EventMemoryFirstReuse, ItemID: "01MEM",
		Payload: map[string]any{"name": "chroma-boot-race"},
	})
	require.NoError(t, err)
	require.True(t, recorded)
	require.Len(t, id, 26)
	select {
	case e := <-ch:
		require.Equal(t, core.EventMemoryFirstReuse, e.Kind)
	default:
		t.Fatal("the first record must publish to subscribers")
	}

	id2, recorded2, err := r.RecordOnce(ctx, core.Event{
		Kind: core.EventMemoryFirstReuse, ItemID: "01MEM",
	})
	require.NoError(t, err)
	require.False(t, recorded2, "the latch was already set")
	require.Empty(t, id2)
	select {
	case <-ch:
		t.Fatal("a latched no-op must not publish")
	default:
	}

	// A different item, and a different kind for the same item, both still land.
	_, recorded3, err := r.RecordOnce(ctx, core.Event{Kind: core.EventMemoryFirstReuse, ItemID: "01OTHER"})
	require.NoError(t, err)
	require.True(t, recorded3)
	_, recorded4, err := r.RecordOnce(ctx, core.Event{Kind: core.EventMemoryRead, ItemID: "01MEM"})
	require.NoError(t, err)
	require.True(t, recorded4)

	_, _, err = r.RecordOnce(ctx, core.Event{Kind: core.EventMemoryFirstReuse})
	require.Error(t, err, "the latch is per item, so an empty item id is a caller bug")
}
