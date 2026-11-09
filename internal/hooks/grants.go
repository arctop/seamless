package hooks

import (
	"context"
	"fmt"
	"strings"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/store"
)

// captureConfined is the refusal of a plan or subagent capture into target by
// a holder that a read grant confined elsewhere (isolation.requests, off by
// default), or "" when the capture may land. A grant tells its holder it now
// writes only into the project it asked from, and that holds for what the
// hooks record on its behalf too: a capture into that project lands, and one
// aimed anywhere else (a working directory that belongs to another project) is
// dropped with this notice. Grants that cannot be read refuse the capture, as
// they refuse a tool call.
func (h *Handler) captureConfined(ctx context.Context, client Client, externalSessionID, target, what string) string {
	if !h.readGrants {
		return ""
	}
	sessionID := ""
	_, hasView := access.FromContext(ctx)
	if !hasView {
		if sessionID, _ = h.ambientRef(ctx, client, externalSessionID); sessionID == "" {
			return ""
		}
	}
	g, err := store.HolderGrants(ctx, h.db, sessionID, h.now())
	if err != nil {
		h.logger.Warn("hooks: load read grants", "error", err)
		return "Seamless did not record " + what + ": the read grants of this connection could not be checked."
	}
	if !access.WriteConfined(access.WithGrants(ctx, g), target) {
		return ""
	}
	confined, _ := g.Confined()
	holder := "session"
	if hasView {
		holder = "agent"
	}
	where := "project " + target
	if target == "" {
		where = "the global scope"
	}
	return fmt.Sprintf("Seamless did not record %s: this %s was granted a read of another project, so it writes only into %s, and this would land in %s.",
		what, holder, strings.Join(confined, ", "), where)
}
