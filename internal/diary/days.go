package diary

import (
	"errors"
	"fmt"
	"time"
)

// ErrInvalidDay is a day or a month that does not parse.
var ErrInvalidDay = errors.New("diary: invalid day")

// Day and month keys: the console's ?day= and ?m= forms.
const (
	dayLayout   = "2006-01-02"
	monthLayout = "2006-01"
)

// ParseDay parses a YYYY-MM-DD day in the daemon's zone, to its midnight.
func ParseDay(s string) (time.Time, error) {
	t, err := time.ParseInLocation(dayLayout, s, time.Local)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w %q: expected YYYY-MM-DD", ErrInvalidDay, s)
	}
	return t, nil
}

// ParseMonth parses a YYYY-MM month in the daemon's zone, to its first
// midnight.
func ParseMonth(s string) (time.Time, error) {
	t, err := time.ParseInLocation(monthLayout, s, time.Local)
	if err != nil {
		return time.Time{}, fmt.Errorf("%w %q: expected YYYY-MM", ErrInvalidDay, s)
	}
	return t, nil
}

// midnight floors t to its midnight in loc.
func midnight(t time.Time, loc *time.Location) time.Time {
	l := t.In(loc)
	return time.Date(l.Year(), l.Month(), l.Day(), 0, 0, 0, 0, loc)
}

// dayBounds is the local day holding t: its midnight and the next, found with
// AddDate (a daylight-saving day is 23 or 25 hours long).
func dayBounds(t time.Time, loc *time.Location) (lo, hi time.Time) {
	lo = midnight(t, loc)
	return lo, lo.AddDate(0, 0, 1)
}

// dayKey is a local day's YYYY-MM-DD.
func dayKey(t time.Time, loc *time.Location) string { return t.In(loc).Format(dayLayout) }

// hourStarts lists the starts of the local hours of [lo, hi): one an hour
// from lo, so a day of 23 or 25 hours has 23 or 25 (every real zone moves its
// clocks by whole quarter-hours, and a quarter-hour bucket never straddles
// one of these hours).
func hourStarts(lo, hi time.Time) []time.Time {
	var out []time.Time
	for t := lo; t.Before(hi); t = t.Add(time.Hour) {
		out = append(out, t)
	}
	return out
}

// hourOf is the index in hourStarts(lo, hi) of the hour holding t, clamped to
// the n hours of the day.
func hourOf(t, lo time.Time, n int) int {
	i := int(t.Sub(lo) / time.Hour)
	return min(max(i, 0), n-1)
}

// dayStarts lists the midnights of the local days of [lo, hi), lo a midnight.
func dayStarts(lo, hi time.Time) []time.Time {
	var out []time.Time
	for t := lo; t.Before(hi); t = t.AddDate(0, 0, 1) {
		out = append(out, t)
	}
	return out
}

// dayIndex is the index of the day of starts (ascending midnights, the last
// day ending at hi) holding t, or -1 outside [starts[0], hi).
func dayIndex(starts []time.Time, hi, t time.Time) int {
	if len(starts) == 0 || t.Before(starts[0]) || !t.Before(hi) {
		return -1
	}
	lo, top := 0, len(starts)-1
	for lo < top {
		mid := (lo + top + 1) / 2
		if t.Before(starts[mid]) {
			top = mid - 1
		} else {
			lo = mid
		}
	}
	return lo
}
