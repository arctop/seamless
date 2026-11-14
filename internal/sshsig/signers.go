package sshsig

import (
	"encoding/base64"
	"fmt"
	"slices"
	"strings"
)

// MaxSignersBytes bounds an allowed_signers file. Two keys take a few
// hundred bytes.
const MaxSignersBytes = 64 << 10

// Signer is one line of an allowed_signers file.
type Signer struct {
	// Principals are the line's identities (its first field, split on
	// commas). Verify accepts a listed key whatever its principals; they
	// name the signer in what a caller reports.
	Principals []string
	// Namespaces are the namespaces="..." option's patterns, nil when the
	// line names none (which allows every namespace, as in ssh-keygen).
	Namespaces []string
	// Key is the line's public key.
	Key *PublicKey
	// Line is the line's number in the file, from 1.
	Line int
}

// AllowsNamespace reports whether the line allows a signature made for
// namespace: no namespaces option at all, or a pattern list that matches it
// (ssh_config(5) PATTERNS: * and ?, and a !negated pattern that matches
// denies, whatever else matches).
func (s Signer) AllowsNamespace(namespace string) bool {
	if s.Namespaces == nil {
		return true
	}
	return matchPatternList(namespace, s.Namespaces)
}

// ParseAllowedSigners parses an allowed_signers file (ssh-keygen(1), ALLOWED
// SIGNERS): one signer per line, "principals [options] keytype base64-key
// [comment]", with blank lines and # comments skipped. Every line must
// parse; a file with none is not an error here (the caller decides).
//
// The options are taken deliberately, not passed over:
//
//   - namespaces="..." limits the line to those namespaces;
//   - cert-authority is refused: a release is signed by a listed key, never
//     by a certificate a listed CA issued;
//   - valid-after and valid-before are refused: this package keeps no clock
//     a caller could trust for them, so a key that should stop being trusted
//     is removed from the file instead;
//   - verify-required is refused: it is no allowed_signers option at all
//     (ssh-keygen rejects the line), and a caller that wants user
//     verification asks for it from the signature's flags (Policy);
//   - any other option is refused, as ssh-keygen refuses it.
func ParseAllowedSigners(data []byte) ([]Signer, error) {
	if len(data) > MaxSignersBytes {
		return nil, fmt.Errorf("%w: %d bytes is larger than %d", ErrSigners, len(data), MaxSignersBytes)
	}
	var out []Signer
	for i, line := range strings.Split(strings.ReplaceAll(string(data), "\r\n", "\n"), "\n") {
		n := i + 1
		line = strings.TrimSpace(line)
		if line == "" || strings.HasPrefix(line, "#") {
			continue
		}
		s, err := parseSignerLine(line)
		if err != nil {
			return nil, fmt.Errorf("%w: line %d: %w", ErrSigners, n, err)
		}
		s.Line = n
		out = append(out, s)
	}
	return out, nil
}

// parseSignerLine parses one non-comment line.
func parseSignerLine(line string) (Signer, error) {
	principals, rest, err := nextField(line)
	if err != nil {
		return Signer{}, fmt.Errorf("principals: %w", err)
	}
	if strings.HasPrefix(principals, `"`) {
		// A quoted principals field: the quotes are not part of it.
		if principals, err = unquote(principals); err != nil {
			return Signer{}, fmt.Errorf("principals: %w", err)
		}
	}
	var s Signer
	for p := range strings.SplitSeq(principals, ",") {
		if p == "" {
			return Signer{}, fmt.Errorf("an empty principal in %q", principals)
		}
		s.Principals = append(s.Principals, p)
	}
	field, rest, err := nextField(rest)
	if err != nil {
		return Signer{}, fmt.Errorf("after the principals: %w", err)
	}
	if field == "" {
		return Signer{}, fmt.Errorf("no key after the principals %q", principals)
	}
	// The options field is optional: a field that is not a key type is it.
	keyType := field
	if !isKeyTypeName(field) {
		if err := s.parseOptions(field); err != nil {
			return Signer{}, err
		}
		if keyType, rest, err = nextField(rest); err != nil {
			return Signer{}, fmt.Errorf("key type: %w", err)
		}
		if keyType == "" {
			return Signer{}, fmt.Errorf("no key after the options %q", field)
		}
	}
	b64, _, err := nextField(rest)
	if err != nil {
		return Signer{}, fmt.Errorf("key: %w", err)
	}
	if b64 == "" {
		return Signer{}, fmt.Errorf("no base64 key after the key type %q", keyType)
	}
	if !slices.Contains(KeyTypes, keyType) {
		return Signer{}, fmt.Errorf("unsupported key type %q (supported: %s)", keyType, strings.Join(KeyTypes, ", "))
	}
	blob, err := base64.StdEncoding.Strict().DecodeString(b64)
	if err != nil {
		return Signer{}, fmt.Errorf("the %s key's base64: %w", keyType, err)
	}
	key, err := ParsePublicKey(blob)
	if err != nil {
		return Signer{}, err
	}
	if key.keyType != keyType {
		return Signer{}, fmt.Errorf("the line names key type %q, but the key is a %s key", keyType, key.keyType)
	}
	s.Key = key
	return s, nil
}

