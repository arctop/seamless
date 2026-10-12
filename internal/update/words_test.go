package update

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

// The shared words, pinned: the briefing, the console and the CLI all render
// these strings, so a change here is a change on every surface.
func TestBlockWords(t *testing.T) {
	for reason, want := range map[string]string{
		BlockRolledBack: "the update to it rolled back",
		BlockVerify:     "it did not pass verification",
		BlockInstall:    "its installer failed 3 times in a row",
		BlockBroken:     "the update to it could not be rolled back cleanly",
		"a_newer_word":  "an update to it failed",
		"":              "an update to it failed",
	} {
		require.Equal(t, want, BlockWords(reason), reason)
	}
}

func TestPauseWords(t *testing.T) {
	require.Equal(t, "two updates in a row rolled back", PauseWords(PauseRollbacks))
	require.Equal(t, "an update could not be rolled back cleanly", PauseWords(PauseBroken))
	require.Equal(t, "two updates in a row rolled back", PauseWords("a_newer_word"))
}

func TestSpanWords(t *testing.T) {
	for d, want := range map[time.Duration]string{
		24 * time.Hour:   "24h",
		36 * time.Hour:   "36h",
		90 * time.Minute: "90m",
		90 * time.Second: "90s",
		2 * time.Minute:  "2m",
		// Past what the largest whole unit says briefly: hours and the rest,
		// not "1530m" or "150s".
		25*time.Hour + 30*time.Minute: "25h30m",
		2*time.Hour + 30*time.Minute:  "2h30m",
		150 * time.Second:             "2m30s",
		// Finer than the units above: Go's own spelling.
		1500 * time.Millisecond:               "1.5s",
		90*time.Second + 500*time.Millisecond: "1m30.5s",
		time.Hour + 30*time.Second:            "1h0m30s",
		0:                                     "0s",
	} {
		require.Equal(t, want, SpanWords(d), d.String())
	}
}

// A span still to go rounds up, so it never reads as sooner than it is, and
// keeps the next unit down above an hour: a soak with 8h58m left once read
// "in 8h".
func TestUntilWords(t *testing.T) {
	for d, want := range map[time.Duration]string{
		-time.Minute:                 "1m", // already due
		0:                            "1m",
		10 * time.Second:             "1m",
		time.Minute:                  "1m",
		time.Minute + time.Second:    "2m",
		22 * time.Minute:             "22m",
		59*time.Minute + time.Second: "1h",
		90 * time.Minute:             "1h 30m",
		8*time.Hour + 57*time.Minute + 30*time.Second: "8h 58m",
		9 * time.Hour:                 "9h",
		23*time.Hour + 59*time.Minute: "23h 59m",
		24 * time.Hour:                "1d",
		24*time.Hour + time.Minute:    "1d 1h",
		50 * time.Hour:                "2d 2h",
		7 * 24 * time.Hour:            "7d",
	} {
		require.Equal(t, want, UntilWords(d), d.String())
	}
}

// The refusal words, pinned like the rest: the console's Last attempt row and
// banner and the CLI's doctor row render them. Each action ends in the
// command to run; a refusal this release does not know reads as a plain
// failure, with neither words nor an action.
func TestRefusalWordsAndAction(t *testing.T) {
	for refusal, want := range map[string][2]string{
		RefusalNotInstaller: {"the install no longer looks like one the installer manages",
			"restart the service so it re-reads how it was installed: seamlessd restart"},
		RefusalStaleBinary: {"the installed seamlessd is not the version the daemon runs",
			"restart the service so it runs the installed release: seamlessd restart"},
		RefusalSelfCheck: {"the updater could not make sure it runs outside the service, which the installer restarts",
			"update by hand from a terminal: seamlessd update"},
		RefusalAutoOff: {"automatic updates were turned off before it began", ""},
		"disk_full":    {"", ""},
		"":             {"", ""},
	} {
		require.Equal(t, want[0], RefusalWords(refusal), refusal)
		require.Equal(t, want[1], RefusalAction(refusal), refusal)
	}
	for _, r := range refusals {
		require.NotEmpty(t, RefusalWords(r), "every refusal this release writes has words: %s", r)
	}
}

func TestPlural(t *testing.T) {
	require.Equal(t, "1 failure", Plural(1, "failure"))
	require.Equal(t, "0 failures", Plural(0, "failure"))
	require.Equal(t, "3 failures", Plural(3, "failure"))
}

// TestHeldBack: the skips, soak aside, in the order they win, each with the
// words a surface sets after "automatic updates skip it: ".
func TestHeldBack(t *testing.T) {
	now := time.Date(2026, 10, 9, 12, 0, 0, 0, time.UTC)
	r := rel("0.7.4", now.Add(-time.Minute), false) // unsigned, and not soaked on any clock
	hold := &Hold{Through: ver("0.7.4"), From: ver("0.7.3")}
	blocks := []Block{{Version: ver("0.7.4"), Reason: BlockVerify}}

	code, why := HeldBack(r, blocks, hold)
	require.Equal(t, WaitHeld, code, "a hold outranks a block")
	require.Equal(t, "this install went back from v0.7.3, so automatic updates skip releases up to v0.7.4 until they are resumed", why)

	code, why = HeldBack(r, blocks, &Hold{Through: ver("0.7.3"), From: ver("0.7.3")})
	require.Equal(t, WaitBlocked, code, "a hold below r does not reach it; the block outranks the missing bundle")
	require.Equal(t, BlockWords(BlockVerify), why)

	code, why = HeldBack(r, []Block{{Version: ver("0.7.5"), Reason: BlockVerify}}, nil)
	require.Equal(t, WaitUnsigned, code)
	require.Equal(t, "it carries no signed checksums bundle, which an automatic update verifies", why)

	r.ChecksumsBundle = true
	code, why = HeldBack(r, nil, nil)
	require.Empty(t, code, "a soak is not HeldBack's to judge")
	require.Empty(t, why)
}

// TestBlockWords_InstallCountsInstallerFailuresOnly is why the CLI's old
// wording for BlockInstall, "its installer failed on every try", was wrong and
// BlockWords' count is right: a try that stops before the installer runs (a
// failed download, say) neither counts toward the block nor resets it, so a
// release is blocked after tries that did not all fail at the installer.
func TestBlockWords_InstallCountsInstallerFailuresOnly(t *testing.T) {
	var st State
	applyOutcome(&st, folded(WhyAuto, "0.7.3", OutcomeFailed, StageInstall), attemptNow, true)
	applyOutcome(&st, folded(WhyAuto, "0.7.3", OutcomeFailed, StageFetch), attemptNow, true)
	applyOutcome(&st, folded(WhyAuto, "0.7.3", OutcomeInterrupted, StageFetch), attemptNow, true)
	for range installFailuresToBlock - 1 {
		applyOutcome(&st, folded(WhyAuto, "0.7.3", OutcomeFailed, StageInstall), attemptNow, true)
	}
	require.Len(t, st.Blocks, 1)
	require.Equal(t, BlockInstall, st.Blocks[0].Reason, "five tries, two of them never reached the installer")
	require.Equal(t, "its installer failed 3 times in a row", BlockWords(st.Blocks[0].Reason))
}
