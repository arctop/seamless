package diary

import (
	"context"
	"database/sql"
	"os"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/store"
)

// The year-sized fixture: a year of generated history at a heavy owner's
// pace (twenty sessions a weekday across five places, a quarter with
// subagents, prompts with injected text, thirty days of tool calls, the first
// month from before the ledger), built once per benchmark run into its own
// temporary directory.
var dmYear struct {
	once sync.Once
	dir  string
	path string
	err  error
	busy time.Time // the busiest weekday of the last month
}

// dmYearDB opens the year-sized fixture, building it on first use.
func dmYearDB(b *testing.B) (*sql.DB, time.Time) {
	b.Helper()
	dmYear.once.Do(func() {
		dmYear.dir, dmYear.err = os.MkdirTemp("", "diary-bench-")
		if dmYear.err != nil {
			return
		}
		dmYear.path = filepath.Join(dmYear.dir, "seam.db")
		f := dmOpenAt(b, dmYear.path, time.Local)
		end := midnight(time.Now(), time.Local)
		start := end.AddDate(-1, 0, 0)
		began := time.Now()
		f.inTx(func() {
			dmGenerate(f, start, 365, 42, dmScale{sessions: 20, prompts: true, content: 600, tools: 60,
				ledger: start.AddDate(0, 1, 0), recent: end.AddDate(0, 0, -30)})
		})
		b.Logf("year fixture built in %s", time.Since(began).Round(time.Millisecond))
		dmYear.busy = dmBusiest(b, f, end)
		_ = f.db.Close()
	})
	require.NoError(b, dmYear.err)
	db, err := store.Open(dmYear.path)
	require.NoError(b, err)
	b.Cleanup(func() { _ = db.Close() })
	return db, dmYear.busy
}

// dmBusiest is the day of the last month with the most agent time.
func dmBusiest(tb testing.TB, f *dmFixture, end time.Time) time.Time {
	tb.Helper()
	m, err := monthAt(f.ctx, f.db, end.AddDate(0, 0, -15), end, time.Local)
	require.NoError(tb, err)
	best, bestMinutes := end.AddDate(0, 0, -15), -1
	for _, d := range m.Days {
		if d.Facts.Minutes > bestMinutes {
			day, err := ParseDay(d.Day)
			require.NoError(tb, err)
			best, bestMinutes = day, d.Facts.Minutes
		}
	}
	return best
}

// dmOpenAt opens a fresh fixture at path, closed by its caller.
func dmOpenAt(tb testing.TB, path string, loc *time.Location) *dmFixture {
	tb.Helper()
	db, err := store.Open(path)
	require.NoError(tb, err)
	return &dmFixture{tb: tb, ctx: context.Background(), db: db, ex: db, path: path, loc: loc}
}

func TestMain(m *testing.M) {
	code := m.Run()
	if dmYear.dir != "" {
		_ = os.RemoveAll(dmYear.dir)
	}
	os.Exit(code)
}

func BenchmarkDay(b *testing.B) {
	db, busy := dmYearDB(b)
	ctx := context.Background()
	e, err := Day(ctx, db, busy)
	require.NoError(b, err)
	b.ReportMetric(float64(e.Facts.Sessions), "sessions")
	for b.Loop() {
		if _, err := Day(ctx, db, busy); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMonth(b *testing.B) {
	db, busy := dmYearDB(b)
	ctx := context.Background()
	for b.Loop() {
		if _, err := Month(ctx, db, busy); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkUsual(b *testing.B) {
	db, busy := dmYearDB(b)
	ctx := context.Background()
	for b.Loop() {
		if _, err := Usual(ctx, db, busy); err != nil {
			b.Fatal(err)
		}
	}
}

// The console reads through the filtered handle while a project is locked:
// the same reads, through the views.
func BenchmarkDayThroughTheView(b *testing.B) {
	_, busy := dmYearDB(b)
	view, err := store.OpenView(dmYear.path, []string{"example-legal"})
	require.NoError(b, err)
	b.Cleanup(func() { _ = view.Close() })
	ctx := context.Background()
	for b.Loop() {
		if _, err := Day(ctx, view, busy); err != nil {
			b.Fatal(err)
		}
	}
}

func BenchmarkMonthThroughTheView(b *testing.B) {
	_, busy := dmYearDB(b)
	view, err := store.OpenView(dmYear.path, []string{"example-legal"})
	require.NoError(b, err)
	b.Cleanup(func() { _ = view.Close() })
	ctx := context.Background()
	for b.Loop() {
		if _, err := Month(ctx, view, busy); err != nil {
			b.Fatal(err)
		}
	}
}
