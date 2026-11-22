package console

import (
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// A task hit in search and the palette (?format=json&fast=1) says its state
// in the console's words, never the stored status (UX 24): an open task reads
// ready, blocked or waiting on its gate's person, as every task surface does.
func TestSearch_TaskHitSaysItsStateInWords(t *testing.T) {
	db, mux := newConsole(t)
	gate := bkTask(t, db, "example-ops", "", "zebrafish gate", func(k *core.Task) {
		k.Kind, k.Gate = core.TaskKindGate, core.TaskGate{Who: "owner", What: "approve"}
	})
	first := bkTask(t, db, "example-ops", "", "zebrafish first", nil)
	bkTask(t, db, "example-ops", "", "zebrafish second", func(k *core.Task) { k.DependsOn = []string{first} })
	bkTask(t, db, "example-ops", "", "zebrafish held", func(k *core.Task) { k.DependsOn = []string{gate} })

	var data searchData
	getJSON(t, mux, "/console/search?q=zebrafish&format=json&fast=1", &data)
	got := map[string]string{}
	for _, g := range data.Groups {
		if g.Kind != "tasks" {
			continue
		}
		for _, r := range g.Rows {
			got[r.Title] = r.Description
		}
	}
	require.Equal(t, map[string]string{
		"zebrafish gate":   "waiting on owner",
		"zebrafish first":  "ready",
		"zebrafish second": "blocked",
		"zebrafish held":   "waiting on owner",
	}, got)
	page := getHTMLBody(t, mux, "/console/search?q=zebrafish")
	require.NotContains(t, page, `<div class="search-result-desc">open</div>`)
}
