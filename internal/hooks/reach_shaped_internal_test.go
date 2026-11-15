package hooks

import (
	"regexp"
	"testing"

	"github.com/stretchr/testify/require"
)

// The SessionStart notice of a restricted account outside its reach: a plain
// entry's is the one it always was; a shaped entry's names no isolation state
// (decision B-D9). Neither names a project.
func TestRestrictedNoticeFor(t *testing.T) {
	require.Equal(t, restrictedNotice("_example_agent"), restrictedNoticeFor("_example_agent", false))
	n := restrictedNoticeFor("_example_desk", true)
	require.Contains(t, n, "this directory maps to no project account _example_desk may reach")
	require.Contains(t, n, "It works only in the projects the owner gives it (project_list shows them)")
	require.False(t, regexp.MustCompile(`\b(open|confidential|sealed|locked)\b`).MatchString(n), n)
}
