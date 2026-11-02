package commitfeed

import (
	"bytes"
	"encoding/json"
	"fmt"
	"regexp"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"
)

// CheckJSON checks an encoded snapshot against the contract the way its
// consumer reads it, and is what the writer runs before it replaces the file:
// a snapshot the consumer would refuse is never written, so the last good one
// stays in place. now bounds taken_at (at most FutureSkew past it).
//
// The rules: exactly the contract's names in every object (an optional one may
// be absent or null), each value of its type, on its pattern and within its
// length; every time ISO-8601 UTC, a real date, from 1970 on (taken_at from
// 2020 on); no list past its bound; no session id, task id or one entry's
// commit given twice.
func CheckJSON(raw []byte, now time.Time) error {
	dec := json.NewDecoder(bytes.NewReader(raw))
	dec.UseNumber()
	var doc any
	if err := dec.Decode(&doc); err != nil {
		return fmt.Errorf("snapshot: not JSON: %w", err)
	}
	if dec.More() {
		return fmt.Errorf("snapshot: more than one JSON value")
	}
	s, err := object(doc, "snapshot", []string{"kind", "version", "host", "taken_at", "since", "sessions", "tasks"}, nil)
	if err != nil {
		return err
	}
	if k, ok := s["kind"].(string); !ok || k != Kind {
		return fmt.Errorf("snapshot.kind: not %q", Kind)
	}
	if v, ok := s["version"].(json.Number); !ok || v.String() != fmt.Sprint(Version) {
		return fmt.Errorf("snapshot.version: not %d", Version)
	}
	if err := str(s["host"], "snapshot.host", 128, hostRe); err != nil {
		return err
	}
	if err := checkTime(s["taken_at"], "snapshot.taken_at", earliestTaken, now.Add(FutureSkew)); err != nil {
		return err
	}
	if err := checkTime(s["since"], "snapshot.since", epoch, time.Time{}); err != nil {
		return err
	}

	sessions, err := list(s["sessions"], "snapshot.sessions", MaxSessions)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(sessions))
	for i, x := range sessions {
		w := fmt.Sprintf("snapshot.sessions[%d]", i)
		e, err := object(x, w, []string{"id", "repo", "started_at", "ended_at"}, []string{"commits"})
		if err != nil {
			return err
		}
		if err := str(e["id"], w+".id", 128, sessionIDRe); err != nil {
			return err
		}
		if e["repo"] != nil {
			if err := str(e["repo"], w+".repo", 100, repoNameRe); err != nil {
				return err
			}
		}
		if err := checkTime(e["started_at"], w+".started_at", epoch, time.Time{}); err != nil {
			return err
		}
		if e["ended_at"] != nil {
			if err := checkTime(e["ended_at"], w+".ended_at", epoch, time.Time{}); err != nil {
				return err
			}
		}
		if err := commitList(e["commits"], w+".commits"); err != nil {
			return err
		}
		ids = append(ids, e["id"].(string))
	}
	if err := unique(ids, "snapshot.sessions"); err != nil {
		return err
	}

	tasks, err := list(s["tasks"], "snapshot.tasks", MaxTasks)
	if err != nil {
		return err
	}
	ids = ids[:0]
	for i, x := range tasks {
		w := fmt.Sprintf("snapshot.tasks[%d]", i)
		e, err := object(x, w, []string{"id"}, []string{"repo", "commits"})
		if err != nil {
			return err
		}
		if err := str(e["id"], w+".id", 26, ulidRe); err != nil {
			return err
		}
		if e["repo"] != nil {
			if err := str(e["repo"], w+".repo", 100, repoNameRe); err != nil {
				return err
			}
		}
		if err := commitList(e["commits"], w+".commits"); err != nil {
			return err
		}
		ids = append(ids, e["id"].(string))
	}
	return unique(ids, "snapshot.tasks")
}

// FutureSkew is how far past the consumer's clock a taken_at may be.
const FutureSkew = 5 * time.Minute

