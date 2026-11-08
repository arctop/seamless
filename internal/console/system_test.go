package console

import (
	"context"
	"encoding/json"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"sort"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
)

func systemJSON(t *testing.T, mux *http.ServeMux) systemData {
	t.Helper()
	rec := getPeek(t, mux, "/console/system?format=json")
	require.Equal(t, http.StatusOK, rec.Code, rec.Body.String())
	var d systemData
	require.NoError(t, json.Unmarshal(rec.Body.Bytes(), &d))
	return d
}

func partByID(d systemData, id string) systemPart {
	for _, p := range d.Parts {
		if p.ID == id {
			return p
		}
	}
	return systemPart{}
}

// sceneParts reads the part ids the 3D scene lays out (static/orrery.js), so
// the server's parts and the drawing cannot drift apart.
func sceneParts(t *testing.T) []string {
	t.Helper()
	src, err := os.ReadFile(filepath.Join("static", "orrery.js"))
	require.NoError(t, err)
	re := regexp.MustCompile(`\{ id: '([a-z0-9]+)', ring: '`)
	var ids []string
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		ids = append(ids, m[1])
	}
	require.NotEmpty(t, ids)
	return ids
}

func TestSystem_PartsMatchTheScene(t *testing.T) {
	_, mux := newConsole(t)
	d := systemJSON(t, mux)
	var got []string
	for _, p := range d.Parts {
		require.NotEmpty(t, p.Label, p.ID)
		require.NotEmpty(t, p.Role, p.ID)
		require.NotEmpty(t, p.What, p.ID)
		require.Contains(t, []string{partOn, partIdle, partWarn, partOff, partAbsent}, p.State, p.ID)
		require.Contains(t, d.Rings(), p.Ring, p.ID)
		got = append(got, p.ID)
	}
	want := sceneParts(t)
	sort.Strings(got)
	sort.Strings(want)
	require.Equal(t, want, got, "every part the scene draws has a readout, and nothing else")
}

func TestSystem_ChainsNameRealParts(t *testing.T) {
	ids := sceneParts(t)
	for kind, c := range systemChains {
		require.NotEmpty(t, c.Hops, "%s lights nothing", kind)
		for _, id := range c.Hops {
			require.True(t, slices.Contains(ids, id), "%s names unknown part %q", kind, id)
		}
		for _, pair := range c.Also {
			require.Len(t, pair, 2, "%s: a side trip is a pair", kind)
			for _, id := range pair {
				require.True(t, slices.Contains(ids, id), "%s names unknown part %q", kind, id)
			}
		}
	}
}

func TestSystem_PageOwnsItsLiveness(t *testing.T) {
	_, mux := newConsole(t)
	rec := getPeek(t, mux, "/console/system")
	require.Equal(t, http.StatusOK, rec.Code)
	body := rec.Body.String()
	require.Contains(t, body, `id="sys-stage" data-live-skip`, "the morph never touches the canvas")
	require.Contains(t, body, "window.SEAM_NO_LIVE_REFRESH = true", "the page refreshes its own numbers")
	require.Contains(t, body, `<script src="/console/static/orrery.js"></script>`)
	require.Contains(t, body, `<script type="application/json" id="sys-data">`)
	require.Contains(t, body, `data-pick="database"`, "every part has a button, with or without WebGL")

	js := getPeek(t, mux, "/console/static/orrery.js")
	require.Equal(t, http.StatusOK, js.Code)
	require.Contains(t, js.Header().Get("Content-Type"), "javascript")
}

func TestSystem_OffPartsSayHowToTurnThemOn(t *testing.T) {
	_, mux := newConsoleFeatures(t, config.Features{})
	d := systemJSON(t, mux)
	for _, id := range []string{"screening", "labs", "socket", "approvals", "commits", "feed"} {
		p := partByID(d, id)
		require.Equal(t, partOff, p.State, id)
		require.NotNil(t, p.TurnOn, "%s is off and says how to turn it on", id)
		require.Empty(t, p.Spark, "%s is off and draws no activity line", id)
	}
	require.Equal(t, partAbsent, partByID(d, "updates").State, "no update check runs in this process")
	require.Equal(t, d.Tally[partOff], func() int {
		n := 0
		for _, p := range d.Parts {
			if p.State == partOff || p.State == partAbsent {
				n++
			}
		}
		return n
	}(), "the tally counts absent parts as off")
}

func TestSystem_ActivityComesFromTheEventLog(t *testing.T) {
	db, mux := newConsole(t)
	quiet := systemJSON(t, mux)
	require.Equal(t, partIdle, partByID(quiet, "memories").State, "running but nothing recorded: quiet")

	now := time.Now().UTC()
	_, err := db.ExecContext(context.Background(), `INSERT INTO events (id, ts, kind, payload) VALUES (?, ?, ?, '{}')`,
		"01EVTSYSTEM0001", core.FormatTime(now.Add(-30*time.Second)), string(core.EventMemoryWritten))
	require.NoError(t, err)

	d := systemJSON(t, mux)
	mem := partByID(d, "memories")
	require.Equal(t, partOn, mem.State, "a recent write puts the part to work")
	require.Len(t, mem.Spark, systemSparkMinutes)
	require.Equal(t, 1, mem.Spark[systemSparkMinutes-1], "the write lands in the newest minute")
	require.Equal(t, partOn, partByID(d, "database").State, "every part on the write's path worked")
	require.Equal(t, partOff, partByID(d, "watcher").State, "no files layer in this process: the watcher is off, whatever the log says")
	require.Equal(t, partOn, partByID(d, "events").State, "the event log counts every event")
	require.Equal(t, partIdle, partByID(d, "tasks").State, "a part off the path stays quiet")
}
