package retrieve

import (
	"context"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/store"
)

// conventionsSectionOf extracts the briefing's Conventions section body: the
// lines between its header and the blank line before the next section (or the
// rest of the briefing when it is the last section).
func conventionsSectionOf(b string) string {
	_, after, ok := strings.Cut(b, "Conventions (project-local choices):\n")
	if !ok {
		return ""
	}
	sec, _, _ := strings.Cut(after, "\n\n")
	return sec
}

// fullConventionLines counts the full-tier bullets in a briefing's Conventions
// section (the closing count line is not a bullet).
func fullConventionLines(b string) int {
	n := 0
	for line := range strings.SplitSeq(conventionsSectionOf(b), "\n") {
		if strings.HasPrefix(line, "- ") {
			n++
		}
	}
	return n
}

// The CONVENTION section tiers at convention_max_full: the top K conventions
// (recency order with the utility gate closed) render as full lines above the
// memory index, the rest stay behind the count line, and only the rendered
// lines' ids reach the read-after-inject funnel -- the hidden ones are named
// nowhere, so recording them would fake exposure.
func TestConventionSectionTierAndCount(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	require.NoError(t, store.SetSetting(ctx, db, store.SettingRepoProjectMap, `{"/w":"p"}`))

	now := time.Now()
	insMemAt(t, db, "01RUL", "constraint", "hard-rule", "the one rule", "p", now.Add(-1*time.Hour))
	insMemAt(t, db, "01CVA", "convention", "conv-a", "layout fact a", "p", now.Add(-1*time.Hour))
	insMemAt(t, db, "01CVB", "convention", "conv-b", "layout fact b", "p", now.Add(-2*time.Hour))
	insMemAt(t, db, "01CVC", "convention", "conv-c", "layout fact c", "p", now.Add(-3*time.Hour))
	insMemAt(t, db, "01CVD", "convention", "conv-d", "layout fact d", "p", now.Add(-4*time.Hour))

	svc := New(db, nil, budgets(), nil)
	svc.SetBriefingConfig(briefingWith(func(b *config.Briefing) { b.ConventionMaxFull = 2 }))

	b, ids, err := svc.Briefing(ctx, BriefingInput{CWD: "/w", Source: "startup"})
	require.NoError(t, err)
	require.Equal(t, 2, fullConventionLines(b), "exactly K full lines")
	require.Contains(t, b, "- conv-a: layout fact a")
	require.Contains(t, b, "- conv-b: layout fact b")
	require.Contains(t, b, "(4 total, 2 shown -- recall kind=convention for the rest)")
	require.NotContains(t, b, "conv-c", "hidden conventions are named nowhere")
	require.NotContains(t, b, "conv-d")
	require.Subset(t, ids, []string{"01CVA", "01CVB"})
	require.NotContains(t, ids, "01CVC", "hidden conventions must not fake funnel exposure")
	require.NotContains(t, ids, "01CVD")

	// Conventions leave the memory index entirely and render in their own
	// section; the Constraints section is untouched.
	require.NotContains(t, b, "Memories (p):", "no index section when conventions absorbed everything")
	require.Contains(t, b, "- hard-rule: the one rule")
	require.Equal(t, 1, strings.Count(b, "conv-a:"), "a convention renders exactly once")
}

// convention_max_full=0 disables the tiering, matching ConstraintMaxFull
// semantics: every convention renders full and the count line still closes the
// section with the retrieval mechanism.
func TestConventionTierZeroDisables(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	require.NoError(t, store.SetSetting(ctx, db, store.SettingRepoProjectMap, `{"/w":"p"}`))

	now := time.Now()
	insMemAt(t, db, "01CVA", "convention", "conv-a", "layout fact a", "p", now.Add(-1*time.Hour))
	insMemAt(t, db, "01CVB", "convention", "conv-b", "layout fact b", "p", now.Add(-2*time.Hour))
	insMemAt(t, db, "01CVC", "convention", "conv-c", "layout fact c", "p", now.Add(-3*time.Hour))

	svc := New(db, nil, budgets(), nil)
	svc.SetBriefingConfig(briefingWith(func(b *config.Briefing) { b.ConventionMaxFull = 0 }))

	b, ids, err := svc.Briefing(ctx, BriefingInput{CWD: "/w", Source: "startup"})
	require.NoError(t, err)
	require.Equal(t, 3, fullConventionLines(b))
	require.Contains(t, b, "(3 total -- recall kind=convention lists them)")
	require.Subset(t, ids, []string{"01CVA", "01CVB", "01CVC"})
}

