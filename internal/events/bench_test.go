package events

// Hot-path baseline benchmarks for the event log's write path: Record (ULID
// stamp + payload marshal + INSERT + fan-out) with N live SSE subscribers, and
// the in-memory publish fan-out isolated from the DB insert. Subscribers drain
// their channels concurrently, as the console's SSE handlers do. Fixtures are
// fresh on-disk SQLite databases (same store.Open PRAGMAs as production). Run
// with `make bench`.

import (
	"context"
	"fmt"
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store/storetest"
)

func openBenchRecorder(b *testing.B) *Recorder {
	b.Helper()
	db, err := storetest.OpenFresh(b, filepath.Join(b.TempDir(), "seam.db"))
	require.NoError(b, err)
	b.Cleanup(func() { _ = db.Close() })
	return NewRecorder(db)
}

// startDrain subscribes n live subscribers, each drained by its own goroutine
// (mirroring SSE handlers), and returns a stop func that unsubscribes them all
// and waits for the drain goroutines to exit.
func startDrain(r *Recorder, n int) (stop func()) {
	var wg sync.WaitGroup
	cancels := make([]func(), 0, n)
	for range n {
		ch, cancel := r.Subscribe()
		cancels = append(cancels, cancel)
		wg.Add(1)
		go func() {
			defer wg.Done()
			for range ch {
			}
		}()
	}
	return func() {
		for _, cancel := range cancels {
			cancel()
		}
		wg.Wait()
	}
}

// benchEvent returns the small business-level event shape Record sees on the
// hot path. ID and TS are left zero so every Record stamps a fresh ULID and
// timestamp, as production call sites do.
func benchEvent() core.Event {
	return core.Event{
		Kind:        core.EventMemoryWritten,
		ProjectSlug: "bench",
		ItemID:      "gotcha/chroma-boot-race",
		Payload:     map[string]any{"name": "chroma-boot-race", "score": 0.93},
	}
}

// BenchmarkRecordFanout measures the full event append: ULID stamp, payload
// marshal, single-connection WAL INSERT (auto-commit per event, as in
// production), and best-effort fan-out to subs live subscribers.
func BenchmarkRecordFanout(b *testing.B) {
	ctx := context.Background()
	for _, subs := range []int{0, 1, 8, 32} {
		b.Run(fmt.Sprintf("subs=%d", subs), func(b *testing.B) {
			r := openBenchRecorder(b)
			stop := startDrain(r, subs)
			defer stop()
			ev := benchEvent()
			b.ReportAllocs()
			for b.Loop() {
				if _, err := r.Record(ctx, ev); err != nil {
					b.Fatal(err)
				}
			}
		})
	}
}

// BenchmarkPublishFanout isolates the in-memory fan-out (the per-subscriber
// non-blocking send under the recorder mutex) from the DB insert, so the SSE
// distribution cost is visible on its own.
func BenchmarkPublishFanout(b *testing.B) {
	for _, subs := range []int{1, 8, 32, 128} {
		b.Run(fmt.Sprintf("subs=%d", subs), func(b *testing.B) {
			r := NewRecorder(nil) // publish never touches the DB
			stop := startDrain(r, subs)
			defer stop()
			ev := benchEvent()
			ev.ID = "01BENCHEVENT0000000000000A"
			b.ReportAllocs()
			for b.Loop() {
				r.publish(ev)
			}
		})
	}
}