// The earliest times the contract accepts: any time it carries from 1970, the
// snapshot's own taken_at from 2020.
var (
	epoch         = time.Date(1970, 1, 1, 0, 0, 0, 0, time.UTC)
	earliestTaken = time.Date(2020, 1, 1, 0, 0, 0, 0, time.UTC)
)

// isoUTCRe is an ISO-8601 UTC time as the contract writes one.
var isoUTCRe = regexp.MustCompile(`^\d{4}-\d{2}-\d{2}T\d{2}:\d{2}:\d{2}(?:\.\d{1,9})?Z$`)

// object checks v is an object holding every required name, optionally the
// optional ones, and nothing else. A required name may hold null; whether null
// is allowed is the caller's to judge.
func object(v any, where string, required, optional []string) (map[string]any, error) {
	m, ok := v.(map[string]any)
	if !ok {
		return nil, fmt.Errorf("%s: not an object", where)
	}
	for k := range m {
		if !slices.Contains(required, k) && !slices.Contains(optional, k) {
			return nil, fmt.Errorf("%s: a name the contract does not have", where)
		}
	}
	for _, k := range required {
		if _, ok := m[k]; !ok {
			return nil, fmt.Errorf("%s.%s: missing", where, k)
		}
	}
	return m, nil
}

// str checks v is a string of 1 to max characters, well formed, with no
// control character, on pattern.
func str(v any, where string, max int, pattern *regexp.Regexp) error {
	s, ok := v.(string)
	switch {
	case !ok:
		return fmt.Errorf("%s: not a string", where)
	case s == "":
		return fmt.Errorf("%s: empty", where)
	case utf8.RuneCountInString(s) > max:
		return fmt.Errorf("%s: longer than %d", where, max)
	case !utf8.ValidString(s) || strings.IndexFunc(s, unicode.IsControl) >= 0:
		return fmt.Errorf("%s: not well formed", where)
	case pattern != nil && !pattern.MatchString(s):
		return fmt.Errorf("%s: off its pattern", where)
	}
	return nil
}

// checkTime checks v is an ISO-8601 UTC time naming a real instant (a date
// that does not exist is refused, never rolled over), from earliest on and,
// when notAfter is set, not past it.
func checkTime(v any, where string, earliest, notAfter time.Time) error {
	s, ok := v.(string)
	if !ok || !isoUTCRe.MatchString(s) {
		return fmt.Errorf("%s: not an ISO-8601 UTC time", where)
	}
	t, err := time.Parse(time.RFC3339Nano, s)
	if err != nil || t.UTC().Format("2006-01-02T15:04:05") != s[:19] {
		return fmt.Errorf("%s: not a real time", where)
	}
	if t.Before(earliest) || (!notAfter.IsZero() && t.After(notAfter)) {
		return fmt.Errorf("%s: outside the accepted range", where)
	}
	return nil
}

// list checks v is an array of at most max entries.
func list(v any, where string, max int) ([]any, error) {
	l, ok := v.([]any)
	if !ok {
		return nil, fmt.Errorf("%s: not an array", where)
	}
	if len(l) > max {
		return nil, fmt.Errorf("%s: more than %d entries", where, max)
	}
	return l, nil
}

// commitList checks an entry's optional commits: absent or null, or at most
// MaxRecordedCommits full lowercase commit ids, none twice.
func commitList(v any, where string) error {
	if v == nil {
		return nil
	}
	l, err := list(v, where, maxEntryCommits)
	if err != nil {
		return err
	}
	ids := make([]string, 0, len(l))
	for i, x := range l {
		w := fmt.Sprintf("%s[%d]", where, i)
		if err := str(x, w, 64, commitRe); err != nil {
			return err
		}
		ids = append(ids, x.(string))
	}
	return unique(ids, where)
}

// commitRe is a commit id as the contract carries one.
var commitRe = regexp.MustCompile(`^(?:[0-9a-f]{40}|[0-9a-f]{64})$`)

// unique refuses an id given twice, naming the second.
func unique(ids []string, where string) error {
	seen := make(map[string]bool, len(ids))
	for i, id := range ids {
		if seen[id] {
			return fmt.Errorf("%s[%d]: given twice", where, i)
		}
		seen[id] = true
	}
	return nil
}