// isKeyTypeName reports whether field reads as a key type rather than an
// options field: one of KeyTypes, or a type this package does not support
// (so the line is refused as such, not misread as options).
func isKeyTypeName(field string) bool {
	if slices.Contains(KeyTypes, field) {
		return true
	}
	if strings.Contains(field, "=") || strings.Contains(field, ",") {
		return false
	}
	return strings.HasPrefix(field, "ssh-") || strings.HasPrefix(field, "ecdsa-") ||
		strings.HasPrefix(field, "sk-") || strings.HasPrefix(field, "rsa-") ||
		strings.HasSuffix(field, "@openssh.com")
}

// parseOptions reads a comma-separated options field.
func (s *Signer) parseOptions(field string) error {
	opts, err := splitOptions(field)
	if err != nil {
		return err
	}
	for _, o := range opts {
		name, value, hasValue := strings.Cut(o, "=")
		switch strings.ToLower(name) {
		case "namespaces":
			if !hasValue {
				return fmt.Errorf("namespaces needs a value: namespaces=\"...\"")
			}
			v, err := unquote(value)
			if err != nil {
				return fmt.Errorf("namespaces: %w", err)
			}
			if s.Namespaces != nil {
				return fmt.Errorf("namespaces is given twice")
			}
			for ns := range strings.SplitSeq(v, ",") {
				if ns == "" {
					return fmt.Errorf("an empty namespace in %q", v)
				}
				s.Namespaces = append(s.Namespaces, ns)
			}
		case "cert-authority":
			return fmt.Errorf("cert-authority is not accepted: list the signing keys themselves")
		case "valid-after", "valid-before":
			return fmt.Errorf("%s is not accepted: remove a key from the file to stop trusting it", strings.ToLower(name))
		case "verify-required":
			return fmt.Errorf("verify-required is not an allowed_signers option (ssh-keygen refuses the line); user verification is read from the signature's flags")
		default:
			return fmt.Errorf("unknown option %q", name)
		}
	}
	return nil
}

// nextField splits off s's first whitespace-separated field, where a double
// quote runs to the next one (whitespace and commas inside stay in the
// field). rest has its leading whitespace removed.
func nextField(s string) (field, rest string, err error) {
	s = strings.TrimLeft(s, " \t")
	quoted := false
	for i, c := range s {
		switch {
		case c == '"':
			quoted = !quoted
		case (c == ' ' || c == '\t') && !quoted:
			return s[:i], strings.TrimLeft(s[i:], " \t"), nil
		}
	}
	if quoted {
		return "", "", fmt.Errorf("an unterminated quote in %q", s)
	}
	return s, "", nil
}

// splitOptions splits an options field on the commas outside quotes.
func splitOptions(field string) ([]string, error) {
	var out []string
	quoted := false
	start := 0
	for i, c := range field {
		switch {
		case c == '"':
			quoted = !quoted
		case c == ',' && !quoted:
			out = append(out, field[start:i])
			start = i + 1
		}
	}
	if quoted {
		return nil, fmt.Errorf("an unterminated quote in the options %q", field)
	}
	out = append(out, field[start:])
	for _, o := range out {
		if o == "" {
			return nil, fmt.Errorf("an empty option in %q", field)
		}
	}
	return out, nil
}

// unquote strips the double quotes around an option's value, which must be
// there.
func unquote(v string) (string, error) {
	if len(v) < 2 || v[0] != '"' || v[len(v)-1] != '"' || strings.Contains(v[1:len(v)-1], `"`) {
		return "", fmt.Errorf("the value %s must be one double-quoted string", v)
	}
	return v[1 : len(v)-1], nil
}

// matchPatternList is OpenSSH's match_pattern_list over a parsed list: true
// when some pattern matches s and no negated (!) pattern does.
func matchPatternList(s string, patterns []string) bool {
	matched := false
	for _, p := range patterns {
		if neg, ok := strings.CutPrefix(p, "!"); ok {
			if matchPattern(s, neg) {
				return false
			}
			continue
		}
		if matchPattern(s, p) {
			matched = true
		}
	}
	return matched
}

// matchPattern matches s against a glob of * (any run) and ? (any one
// character), case-sensitively, as OpenSSH's match_pattern does.
func matchPattern(s, p string) bool {
	for len(p) > 0 {
		switch p[0] {
		case '*':
			for len(p) > 0 && p[0] == '*' {
				p = p[1:]
			}
			if p == "" {
				return true
			}
			for i := 0; i <= len(s); i++ {
				if matchPattern(s[i:], p) {
					return true
				}
			}
			return false
		case '?':
			if s == "" {
				return false
			}
			s, p = s[1:], p[1:]
		default:
			if s == "" || s[0] != p[0] {
				return false
			}
			s, p = s[1:], p[1:]
		}
	}
	return s == ""
}
