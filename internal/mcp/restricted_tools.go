package mcp

import (
	"maps"
	"strings"

	"github.com/mark3labs/mcp-go/mcp"
)

// The registered tool text promises that naming an unknown project creates it.
// That holds for the owner, whose tools/list must stay as it is, and is false
// for a restricted account (peer_auth.restricted), which writes only into open
// projects that already exist.
const (
	restrictedProjectArgDesc = "project slug; defaults to the bound/ambient session's project. " +
		"This restricted account writes only into open projects that already exist: an unknown slug is refused, never created, " +
		"and the global scope is read-only for it. With no session and no explicit project the call is rejected as ambiguous."
	restrictedProjectCreateDesc = "Not available to this restricted account, so a call is refused: the owner creates projects " +
		"and maps repositories to them. Use project_list to see the projects it may write to."

	sessionUnknownSlugCreates = "an unknown slug creates that project."
	sessionUnknownSlugRefused = "an unknown slug is refused for this restricted account."
)

// restrictedToolText is tools with the text a restricted account sees. The
// list is the server's own, shared by every connection, so what changes is
// copied rather than written through.
func restrictedToolText(tools []mcp.Tool) []mcp.Tool {
	out := make([]mcp.Tool, len(tools))
	for i, t := range tools {
		if t.Name == "project_create" {
			t.Description = restrictedProjectCreateDesc
		}
		if prop, ok := t.InputSchema.Properties["project"].(map[string]any); ok {
			if desc, ok := restrictedProjectDesc(prop); ok {
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
func restrictedProjectDesc(prop map[string]any) (string, bool) {
	desc, _ := prop["description"].(string)
	switch {
	case desc == writeProjectArgDesc:
		return restrictedProjectArgDesc, true
	case strings.Contains(desc, sessionUnknownSlugCreates):
		return strings.Replace(desc, sessionUnknownSlugCreates, sessionUnknownSlugRefused, 1), true
	}
	return "", false
}
