package main

import (
	"bufio"
	"bytes"
	"errors"
	"fmt"
	"os"
	"os/exec"
	"slices"
	"strings"

	"github.com/arctop/seamless/internal/releasesig"
	"github.com/arctop/seamless/internal/sshsig"
	"github.com/arctop/seamless/internal/update"
)

// The owner's signature gate (plan agent-accounts-and-releases, step D).
// CI builds and Sigstore-signs a release into a draft; the owner then signs
// its checksums.txt with a hardware key (scripts/release/sign.sh) and
// uploads checksums.txt.sshsig; the publish job publishes only once
// verify-owner accepts it. Which keys may sign is the repository's
// release_signers file, read from git at two tags:
//
//   - the previous release's tag (the newest vX.Y.Z below this tag's
//     version), and
//   - this tag.
//
// A key must be in both. So a key added in this release -- by anyone who
// could push the commit -- cannot sign the release that adds it; it signs
// from the next one on, after a release the owner did sign has carried it.
// And a key removed in this release stops signing at once. The bootstrap is
// the one exception: when the previous release's tag has no release_signers
// at all (the first signed release), this tag's file alone decides, and the
// tool says so loudly so the owner checks the fingerprints by hand.

// releaseSignersFile is the signers file at the repository root.
const releaseSignersFile = "release_signers"

// signersAt is release_signers as one tag holds it.
type signersAt struct {
	tag     string
	present bool
	signers []sshsig.Signer
}

// tagLister lists the repository's v* tags; readFile reads a file at a tag
// (ok false when the tag holds no such file). Both are git in production
// and fixtures in tests.
type gitSource struct {
	tags     func() ([]string, error)
	readFile func(tag, path string) (data []byte, ok bool, err error)
}

// repoGit reads the repository at root with git.
func repoGit(root string) gitSource {
	return gitSource{
		tags: func() ([]string, error) {
			out, err := gitOut(root, "tag", "-l", "v*")
			if err != nil {
				return nil, err
			}
			return strings.Fields(out), nil
		},
		readFile: func(tag, path string) ([]byte, bool, error) {
			if _, err := gitOut(root, "rev-parse", "--verify", "-q", tag+"^{commit}"); err != nil {
				return nil, false, fmt.Errorf("the tag %s is not in this clone (git fetch --tags): %w", tag, err)
			}
			listed, err := gitOut(root, "ls-tree", "--name-only", tag, "--", path)
			if err != nil {
				return nil, false, err
			}
			if listed == "" {
				return nil, false, nil
			}
			cmd := exec.Command("git", "-C", root, "show", tag+":"+path)
			cmd.Stderr = os.Stderr
			data, err := cmd.Output()
			if err != nil {
				return nil, false, fmt.Errorf("git show %s:%s: %w", tag, path, err)
			}
			return data, true, nil
		},
	}
}

// previousRelease is the release before tag: the newest plain vX.Y.Z tag
// whose version is below tag's own (a prerelease's base version for one).
// Prerelease tags are never "the previous release". ok is false when there
// is none.
func previousRelease(tags []string, tag string) (string, bool, error) {
	version, err := versionOf(tag)
	if err != nil {
		return "", false, err
	}
	base, _, _ := strings.Cut(version, "-")
	cur, ok := update.Parse(base)
	if !ok {
		return "", false, fmt.Errorf("%s is not a release tag", tag)
	}
	var best update.Version
	found := false
	for _, t := range tags {
		v, ok := update.ParseTag(t)
		if !ok || v.Compare(cur) >= 0 {
			continue
		}
		if !found || v.Compare(best) > 0 {
			best, found = v, true
		}
	}
	if !found {
		return "", false, nil
	}
	return "v" + best.String(), true, nil
}

// readSigners reads and parses release_signers at tag.
func (g gitSource) readSigners(tag string) (signersAt, error) {
	data, ok, err := g.readFile(tag, releaseSignersFile)
	if err != nil {
		return signersAt{}, err
	}
	at := signersAt{tag: tag, present: ok}
	if !ok {
		return at, nil
	}
	if at.signers, err = releasesig.ParseOwnerSigners(data); err != nil {
		return signersAt{}, fmt.Errorf("%s at %s: %w", releaseSignersFile, tag, err)
	}
	return at, nil
}

