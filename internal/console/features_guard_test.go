package console

// Cross-package guard tests for the optional-feature registry, console half.
//
// internal/features is a leaf, so the assertions that tie a registry entry to a
// real console surface live here. features_test.go asserts the BEHAVIOUR of the
// research feature's gates against named paths; these tests assert the
// STRUCTURAL correspondence, derived from the registry, so a second optional
// feature -- or a new screen under an existing one -- is covered the day it is
// registered.

import (
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"slices"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/features"
)

// featuresAll builds a config with every registered feature switched the same
// way, so these guards scale to a second optional feature with no edit here.
func featuresAll(on bool) config.Features {
	var c config.Features
	for _, f := range features.Registry() {
		f.Set(&c, on)
	}
	return c
}

// probeChild is a path segment no handler can mistake for real data. It exists
// to discover whether a registry route prefix has CHILD routes registered under
// it (a detail page), so those are gate-checked too.
const probeChild = "registry-guard-probe"

// Invariant 4: every RoutePrefixes entry names a route this console actually
// registers -- and that route, plus any child route under it, is gated.
//
// Both halves matter. A prefix pointing at nothing is a registry entry that
// gates air (the Settings copy and the docs would promise a screen disappears
// while it happily serves), and a registered child that is not wrapped in
// s.gate is the leak the prefix was supposed to close.
func TestFeatureRoutePrefixes_AreRegisteredAndGated(t *testing.T) {
	_, muxOn := newConsoleFeatures(t, featuresAll(true))
	_, muxOff := newConsoleFeatures(t, featuresAll(false))
	// The styled-404 catch-all answers every /console/ path no route claims:
	// a probe child that reaches it means the prefix has no detail route.
	_, catchAll := muxOn.Handler(httptest.NewRequest(http.MethodGet, "/console/"+probeChild, nil))
	require.NotEmpty(t, catchAll)

	for _, f := range features.Registry() {
		for _, prefix := range f.RoutePrefixes {
			require.True(t, strings.HasPrefix(prefix, "/console/"),
				"feature %q lists route prefix %q: console route prefixes are absolute paths under /console/",
				f.Key, prefix)

			for _, path := range []string{prefix, prefix + "/" + probeChild} {
				_, pattern := muxOn.Handler(httptest.NewRequest(http.MethodGet, path, nil))
				if path != prefix && (pattern == "" || pattern == catchAll) {
					continue // this prefix simply has no detail route; nothing to gate
				}
				require.NotEqual(t, catchAll, pattern,
					"feature %q lists route prefix %q, but only the styled-404 catch-all answers it", f.Key, prefix)
				require.NotEmpty(t, pattern,
					"feature %q lists route prefix %q, but Register mounts no handler there: "+
						"either the screen was renamed or removed (drop the prefix from the registry "+
						"entry in internal/features/features.go) or the route was never registered.",
					f.Key, prefix)

				body := getPeek(t, muxOff, path).Body.String()
				require.Contains(t, body, `data-feature-off="`+string(f.Key)+`"`,
					"%s (pattern %q) serves its own content while feature %q is off: "+
						"wrap the handler in gated(features.%s, ...) in Register, or the screen the "+
						"registry claims to hide stays reachable.",
					path, pattern, f.Key, titleWord(string(f.Key)))

				require.NotContains(t, getPeek(t, muxOn, path).Body.String(), "data-feature-off",
					"%s (pattern %q) still answers with the switched-off page while feature %q is ON: "+
						"enabling a feature must restore every screen it owns", path, pattern, f.Key)
			}
		}
	}
}

// gatedRoutePattern pulls the route pattern out of a Register line.
var gatedRoutePattern = regexp.MustCompile(`"(?:GET|POST) (/[^"]+)"`)

// Invariant 4, the other direction: every route Register gates is covered by a
// registry RoutePrefixes entry.
//
// Without this, a new gated screen is invisible to everything that reads the
// registry -- the Settings "what this hides" line, the docs page, and the guard
// above -- so the owner is told a feature hides two screens while it hides three.
// The scan is over the registration site itself, because there is no way to
// enumerate an http.ServeMux's patterns after the fact.
func TestGatedRoutes_AreCoveredByARegistryRoutePrefix(t *testing.T) {
	const registerSite = "console.go"
	src, err := os.ReadFile(registerSite)
	require.NoError(t, err)

	var prefixes []string
	for _, f := range features.Registry() {
		prefixes = append(prefixes, f.RoutePrefixes...)
	}

	gated := 0
	for line := range strings.SplitSeq(string(src), "\n") {
		if !strings.Contains(line, "gated(features.") {
			continue
		}
		match := gatedRoutePattern.FindStringSubmatch(line)
		require.NotNil(t, match,
			"%s: could not read a route pattern out of a gated registration -- this scan and the "+
				"registration style have drifted apart:\n%s", registerSite, strings.TrimSpace(line))
		gated++

		path := match[1]
		covered := slices.ContainsFunc(prefixes, func(p string) bool {
			return path == p || strings.HasPrefix(path, p+"/")
		})
		require.True(t, covered,
			"%s gates route %q, but no feature's RoutePrefixes covers it (registered prefixes: %v): "+
				"add the prefix to the owning entry in internal/features/features.go, or Settings, the "+
				"docs, and the gate guards will all describe a smaller feature than the one you shipped.",
			registerSite, path, prefixes)
	}
	require.NotZero(t, gated,
		"%s registers no gated routes at all -- either the optional-feature gate was removed or route "+
			"registration moved, and this guard needs to follow it", registerSite)
}

// Invariant 5: the registry's NavIDs and the console's screen registry are the
// same set, and each such screen links into the feature's own routes.
//
// The sidebar is rendered from the screen registry (screens.go), and a screen
// with a Feature leaves it while that feature is off. So the two registries are
// useless apart: a NavID with no screen names nothing the sidebar can hide (the
// Settings "what this hides" line would promise a screen that does not exist),
// and a feature-owned screen missing from NavIDs would vanish from the sidebar
// while every registry reader -- Settings, the docs -- says the feature owns
// nothing there.
func TestFeatureNavIDs_MatchTheScreenRegistry(t *testing.T) {
	registered := map[string]features.Key{}
	for _, f := range features.Registry() {
		for _, id := range f.NavIDs {
			registered[id] = f.Key
			sc, ok := screenByID(id)
			require.True(t, ok,
				"feature %q claims nav id %q, but internal/console/screens.go registers no such screen: "+
					"the sidebar cannot hide an entry it never renders. Register the screen with "+
					"Feature: features.%s, or drop the id from the feature's NavIDs.",
				f.Key, id, titleWord(string(f.Key)))
			require.Equal(t, f.Key, sc.Feature,
				"screen %q is claimed by feature %q but its registry entry names feature %q: the sidebar "+
					"would hide it on the wrong switch", id, f.Key, sc.Feature)
			require.Contains(t, f.RoutePrefixes, sc.Href,
				"the %q screen links to %q, which is not one of feature %q's route prefixes (%v): a gated "+
					"nav entry must point into gated routes, or hiding the entry hides nothing.",
				id, sc.Href, f.Key, f.RoutePrefixes)
		}
	}

	for _, sc := range screenRegistry() {
		if sc.Feature == "" {
			continue
		}
		owner, ok := registered[sc.ID]
		require.True(t, ok,
			"screen %q names feature %q, but that feature's NavIDs do not list it, so Settings and the "+
				"docs would describe a feature that hides fewer screens than it does", sc.ID, sc.Feature)
		require.Equal(t, sc.Feature, owner)
	}
}
