package feed

import (
	"context"
	"path/filepath"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// A consumer's limits only ever narrow what is sent: above its default a
// value is the default, below a floor it is the floor, and heartbeat_s is the
// consumer's to choose.
func TestParseLimits(t *testing.T) {
	for _, tc := range []struct {
		name string
		body string
		want Limits
	}{
		{"missing keeps the defaults", `{"v":1}`, DefaultLimits},
		{"zero and negative keep the defaults", `{"v":1,"frame_bytes":0,"batch_frames":-1,"batch_bytes":0,"heartbeat_s":-5}`, DefaultLimits},
		{"narrower is taken", `{"v":1,"frame_bytes":2048,"batch_frames":10,"batch_bytes":65536,"heartbeat_s":30}`,
			Limits{FrameBytes: 2048, BatchFrames: 10, BatchBytes: 65536, HeartbeatS: 30}},
		{"wider is the default, but a longer heartbeat is taken",
			`{"v":1,"frame_bytes":99999,"batch_frames":9999,"batch_bytes":99999999,"heartbeat_s":3600}`,
			Limits{FrameBytes: 16384, BatchFrames: 500, BatchBytes: 1 << 20, HeartbeatS: 3600}},
		{"below the floors is the floor", `{"v":1,"frame_bytes":10,"batch_frames":1,"batch_bytes":100}`,
			Limits{FrameBytes: minFrameBytes, BatchFrames: 1, BatchBytes: minBatchBytes, HeartbeatS: 300}},
	} {
		t.Run(tc.name, func(t *testing.T) {
			got, err := parseLimits([]byte(tc.body))
			require.NoError(t, err)
			require.Equal(t, tc.want, got)
		})
	}
	got, err := parseLimits([]byte(`{"v":2,"batch_frames":10}`))
	require.Error(t, err, "another version is refused")
	require.Equal(t, DefaultLimits, got)
}

func TestBoundsFor(t *testing.T) {
	require.Equal(t, spoolBounds{frameBytes: spoolFrameBytes, frames: spoolSegmentFrames, bytes: spoolSegmentBytes}, boundsFor(DefaultLimits),
		"the defaults' bounds are the spool's own")
	b := boundsFor(Limits{FrameBytes: 16384, BatchFrames: 500, BatchBytes: minBatchBytes})
	require.Equal(t, minBatchBytes-spoolHeaderRoom, b.bytes, "a segment leaves the batch room for its header")
	require.Equal(t, b.bytes-1, b.frameBytes, "a frame must fit in one batch, newline and all")
	require.Equal(t, boundsFor(DefaultLimits), boundsFor(Limits{FrameBytes: 1 << 30, BatchFrames: 1 << 30, BatchBytes: 1 << 30}),
		"never wider than the defaults")
}

func TestSpool_SegmentsWithinTheConsumersLimits(t *testing.T) {
	sp, err := openSpool(spoolDir(t))
	require.NoError(t, err)
	defer sp.close()
	// Three frames under the defaults, then the consumer narrows to two frames
	// a batch: the segment being filled is full by the new bound, so the next
	// frame starts a new one.
	for i := range 3 {
		ok, err := sp.add(hbFrame(i))
		require.NoError(t, err)
		require.True(t, ok)
	}
	sp.setLimits(Limits{FrameBytes: 1024, BatchFrames: 2, BatchBytes: minBatchBytes, HeartbeatS: 300})
	big := hbFrame(99)
	big.Fields["pad"] = strings.Repeat("x", 1100)
	ok, err := sp.add(big)
	require.NoError(t, err)
	require.False(t, ok, "a frame longer than the consumer's frame_bytes is never spooled")
	require.Equal(t, 1, sp.stats().Dropped)
	for i := range 5 {
		ok, err := sp.add(hbFrame(10 + i))
		require.NoError(t, err)
		require.True(t, ok)
	}
	var frames []int
	for _, b := range drain(t, sp) {
		frames = append(frames, b.frames)
	}
	require.Equal(t, []int{3, 2, 2, 1}, frames,
		"the segment filled under the defaults is sent as it is; later ones keep to batch_frames")

	// Frames of about 900 bytes under batch_bytes 4096: four of them fit in
	// one default batch, but not in one of these.
	sp.setLimits(Limits{FrameBytes: 1024, BatchFrames: 500, BatchBytes: minBatchBytes, HeartbeatS: 300})
	for i := range 4 {
		f := hbFrame(20 + i)
		f.Fields["pad"] = strings.Repeat("y", 800)
		ok, err := sp.add(f)
		require.NoError(t, err)
		require.True(t, ok)
	}
	batches := drain(t, sp)
	require.Greater(t, len(batches), 1, "batch_bytes starts a new segment")
	total := 0
	for _, b := range batches {
		require.LessOrEqual(t, len(b.body), minBatchBytes, "a batch keeps to batch_bytes, header included")
		total += b.frames
	}
	require.Equal(t, 4, total)
}

// drain sends and settles every batch the spool holds, oldest first.
func drain(t *testing.T, sp *spool) []*pendingBatch {
	t.Helper()
	var out []*pendingBatch
	for {
		b, err := sp.next()
		require.NoError(t, err)
		if b == nil {
			return out
		}
		out = append(out, b)
		_, err = sp.settle(Receipt{Action: ActionSettle})
		require.NoError(t, err)
	}
}

// The feed reads the consumer's limits before it spools, and every batch it
// starts keeps to them.
func TestFeed_BatchesKeepToTheConsumersLimits(t *testing.T) {
	db, rec := feedDB(t)
	ctx, cancel := context.WithCancel(context.Background())
	defer cancel()
	sockDir := shortDir(t)
	sock := filepath.Join(sockDir, "c.sock")
	c := startConsumerWithLimits(t, sock, `{"v":1,"frame_bytes":4096,"batch_frames":2,"batch_bytes":8192,"heartbeat_s":60}`)
	c.down.Store(true)

	on := &atomic.Bool{}
	on.Store(true)
	f := New(Config{DB: db, Events: rec, Socket: sock, Spool: filepath.Join(sockDir, "spool"), Host: "testhost",
		Enabled: func(context.Context) bool { return on.Load() }})
	f.pullEvery, f.backoffFloor, f.backoffCeil = 100*time.Millisecond, 50*time.Millisecond, 300*time.Millisecond
	require.NoError(t, f.Run(ctx))
	require.Eventually(t, func() bool {
		st, ok, _ := readSpoolState(filepath.Join(sockDir, "spool", "state.json"))
		return ok && st.Active
	}, 10*time.Second, 20*time.Millisecond)

	const n = 7
	for range n {
		_, err := rec.Record(ctx, core.Event{Kind: core.EventMemoryWritten, Payload: map[string]any{"kind": "gotcha"}})
		require.NoError(t, err)
	}
	require.Eventually(t, func() bool {
		st, ok, _ := readSpoolState(filepath.Join(sockDir, "spool", "state.json"))
		return ok && st.Inflight != nil
	}, 10*time.Second, 20*time.Millisecond)
	c.down.Store(false)
	require.Eventually(t, func() bool { return countActivities(c.received()) >= n }, 30*time.Second, 50*time.Millisecond)
	cancel()
	f.Wait()

	batches := c.sentBatches()
	require.Greater(t, len(batches), n/2, "seven frames at two a batch take at least four batches")
	for _, b := range batches {
		require.LessOrEqual(t, b.frames, 2, "batch_frames")
		require.LessOrEqual(t, b.bytes, 8192, "batch_bytes")
	}
}