// ownerGate is which keys may sign a release, and why.
type ownerGate struct {
	tag, prev string
	// required is false when no release_signers exists at either tag and
	// the caller did not ask for one: there is nothing to check.
	required bool
	// bootstrap is the first signed release: this tag's keys alone.
	bootstrap bool
	// signers may sign: in both files (or this tag's alone, bootstrapping).
	signers []sshsig.Signer
	// added and removed are this tag's change to the keys.
	added, removed []sshsig.Signer
	cur, before    signersAt
}

// errNoSigners is a release that must be signed with no release_signers to
// hold it to.
var errNoSigners = errors.New("no " + releaseSignersFile + " to hold the owner's signature to")

// decideOwnerGate builds the gate for tag from its signers and the previous
// release's. require refuses a release with no signers file at all.
func decideOwnerGate(tag, prev string, before, cur signersAt, require bool) (ownerGate, error) {
	g := ownerGate{tag: tag, prev: prev, cur: cur, before: before, required: true}
	switch {
	case !before.present && !cur.present:
		if require {
			return ownerGate{}, fmt.Errorf("%s: %w at %s or at the previous release's tag (%s)", tag, errNoSigners, tag, orNone(prev))
		}
		g.required = false
		return g, nil
	case before.present && !cur.present:
		return ownerGate{}, fmt.Errorf("%s removes %s, which %s carries: a release cannot turn the owner's signature off, so keep the file",
			tag, releaseSignersFile, prev)
	case !before.present:
		g.bootstrap = true
		g.signers = ownerOnly(cur.signers)
		g.added = g.signers
		return g, nil
	}
	in := func(list []sshsig.Signer, k *sshsig.PublicKey) bool {
		return slices.ContainsFunc(list, func(s sshsig.Signer) bool {
			return bytes.Equal(s.Key.Marshal(), k.Marshal()) && s.AllowsNamespace(releasesig.OwnerNamespace)
		})
	}
	for _, s := range ownerOnly(cur.signers) {
		if in(before.signers, s.Key) {
			g.signers = append(g.signers, s)
		} else {
			g.added = append(g.added, s)
		}
	}
	for _, s := range ownerOnly(before.signers) {
		if !in(cur.signers, s.Key) {
			g.removed = append(g.removed, s)
		}
	}
	if len(g.signers) == 0 {
		return ownerGate{}, fmt.Errorf("%s at %s and at %s share no key, so no signature can pass: keep at least one key across a release "+
			"(add the new key in one release, remove the old one in the next)", releaseSignersFile, prev, tag)
	}
	return g, nil
}

// ownerOnly keeps the lines that may sign in the release namespace.
func ownerOnly(list []sshsig.Signer) []sshsig.Signer {
	var out []sshsig.Signer
	for _, s := range list {
		if s.AllowsNamespace(releasesig.OwnerNamespace) {
			out = append(out, s)
		}
	}
	return out
}

func orNone(prev string) string {
	if prev == "" {
		return "none"
	}
	return prev
}

func describeSigner(s sshsig.Signer) string {
	return strings.Join(s.Principals, ",") + " " + s.Key.Fingerprint()
}

// notes is what the gate prints before it checks anything: which keys may
// sign, and this tag's change to them.
func (g ownerGate) notes() []string {
	if !g.required {
		return []string{fmt.Sprintf("no %s at %s or %s: this release is not held to an owner's signature", releaseSignersFile, g.tag, orNone(g.prev))}
	}
	var out []string
	if g.bootstrap {
		out = append(out, fmt.Sprintf("BOOTSTRAP: the previous release (%s) has no %s, so %s's own file alone decides who signs it. "+
			"Check each fingerprint below against the keys themselves (ssh-keygen -l -f <key>.pub) before you trust it.",
			orNone(g.prev), releaseSignersFile, g.tag))
	}
	for _, s := range g.signers {
		out = append(out, "may sign: "+describeSigner(s))
	}
	if !g.bootstrap {
		for _, s := range g.added {
			out = append(out, fmt.Sprintf("ADDED at %s, so it signs from the next release on, not this one: %s", g.tag, describeSigner(s)))
		}
		for _, s := range g.removed {
			out = append(out, fmt.Sprintf("REMOVED at %s, so it no longer signs: %s", g.tag, describeSigner(s)))
		}
	}
	return out
}

