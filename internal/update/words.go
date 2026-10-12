package update

import (
	"fmt"
	"strings"
	"time"
)

// The fixed words of automatic updates, for every surface that renders them:
// the briefing notice and the wait reasons in this package, the console's
// Settings > Updates and its banner, and the CLI's doctor row and `seamlessd
// update --check`. A surface frames them -- capitalizes them, sets them in its
// own sentence, appends the versions -- and never restates them, so the
// surfaces cannot drift apart. Every word is fixed or a parsed version
// (constraint update-surfaces-render-parsed-versions-only).

// unsignedWords says why a release without the checksums bundle is skipped,
// after its subject ("v0.7.4 ", "it ").
const unsignedWords = "carries no signed checksums bundle, which an automatic update verifies"

// BlockWords says why automatic updates skip a blocked release (Block.Reason),
// in words that read after "automatic updates skip v0.7.4: ". A reason this
// release does not know (a newer daemon's) reads as a plain failure.
func BlockWords(reason string) string {
	switch reason {
	case BlockRolledBack:
		return "the update to it rolled back"
	case BlockVerify:
		return "it did not pass verification"
	case BlockInstall:
		return fmt.Sprintf("its installer failed %d times in a row", installFailuresToBlock)
	case BlockBroken:
		return "the update to it could not be rolled back cleanly"
	default:
		return "an update to it failed"
	}
}

// PauseWords says why automatic updates paused themselves (Pause.Reason). A
// reason this release does not know reads as the rollbacks.
func PauseWords(reason string) string {
	if reason == PauseBroken {
		return "an update could not be rolled back cleanly"
	}
	return "two updates in a row rolled back"
}

// RefusalWords says why the updater's gates refused an attempt
// (AttemptResult.Refused), in words that read after "it stopped while
// checking it may install now: ". A refusal this release does not know (a
// newer updater's), and none, read "": a plain failure.
func RefusalWords(refusal string) string {
	switch refusal {
	case RefusalNotInstaller:
		return "the install no longer looks like one the installer manages"
	case RefusalStaleBinary:
		return "the installed seamlessd is not the version the daemon runs"
	case RefusalSelfCheck:
		return "the updater could not make sure it runs outside the service, which the installer restarts"
	case RefusalAutoOff:
		return "automatic updates were turned off before it began"
	default:
		return ""
	}
}

// RefusalAction is the owner's action for a gate refusal, ending in the
// command to run, or "" when there is none: automatic updates turned off are
// the owner's own answer, and a refusal this release does not know names no
// action. A surface sets it as the owner's action only while the daemon still
// runs the attempt's From -- once it runs another version, the refusal no
// longer describes this install.
func RefusalAction(refusal string) string {
	switch refusal {
	case RefusalNotInstaller:
		return "restart the service so it re-reads how it was installed: seamlessd restart"
	case RefusalStaleBinary:
		return "restart the service so it runs the installed release: seamlessd restart"
	case RefusalSelfCheck:
		return "update by hand from a terminal: seamlessd update"
	default:
		return ""
	}
}

// SpanWords renders a configured duration -- update.min_age, update.max_defer
// -- for prose: in its largest whole unit while that stays short ("24h",
// "90m", "90s"), otherwise in Go's units with the zero tail dropped
// ("25h30m", not "1530m"); anything finer than a second is Go's own spelling
// ("1m30.5s").
func SpanWords(d time.Duration) string {
	switch {
	case d >= time.Hour && d%time.Hour == 0:
		return fmt.Sprintf("%dh", d/time.Hour)
	case d >= time.Minute && d < 2*time.Hour && d%time.Minute == 0:
		return fmt.Sprintf("%dm", d/time.Minute)
	case d >= time.Second && d < 2*time.Minute && d%time.Second == 0:
		return fmt.Sprintf("%ds", d/time.Second)
	case d >= time.Hour && d%time.Minute == 0:
		return strings.TrimSuffix(d.String(), "0s") // "25h30m0s"
	default:
		return d.String()
	}
}

// UntilWords renders the time left before something due -- a soak's end, a
// deadline, a retry -- for prose after "in" or before "to go": rounded up to
// the minute, so it never reads as sooner than it is, and keeping the next
// unit down above an hour, so 8h58m is not "8h": "22m", "8h 58m", "9h",
// "3d 4h". Anything under a minute, or already due, is "1m". It is the
// forward counterpart of the briefing's floored "5h ago".
func UntilWords(d time.Duration) string {
	mins := max(1, int64((d+time.Minute-1)/time.Minute))
	switch {
	case mins < 60:
		return fmt.Sprintf("%dm", mins)
	case mins < 24*60:
		return unitPair(mins/60, "h", mins%60, "m")
	default:
		hours := (mins + 59) / 60
		return unitPair(hours/24, "d", hours%24, "h")
	}
}

// unitPair renders "8h 58m", or "9h" when the smaller unit is zero.
func unitPair(big int64, bigUnit string, small int64, smallUnit string) string {
	if small == 0 {
		return fmt.Sprintf("%d%s", big, bigUnit)
	}
	return fmt.Sprintf("%d%s %d%s", big, bigUnit, small, smallUnit)
}

// Plural renders a count of a regular noun: "1 request", "3 requests".
func Plural(n int64, noun string) string {
	if n == 1 {
		return "1 " + noun
	}
	return fmt.Sprintf("%d %ss", n, noun)
}

// HeldBack says why automatic updates skip r, a release above the running
// version, soak aside: as a wait code and in words that read after "automatic
// updates skip it: ". First match wins: a hold that reaches r (WaitHeld), a
// block on r (WaitBlocked), no checksums bundle (WaitUnsigned). Both are ""
// when none of them skips r; whether r has soaked is a question for GitHub's
// clock, which only the checker keeps (Target).
func HeldBack(r Release, blocks []Block, hold *Hold) (code, why string) {
	switch b := blocked(blocks, r.Version); {
	case held(hold, r.Version):
		return WaitHeld, fmt.Sprintf("this install went back from v%s, so automatic updates skip releases up to v%s until they are resumed",
			hold.From, hold.Through)
	case b != nil:
		return WaitBlocked, BlockWords(b.Reason)
	case !r.ChecksumsBundle:
		return WaitUnsigned, "it " + unsignedWords
	}
	return "", ""
}