// A starred convention keeps the favorite pin inside its own section: it
// sorts to the section head, renders exactly once, always makes the full tier
// even past ConventionMaxFull, and survives a starved budget -- the render
// guarantee the old FAVORITE head line used to provide.
func TestConventionStarredPinsInSection(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	require.NoError(t, store.SetSetting(ctx, db, store.SettingRepoProjectMap, `{"/w":"p"}`))

	now := time.Now()
	// Starred but older, so recency alone would sort it last.
	insMemAt(t, db, "01STA", "convention", "starred-conv", "starred layout fact", "p", now.Add(-3*time.Hour))
	insMemAt(t, db, "01PLA", "convention", "plain-conv", "plain layout fact", "p", now.Add(-2*time.Hour))
	_, err := db.ExecContext(ctx, `UPDATE memories_index SET favorite = 1 WHERE id = '01STA'`)
	require.NoError(t, err)

	svc := New(db, nil, budgets(), nil)
	svc.SetBriefingConfig(briefingWith(func(b *config.Briefing) { b.ConventionMaxFull = 4 }))

	b, ids, err := svc.Briefing(ctx, BriefingInput{CWD: "/w", Source: "startup"})
	require.NoError(t, err)
	require.NotContains(t, b, "FAVORITE:", "no separate starred section")
	require.Contains(t, b, "- starred-conv: starred layout fact")
	require.Equal(t, 1, strings.Count(b, "starred-conv"), "starred convention renders once, in its section")
	require.Equal(t, 2, fullConventionLines(b))
	require.Less(t, strings.Index(b, "- starred-conv"), strings.Index(b, "- plain-conv"),
		"star sorts to the section head past recency")
	require.Contains(t, b, "(2 total -- recall kind=convention lists them)")
	require.Subset(t, ids, []string{"01STA", "01PLA"})

	// The star overrides the tier split: with room for only one full line the
	// starred convention takes it and the fresher plain one is demoted.
	svc.SetBriefingConfig(briefingWith(func(b *config.Briefing) { b.ConventionMaxFull = 1 }))
	b, ids, err = svc.Briefing(ctx, BriefingInput{CWD: "/w", Source: "startup"})
	require.NoError(t, err)
	require.Contains(t, b, "- starred-conv: starred layout fact")
	require.NotContains(t, b, "- plain-conv")
	require.Contains(t, b, "(2 total, 1 shown -- recall kind=convention for the rest)")
	require.Contains(t, ids, "01STA")
	require.NotContains(t, ids, "01PLA")

	// The star also overrides the budget: a starved budget drops the plain
	// convention but never the starred one.
	starved := New(db, nil, config.Budgets{MaxBriefingTokens: 1}, nil)
	starved.SetBriefingConfig(briefingWith(func(b *config.Briefing) { b.ConventionMaxFull = 4 }))
	b, ids, err = starved.Briefing(ctx, BriefingInput{CWD: "/w", Source: "startup"})
	require.NoError(t, err)
	require.Contains(t, b, "- starred-conv: starred layout fact")
	require.NotContains(t, b, "- plain-conv")
	require.Contains(t, ids, "01STA")
	require.NotContains(t, ids, "01PLA")
}

// Conventions are budget-competing: a starved budget drops the full lines, but
// the count line still renders, so the section is never invisible and the pool
// stays one kind-filtered recall away.
func TestConventionCountLineSurvivesBudget(t *testing.T) {
	db := setupDB(t)
	ctx := context.Background()
	require.NoError(t, store.SetSetting(ctx, db, store.SettingRepoProjectMap, `{"/w":"p"}`))

	now := time.Now()
	insMemAt(t, db, "01CVA", "convention", "conv-a", strings.Repeat("wide description ", 30), "p", now.Add(-1*time.Hour))
	insMemAt(t, db, "01CVB", "convention", "conv-b", strings.Repeat("wide description ", 30), "p", now.Add(-2*time.Hour))

	svc := New(db, nil, config.Budgets{MaxBriefingTokens: 1}, nil)
	svc.SetBriefingConfig(briefingWith(func(b *config.Briefing) { b.ConventionMaxFull = 2 }))

	b, ids, err := svc.Briefing(ctx, BriefingInput{CWD: "/w", Source: "startup"})
	require.NoError(t, err)
	require.Equal(t, 0, fullConventionLines(b), "budget drops every full line")
	require.Contains(t, b, "(2 total, 0 shown -- recall kind=convention for the rest)")
	require.NotContains(t, ids, "01CVA")
	require.NotContains(t, ids, "01CVB")
}

// The count line's hint is a command that lists the whole pool: past the
// listing's default limit a bare "recall kind=convention" returns only 10, so
// the hint names the limit, capped where the listing's own more note takes over.
func TestConventionCountLine_HintListsTheWholePool(t *testing.T) {
	require.Equal(t, "(4 total, 2 shown -- recall kind=convention for the rest)\n", conventionCountLine(4, 2))
	require.Equal(t, "(10 total -- recall kind=convention lists them)\n", conventionCountLine(10, 10))
	require.Equal(t, "(18 total, 3 shown -- recall kind=convention limit=18 for the rest)\n", conventionCountLine(18, 3))
	require.Equal(t, "(150 total, 0 shown -- recall kind=convention limit=100 for the rest)\n", conventionCountLine(150, 0))
}