// verify checks the owner's signature over checksums against the gate. A
// signature by a key this tag added, or removed, is refused with that said.
func (g ownerGate) verify(checksums, sig []byte) (sshsig.Verified, error) {
	v, err := releasesig.VerifyOwner(g.signers, checksums, sig)
	if err == nil {
		return v, nil
	}
	if !errors.Is(err, sshsig.ErrUnlisted) {
		return sshsig.Verified{}, err
	}
	parsed, perr := sshsig.ParseSignature(sig)
	if perr != nil {
		return sshsig.Verified{}, err
	}
	match := func(list []sshsig.Signer) (sshsig.Signer, bool) {
		i := slices.IndexFunc(list, func(s sshsig.Signer) bool { return bytes.Equal(s.Key.Marshal(), parsed.Key.Marshal()) })
		if i < 0 {
			return sshsig.Signer{}, false
		}
		return list[i], true
	}
	if s, ok := match(g.added); ok {
		return sshsig.Verified{}, fmt.Errorf("%w: %s was added to %s at %s, so it may sign only from the next release on -- sign with a key %s already lists",
			err, describeSigner(s), releaseSignersFile, g.tag, g.prev)
	}
	if s, ok := match(g.removed); ok {
		return sshsig.Verified{}, fmt.Errorf("%w: %s was removed from %s at %s -- sign with a key the release keeps",
			err, describeSigner(s), releaseSignersFile, g.tag)
	}
	return sshsig.Verified{}, err
}

// loadOwnerGate reads the gate for tag from the repository.
func loadOwnerGate(g gitSource, tag string, require bool) (ownerGate, error) {
	tags, err := g.tags()
	if err != nil {
		return ownerGate{}, err
	}
	if !slices.Contains(tags, tag) {
		return ownerGate{}, fmt.Errorf("the tag %s is not in this clone (git fetch --tags)", tag)
	}
	prev, hasPrev, err := previousRelease(tags, tag)
	if err != nil {
		return ownerGate{}, err
	}
	cur, err := g.readSigners(tag)
	if err != nil {
		return ownerGate{}, err
	}
	before := signersAt{}
	if hasPrev {
		if before, err = g.readSigners(prev); err != nil {
			return ownerGate{}, err
		}
	}
	return decideOwnerGate(tag, prev, before, cur, require)
}

// checkDraft holds a draft's checksums.txt, before the owner signs it, to
// what the owner can vouch for: its Sigstore bundle by this exact tag's
// release workflow (verifyBundle), and its darwin lines to the manifest in
// the owner's signed tag -- every manifest asset listed with its sha256,
// and no darwin asset the manifest does not name. It returns the names the
// file lists.
func checkDraft(cfg grConfig, tag string, rawTag, checksums, bundle []byte, verifyBundle func(bundle, artifact []byte, tag string) error) ([]string, error) {
	version, err := versionOf(tag)
	if err != nil {
		return nil, err
	}
	if err := verifyBundle(bundle, checksums, tag); err != nil {
		return nil, fmt.Errorf("checksums.txt is not %s's: %w", tag, err)
	}
	manifest, err := parseManifest(rawTag)
	if err != nil {
		return nil, err
	}
	listed := map[string]string{}
	var names []string
	sc := bufio.NewScanner(bytes.NewReader(checksums))
	for n := 1; sc.Scan(); n++ {
		line := sc.Text()
		if line == "" {
			continue
		}
		m := manifestLineRe.FindStringSubmatch(line)
		if m == nil {
			return nil, fmt.Errorf("checksums.txt line %d is not a sha256sum line: %q", n, line)
		}
		if _, dup := listed[m[2]]; dup {
			return nil, fmt.Errorf("checksums.txt lists %s twice", m[2])
		}
		listed[m[2]] = m[1]
		names = append(names, m[2])
	}
	if err := sc.Err(); err != nil {
		return nil, err
	}
	darwin, err := cfg.darwinArchiveNames(version)
	if err != nil {
		return nil, err
	}
	darwin = append(darwin, approverAsset(version))
	var errs []error
	for _, name := range sortedKeys(manifest) {
		switch got, ok := listed[name]; {
		case !ok:
			errs = append(errs, fmt.Errorf("checksums.txt does not list %s, which the tag's manifest names", name))
		case got != manifest[name]:
			errs = append(errs, fmt.Errorf("checksums.txt lists %s as %s, the tag's manifest says %s", name, got, manifest[name]))
		}
	}
	for _, name := range names {
		if _, ok := manifest[name]; !ok && (slices.Contains(darwin, name) || strings.Contains(name, "darwin")) {
			errs = append(errs, fmt.Errorf("checksums.txt lists %s, a macOS asset the tag's manifest does not name", name))
		}
	}
	if len(errs) > 0 {
		return nil, errors.Join(errs...)
	}
	return names, nil
}
