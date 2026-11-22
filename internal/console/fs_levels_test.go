package console

import (
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
)

// UX 35: Experience's level cards show what each level shows or adds as
// chips (its screens, then its Settings sections, each by name, generated
// from the registries) and fold its finer details into a "+N details" hint.

// The card's chips are Items split in two, never a list of their own: the
// screens, then the sections, both from the registries. Items keeps its
// "Settings: ..." entry for levelAdds.
func TestFsLevelCards_ChipsComeFromTheRegistries(t *testing.T) {
	feats := config.Features{Research: true}
	for _, c := range levelCards(feats, levelStandard) {
		want := append([]string(nil), c.Screens...)
		if len(c.Sections) > 0 {
			want = append(want, "Settings: "+joinWithAnd(c.Sections))
		}
		require.Equal(t, want, c.Items, "%s: the chips are the items", c.Level)

		var sections []string
		for _, sec := range settingsSections {
			if sec.Min == c.Level {
				sections = append(sections, sec.Label)
			}
		}
		require.Equal(t, sections, c.Sections, "%s: the sections the sub-nav's registry puts at this level", c.Level)
		for _, s := range c.Screens {
			require.NotContains(t, s, "Settings:", "%s: a screen chip is one screen", c.Level)
		}

		require.Equal(t, levelAddsSurfaces(c.Level), c.Details)
		if len(c.Details) == 0 {
			require.Empty(t, c.DetailsLabel, c.Level)
			require.Empty(t, c.DetailsID, c.Level)
			continue
		}
		require.Equal(t, plural(len(c.Details), "detail", "details"), c.DetailsLabel)
		require.Regexp(t, `^hint-[0-9a-f]{8}$`, c.DetailsID, "an id uniqueHintIDs keeps unique")
	}
	cards := levelCards(feats, levelBasic)
	require.NotEqual(t, cards[1].DetailsID, cards[2].DetailsID, "each level's hint has its own id")
	require.Contains(t, cards[1].Screens, "Labs")
	require.NotContains(t, levelCards(config.Features{}, levelBasic)[1].Screens, "Labs", "a screen whose feature is off is no chip")
}

// The Experience page: three cards in one grid (its rows stretch them to one
// height), the details hints, and Theme and Keyboard as two rows of one card.
// Every hint id on the page is its own.
func TestFsExperienceSection_CardsAndRows(t *testing.T) {
	_, mux := newConsoleLevel(t, config.Features{Research: true}, "advanced")
	page := getHTMLBody(t, mux, "/console/settings?s=experience")

	require.Equal(t, 3, strings.Count(page, `<form class="level-card`))
	require.Equal(t, 2, strings.Count(page, `<li class="level-card-more">`), "Standard and Advanced have details; Basic has none")
	require.Contains(t, page, `<span class="badge ok">In use</span>`)
	require.Equal(t, 1, strings.Count(page, "In use"), "the level in use says so once")
	require.NotContains(t, page, `class="level-card-details"`)
	require.NotContains(t, page, `experience-extras`)

	start := strings.Index(page, `<div class="experience-rows">`)
	require.Positive(t, start, "Theme and Keyboard share one card")
	rows := page[start:]
	rows = rows[:strings.Index(rows, "</section>")]
	require.Equal(t, 2, strings.Count(rows, `<div class="experience-row" role="group"`))
	require.Contains(t, rows, `<span class="experience-row-label" id="experience-theme-title">Theme</span>`)
	require.Contains(t, rows, `data-theme-choice`)
	require.Contains(t, rows, `aria-label="About Theme"`, "the theme's why is a hint")
	require.Contains(t, rows, `<span class="experience-row-label" id="experience-keys-title">Keyboard</span>`)
	require.Contains(t, rows, `data-keys-open`)

	ids := map[string]int{}
	for _, m := range regexp.MustCompile(`id="(hint-[0-9a-f]{8}(?:-\d+)?)"`).FindAllStringSubmatch(page, -1) {
		ids[m[1]]++
	}
	require.NotEmpty(t, ids)
	for id, n := range ids {
		require.Equal(t, 1, n, "hint id %s is drawn once", id)
		require.Equal(t, 1, strings.Count(page, `aria-describedby="`+id+`"`), "hint %s has one button", id)
	}

	css := string(consoleCSS)
	require.Contains(t, css, ".level-cards { display: grid; grid-template-columns: repeat(3, minmax(0, 1fr));",
		"one grid row: the cards stretch to one height")
	require.NotContains(t, css, ".level-cards { align-items")
	for _, gone := range []string{".experience-extras", ".experience-group", "ul.level-card-details"} {
		require.NotContains(t, css, gone, "the old Experience rules left with their markup")
	}
}
