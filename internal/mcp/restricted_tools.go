package mcp

import (
	"maps"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// The registered tool text promises that naming an unknown project creates it.
// That holds for the owner, whose tools/list must stay as it is, and is false
// for a restricted account (peer_auth.restricted), which writes only into open
// projects that already exist -- or, when its entry shapes its reach
// (access.ReachShaped), only into the projects project_list shows it.
const (
	restrictedProjectArgDesc = "project slug; defaults to the bound/ambient session's project. " +
		"This restricted account writes only into open projects that already exist: an unknown slug is refused, never created, " +
		"and the global scope is read-only for it. With no session and no explicit project the call is rejected as ambiguous."
	shapedProjectArgDesc = "project slug; defaults to the bound/ambient session's project. " +
		"This restricted account writes only into the projects project_list shows: an unknown slug is refused, never created, " +
		"and it never writes the global scope. With no session and no explicit project the call is rejected as ambiguous."
	restrictedProjectCreateDesc = "Not available to this restricted account, so a call is refused: the owner creates projects " +
		"and maps repositories to them. Use project_list to see the projects it may write to."
	restrictedGardenerPlanDesc = "Not available to this restricted account, so a call is refused: reorganizing memory is " +
		"the owner's curation. Use gardener_proposals to review what is pending; the owner raises and resolves proposals " +
		"on the console's Gardener screen."

	sessionUnknownSlugCreates = "an unknown slug creates that project."
	sessionUnknownSlugRefused = "an unknown slug is refused for this restricted account."
)

// restrictedToolText is tools with the text a restricted account sees, in the
// shaped wording when its entry shapes its reach. The list is the server's
// own, shared by every connection, so what changes is copied rather than
// written through.
func restrictedToolText(tools []mcp.Tool, shaped bool) []mcp.Tool {
	out := make([]mcp.Tool, len(tools))
	for i, t := range tools {
		switch t.Name {
		case "project_create":
			t.Description = restrictedProjectCreateDesc
		case "gardener_request", "gardener_split":
			t.Description = restrictedGardenerPlanDesc
		}
		if prop, ok := t.InputSchema.Properties["project"].(map[string]any); ok {
			if desc, ok := restrictedProjectDesc(prop, shaped); ok {
				prop = maps.Clone(prop)
				prop["description"] = desc
				props := maps.Clone(t.InputSchema.Properties)
				props["project"] = prop
				t.InputSchema.Properties = props
			}
		}
		out[i] = t
	}
	return out
}

// restrictedProjectDesc is the restricted account's wording of a project
// argument whose description promises a new project, and whether it has one.
func restrictedProjectDesc(prop map[string]any, shaped bool) (string, bool) {
	desc, _ := prop["description"].(string)
	switch {
	case desc == writeProjectArgDesc && shaped:
		return shapedProjectArgDesc, true
	case desc == writeProjectArgDesc:
		return restrictedProjectArgDesc, true
	case strings.Contains(desc, sessionUnknownSlugCreates):
		return strings.Replace(desc, sessionUnknownSlugCreates, sessionUnknownSlugRefused, 1), true
	}
	return "", false
}
