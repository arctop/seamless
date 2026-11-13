package main

import (
	"bufio"
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
)

// The macOS manifest: the sha256 of every darwin asset the owner built,
// signed and notarized, one sha256sum line each ("<hex>  <name>", the format
// checksums.txt uses), carried in the message of the release's signed,
// annotated tag. Only an admin may push a v* tag (the tag ruleset), so the
// message is the owner's word for which bytes the release ships for macOS;
// release.yml holds the draft release's darwin assets to it before they
// enter checksums.txt.

// manifestLineRe is one manifest line.
var manifestLineRe = regexp.MustCompile(`^([0-9a-f]{64})  ([A-Za-z0-9._+-]+)$`)

// hexLeadRe is a line that begins like a manifest line: anything starting
// with 64 hex digits must be one exactly, not prose to skip.
var hexLeadRe = regexp.MustCompile(`^[0-9A-Fa-f]{64}`)

// fileSHA256 is the lowercase hex SHA-256 of the file at p.
func fileSHA256(p string) (sum string, err error) {
	f, err := os.Open(p)
	if err != nil {
		return "", err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
	}()
	h := sha256.New()
	if _, err := io.Copy(h, f); err != nil {
		return "", err
	}
	return hex.EncodeToString(h.Sum(nil)), nil
}

// writeManifest writes the manifest lines for paths, sorted by name.
func writeManifest(w io.Writer, paths []string) error {
	type line struct{ name, sum string }
	var lines []line
	seen := map[string]bool{}
	for _, p := range paths {
		name := filepath.Base(p)
		if !manifestLineRe.MatchString(strings.Repeat("0", 64) + "  " + name) {
			return fmt.Errorf("%s is not a name a manifest can carry", name)
		}
		if seen[name] {
			return fmt.Errorf("%s is named twice", name)
		}
		seen[name] = true
		sum, err := fileSHA256(p)
		if err != nil {
			return err
		}
		lines = append(lines, line{name, sum})
	}
	slices.SortFunc(lines, func(a, b line) int { return strings.Compare(a.name, b.name) })
	for _, l := range lines {
		if _, err := fmt.Fprintf(w, "%s  %s\n", l.sum, l.name); err != nil {
			return err
		}
	}
	return nil
}

// tagMessage is the message of a tag object as `git cat-file tag` prints it:
// everything after the header's blank line, up to the signature block. Text
// that is not a tag object is taken as the message itself.
func tagMessage(raw []byte) []byte {
	msg := raw
	if bytes.HasPrefix(raw, []byte("object ")) {
		_, after, found := bytes.Cut(raw, []byte("\n\n"))
		if !found {
			return nil
		}
		msg = after
	}
	if i := bytes.Index(msg, []byte("\n-----BEGIN ")); i >= 0 {
		msg = msg[:i+1]
	} else if bytes.HasPrefix(msg, []byte("-----BEGIN ")) {
		msg = nil
	}
	return msg
}

// parseManifest reads the manifest lines out of a tag message (or a whole
// tag object). Other lines are prose and skipped, but a line that starts
// with 64 hex digits must be a well-formed manifest line, and no name may
// appear twice.
func parseManifest(raw []byte) (map[string]string, error) {
	out := map[string]string{}
	sc := bufio.NewScanner(bytes.NewReader(tagMessage(raw)))
	for n := 1; sc.Scan(); n++ {
		line := strings.TrimRight(sc.Text(), "\r")
		if m := manifestLineRe.FindStringSubmatch(line); m != nil {
			if _, dup := out[m[2]]; dup {
				return nil, fmt.Errorf("the tag's manifest names %s twice", m[2])
			}
			out[m[2]] = m[1]
			continue
		}
		if hexLeadRe.MatchString(line) {
			return nil, fmt.Errorf("the tag's message line %d is not a manifest line (<sha256>  <name>): %q", n, line)
		}
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	return out, nil
}

// checkDarwin holds a draft release's assets, downloaded into dir, to the
// manifest in the release tag's message: the tag must carry a manifest; it
// may name only this version's darwin archives and Touch ID approver, and
// must name every darwin archive; dir must hold exactly the files it names,
// each with the sha256 it gives. It returns the checked files' paths, sorted.
func checkDarwin(cfg grConfig, tag string, rawTag []byte, dir string) ([]string, error) {
	version, err := versionOf(tag)
	if err != nil {
		return nil, err
	}
	manifest, err := parseManifest(rawTag)
	if err != nil {
		return nil, err
	}
	if len(manifest) == 0 {
		return nil, fmt.Errorf("the message of tag %s carries no macOS manifest (scripts/release/macos.sh writes it)", tag)
	}
	required, err := cfg.darwinArchiveNames(version)
	if err != nil {
		return nil, err
	}
	allowed := append(slices.Clone(required), approverAsset(version))
	var errs []error
	for _, name := range sortedKeys(manifest) {
		if !slices.Contains(allowed, name) {
			errs = append(errs, fmt.Errorf("the tag's manifest names %s, which is no macOS asset of %s", name, tag))
		}
	}
	for _, name := range required {
		if _, ok := manifest[name]; !ok {
			errs = append(errs, fmt.Errorf("the tag's manifest does not name %s", name))
		}
	}

	entries, err := os.ReadDir(dir)
	if err != nil {
		return nil, err
	}
	have := map[string]bool{}
	for _, e := range entries {
		if !e.Type().IsRegular() {
			errs = append(errs, fmt.Errorf("the draft's %s is not a regular file", e.Name()))
			continue
		}
		have[e.Name()] = true
		if _, ok := manifest[e.Name()]; !ok {
			errs = append(errs, fmt.Errorf("the draft holds %s, which the tag's manifest does not name", e.Name()))
		}
	}
	var checked []string
	for _, name := range sortedKeys(manifest) {
		if !have[name] {
			errs = append(errs, fmt.Errorf("the draft lacks %s, which the tag's manifest names", name))
			continue
		}
		p := filepath.Join(dir, name)
		sum, err := fileSHA256(p)
		if err != nil {
			return nil, err
		}
		if sum != manifest[name] {
			errs = append(errs, fmt.Errorf("the draft's %s has sha256 %s, the tag's manifest says %s", name, sum, manifest[name]))
			continue
		}
		checked = append(checked, p)
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return checked, nil
}

func sortedKeys(m map[string]string) []string {
	keys := make([]string, 0, len(m))
	for k := range m {
		keys = append(keys, k)
	}
	slices.Sort(keys)
	return keys
}
