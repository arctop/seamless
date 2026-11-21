package console

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// seam-pop animates transform (scale) with fill both, so an element it
// animates keeps the keyframe's transform after the animation ends: a
// transform the element's own rule set to place itself (translateX(-50%) to
// center it) is gone. Such an element places itself with the translate
// property, which composes with the animation's transform. The g-chord hint
// sat with its left edge at the middle of the screen, and the collapsed
// rail's name tag half its height low, until they did.
func TestConsoleCSS_PoppedElementsPlaceThemselvesWithTranslate(t *testing.T) {
	css := string(consoleCSS)
	translateTransform := regexp.MustCompile(`(^|[;{\s])transform:\s*translate`)
	popped := 0
	for _, block := range strings.Split(css, "}") {
		if !strings.Contains(block, "animation:") || !strings.Contains(block, "seam-pop") {
			continue
		}
		popped++
		require.False(t, translateTransform.MatchString(block),
			"a rule animated by seam-pop places itself with transform, which the animation overrides; use the translate property:\n%s", block)
	}
	require.Greater(t, popped, 5, "the seam-pop rules were found")

	for selector, want := range map[string]string{
		".chord-hint {": "translate: -50% 0;",
		".side-tip {":   "translate: 0 -50%;",
	} {
		i := strings.Index(css, selector)
		require.GreaterOrEqual(t, i, 0, selector)
		rule := css[i : i+strings.Index(css[i:], "}")]
		require.Contains(t, rule, want, "%s centers itself with the translate property", selector)
	}
}
