package config

import (
	"fmt"
	"strings"
	"time"

	"gopkg.in/yaml.v3"
)

// Duration is a time.Duration that the YAML file and SEAMLESS_* environment
// overrides spell as a Go duration string ("6h", "90m", "1h30m").
//
// A config key cannot simply be typed time.Duration: yaml.v3 refuses every
// bare integer into one, so `min_age: 0` -- the one bare number that is
// unambiguous -- would not load. Typed as a plain int64 it is worse: "6h" is
// refused and `check_interval: 6` loads as six nanoseconds without a word.
// Duration reads the strings, accepts a bare zero, and refuses every other
// bare number, naming the line that holds it.
type Duration time.Duration

// durationHint is the shape every duration error asks for.
const durationHint = "want a Go duration such as 6h, 90m or 1h30m"

// ParseDuration parses a configured duration: a Go duration string ("6h",
// "90m", "1h30m", "0s") or a bare 0. Surrounding whitespace is trimmed.
//
// Any other bare integer is an error rather than a guess at its unit: "30"
// could mean seconds, minutes, or hours, and the wrong guess is a check every
// 30 seconds that should have run every 30 hours. Zero is the same in every
// unit, so it alone may go bare (in any spelling whose value is zero: "0",
// "00", "-0"). Empty and negative values are errors too.
func ParseDuration(s string) (Duration, error) {
	t := strings.TrimSpace(s)
	if t == "" {
		return 0, fmt.Errorf("invalid duration %q: %s", s, durationHint)
	}
	if isInt, zero := bareInteger(t); isInt {
		if zero {
			return 0, nil
		}
		return 0, needsUnit(t)
	}
	d, err := time.ParseDuration(t)
	if err != nil {
		return 0, fmt.Errorf("invalid duration %q: %w", t, err)
	}
	if d < 0 {
		return 0, fmt.Errorf("invalid duration %q: must not be negative", t)
	}
	return Duration(d), nil
}

// needsUnit is the error for a bare non-zero integer, shared by ParseDuration
// and the YAML !!int path so both read the same.
func needsUnit(s string) error {
	return fmt.Errorf("invalid duration %q: needs a unit, e.g. 24h", s)
}

// bareInteger reports whether s is an optionally signed run of ASCII digits,
// and whether its value is zero.
func bareInteger(s string) (isInt, zero bool) {
	if s != "" && (s[0] == '+' || s[0] == '-') {
		s = s[1:]
	}
	if s == "" {
		return false, false
	}
	zero = true
	for i := range len(s) {
		switch c := s[i]; {
		case c < '0' || c > '9':
			return false, false
		case c != '0':
			zero = false
		}
	}
	return true, zero
}

// Std returns d as a time.Duration.
func (d Duration) Std() time.Duration { return time.Duration(d) }

// String renders d compactly: time.Duration's form with zero trailing units
// dropped, so 6h0m0s reads "6h", 1h30m0s "1h30m", and 2m0s "2m". Zero is "0s".
// The result parses back to d through ParseDuration.
func (d Duration) String() string {
	s := time.Duration(d).String()
	if strings.HasSuffix(s, "m0s") {
		s = strings.TrimSuffix(s, "0s")
	}
	if strings.HasSuffix(s, "h0m") {
		s = strings.TrimSuffix(s, "0m")
	}
	return s
}

// UnmarshalYAML reads a duration from a YAML scalar. A string goes through
// ParseDuration; a YAML integer is accepted only when it is zero, so a bare
// `6` is refused exactly like the string "6". Floats, booleans, sequences,
// mappings, and any other tag are errors naming the line.
//
// An explicit null never arrives here: yaml.v3 handles null before consulting
// an unmarshaler (and leaves the field as it was), and LoadFrom refuses an
// explicit null for every key before decoding (explicitNullPath).
func (d *Duration) UnmarshalYAML(n *yaml.Node) error {
	if n.Kind == yaml.ScalarNode {
		switch n.ShortTag() {
		case "!!str":
			v, err := ParseDuration(n.Value)
			if err != nil {
				return fmt.Errorf("line %d: %w", n.Line, err)
			}
			*d = v
			return nil
		case "!!int":
			// YAML has already read a number, so the only question is whether it is
			// zero. A value too large for int64 fails to decode and is, by
			// construction, not zero.
			var i int64
			if err := n.Decode(&i); err == nil && i == 0 {
				*d = 0
				return nil
			}
			return fmt.Errorf("line %d: %w", n.Line, needsUnit(n.Value))
		}
	}
	return fmt.Errorf("line %d: %s is not a duration: %s", n.Line, describeNode(n), durationHint)
}

// MarshalYAML writes d in the compact form String returns, which
// UnmarshalYAML reads back.
func (d Duration) MarshalYAML() (any, error) { return d.String(), nil }

// MarshalText writes d in the compact form String returns, so JSON (a stored
// console override, store.BackupOverride) carries "24h" rather than a count of
// nanoseconds. YAML keeps using MarshalYAML, which yaml.v3 prefers.
func (d Duration) MarshalText() ([]byte, error) { return []byte(d.String()), nil }

// UnmarshalText reads a duration through ParseDuration, the rules the file and
// the environment follow: "24h" or a bare "0", never a bare "30". A JSON
// number is refused by encoding/json before it gets here, so a stored row can
// never be read as nanoseconds. YAML keeps using UnmarshalYAML, which yaml.v3
// prefers.
func (d *Duration) UnmarshalText(b []byte) error {
	v, err := ParseDuration(string(b))
	if err != nil {
		return err
	}
	*d = v
	return nil
}

// describeNode names a YAML node for an error message: a scalar by its tag and
// value, a collection by its kind.
func describeNode(n *yaml.Node) string {
	switch n.Kind {
	case yaml.ScalarNode:
		return n.ShortTag() + " " + n.Value
	case yaml.SequenceNode:
		return "a sequence"
	case yaml.MappingNode:
		return "a mapping"
	default:
		return "this value"
	}
}
