package console

import (
	"context"
	"net/http"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/files"
)

// A [[name]] link is a by-name read (retrieve.MemoryByNameRead): when the
// project's memory of that name is a restricted account's and the global memory
// of it is a briefing-injected kind, the link goes to the global rule. An
// owner's project memory, and one whose global namesake is no rule, still
// answer as before.
func TestWikiLink_RestrictedProjectMemoryStepsAsideForTheRule(t *testing.T) {
	for _, tc := range []struct {
		name       string
		account    string
		globalKind core.MemoryKind
		wantGlobal bool
	}{
		{"a restricted account's memory steps aside", "_example_agent", core.KindConstraint, true},
		{"a stage is a rule too", "_example_agent", core.KindStage, true},
		{"the owner's project memory still answers", "", core.KindConstraint, false},
		{"a non-briefing global kind does not trigger it", "_example_agent", core.KindGotcha, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			_, mgr, mux := newConsoleWithFiles(t)
			local := writeAccountMemory(t, mgr, "seamless", "deploy-rule", tc.account)
			global := writeMemory(t, mgr, tc.globalKind, "", "deploy-rule", "the owner's rule")
			note := writeNote(t, mgr, "seamless", "deploy-notes", "Deploy notes", "Follow [[deploy-rule]] first.")

			frag := getPeek(t, mux, "/console/notes/"+note.ID+"?peek=1")
			require.Equal(t, http.StatusOK, frag.Code)
			body := frag.Body.String()
			want, other := local.ID, global.ID
			if tc.wantGlobal {
				want, other = global.ID, local.ID
			}
			require.Contains(t, body, "/console/memories/"+want)
			require.NotContains(t, body, "/console/memories/"+other)
		})
	}
}

// writeAccountMemory writes a project gotcha whose content account wrote ("" =
// the owner).
func writeAccountMemory(t *testing.T, mgr *files.Manager, project, name, account string) core.Memory {
	t.Helper()
	id, err := core.NewID()
	require.NoError(t, err)
	now := time.Now().UTC()
	m, err := mgr.WriteMemory(context.Background(), core.Memory{
		ID: id, Kind: core.KindGotcha, Name: name, Description: "the project's " + name, Project: project,
		Body: "body of " + name, Created: now, Updated: now, ValidFrom: now, Account: account,
	})
	require.NoError(t, err)
	return m
}
