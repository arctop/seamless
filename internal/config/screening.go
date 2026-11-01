package config

import (
	"fmt"
	"strings"
)

// DefaultScreeningBlobMinLength is screening.blob_min_length's default: an
// unbroken base64 or hex run at least this long counts as a long encoded blob.
// 200 characters is 150 bytes of base64 payload -- room for a hidden paragraph
// -- while a SHA-256 (64 hex characters), a git object id (40) and the
// 64-character lines of a PEM certificate all stay below it.
const DefaultScreeningBlobMinLength = 200

// Screening blob-length bounds. Below the floor every identifier and hash
// would read as a blob; above the ceiling the screen could never fire on a
// write the MCP transport accepts.
const (
	minScreeningBlobLength = 32
	maxScreeningBlobLength = 1 << 20
)

// Validate rejects a present-but-unusable screening block: a blob length
// outside its bounds, or an allowlist entry that is not a bare host name. A
// URL, a path or a port in the list is a caller mistake that would otherwise
// match nothing and quietly report every URL as unlisted.
func (s Screening) Validate() error {
	if s.BlobMinLength < minScreeningBlobLength || s.BlobMinLength > maxScreeningBlobLength {
		return fmt.Errorf("config: screening.blob_min_length must be in [%d, %d]",
			minScreeningBlobLength, maxScreeningBlobLength)
	}
	for _, h := range s.URLAllowlist {
		if err := validAllowlistHost(h); err != nil {
			return fmt.Errorf("config: screening.url_allowlist: %q %w", h, err)
		}
	}
	return nil
}

// validAllowlistHost accepts a bare host name, optionally written with a
// leading "*." or "." (both mean "and its subdomains", which every entry
// already does).
func validAllowlistHost(h string) error {
	host := strings.TrimPrefix(strings.TrimPrefix(strings.TrimSpace(h), "*."), ".")
	switch {
	case host == "":
		return fmt.Errorf("is empty")
	case strings.Contains(host, "://"):
		return fmt.Errorf("is a URL: name the host only (example.com)")
	case strings.ContainsAny(host, "/?#@ \t"):
		return fmt.Errorf("is not a host name: name the host only (example.com)")
	case strings.Contains(host, ":"):
		return fmt.Errorf("carries a port: name the host only (example.com)")
	}
	return nil
}
