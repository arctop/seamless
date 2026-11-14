// releasekit is the release's own tooling for the macOS half that leaves CI:
// the owner's Mac builds, signs and notarizes the darwin binaries and the
// Touch ID approver (scripts/release/macos.sh), and CI holds what it uploaded
// to the tag's draft release to the manifest in the signed tag before
// goreleaser lists it in checksums.txt (.github/workflows/release.yml).
// After the release publishes, scripts/release/cask.sh renders the Homebrew
// cask with it. Run it from the repository root:
//
//	releasekit darwin-build   -tag vX.Y.Z -out DIR           build the darwin binaries (.goreleaser.yaml's builds)
//	releasekit darwin-archive -tag vX.Y.Z -bin DIR -out DIR  pack the signed binaries (.goreleaser.yaml's archive)
//	releasekit manifest       FILE...                        print the sha256 lines for the tag's message
//	releasekit check-darwin   -tag vX.Y.Z -tag-object FILE -assets DIR
//	                                                         hold the draft's darwin assets to the tag's manifest
//	releasekit check-draft    -tag vX.Y.Z -tag-object FILE -checksums FILE -bundle FILE
//	                                                         hold a draft's checksums.txt to its Sigstore bundle and the tag's manifest
//	releasekit owner-keys     -tag vX.Y.Z [-require]         print the keys that may sign the release (release_signers)
//	releasekit verify-owner   -tag vX.Y.Z -checksums FILE -sig FILE [-require]
//	                                                         check the owner's checksums.txt.sshsig against them
//	releasekit cask           -tag vX.Y.Z -checksums FILE -bundle FILE [-owner-sig FILE]
//	                                                         print the Homebrew cask of a published release
//
// The owner's signature (check-draft, owner-keys, verify-owner) is step D of
// plan agent-accounts-and-releases: CI leaves the release a draft, the owner
// signs its checksums.txt with a hardware key (scripts/release/sign.sh), and
// the publish job publishes only once verify-owner accepts it (owner.go).
package main

import (
	"crypto/sha256"
	"encoding/hex"
	"flag"
	"fmt"
	"io"
	"os"
	"strings"

	"github.com/arctop/seamless/internal/releasesig"
	"github.com/arctop/seamless/internal/update"
)

func main() {
	if err := run(os.Args[1:], os.Stdout); err != nil {
		fmt.Fprintf(os.Stderr, "releasekit: %v\n", err)
		os.Exit(1)
	}
}

const usage = "usage: releasekit darwin-build|darwin-archive|manifest|check-darwin|check-draft|owner-keys|verify-owner|cask [flags]"

func run(args []string, stdout io.Writer) error {
	if len(args) == 0 {
		return fmt.Errorf("%s", usage)
	}
	cmd, args := args[0], args[1:]
	fs := flag.NewFlagSet("releasekit "+cmd, flag.ContinueOnError)
	root := fs.String("root", ".", "the repository root (where .goreleaser.yaml is)")
	tag := fs.String("tag", "", "the release tag, vX.Y.Z or vX.Y.Z-pre")
	switch cmd {
	case "darwin-build":
		out := fs.String("out", "", "directory for the binaries: <out>/<build id>_darwin_<arch>/<binary>")
		if err := parse(fs, args, false, "tag", "out"); err != nil {
			return err
		}
		cfg, r, err := load(*root, *tag)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "releasekit: building %s (commit %s) with %s\n", r.Tag, r.Short, r.Toolchain)
		built, err := darwinBuild(cfg, r, *out)
		if err != nil {
			return err
		}
		for _, b := range built {
			if _, err := fmt.Fprintln(stdout, b.Path); err != nil {
				return err
			}
		}
		return nil

	case "darwin-archive":
		bin := fs.String("bin", "", "the directory darwin-build wrote (binaries signed in place)")
		out := fs.String("out", "", "directory for the archives")
		if err := parse(fs, args, false, "tag", "bin", "out"); err != nil {
			return err
		}
		cfg, r, err := load(*root, *tag)
		if err != nil {
			return err
		}
		written, err := darwinArchive(cfg, r, *bin, *out)
		if err != nil {
			return err
		}
		for _, p := range written {
			if _, err := fmt.Fprintln(stdout, p); err != nil {
				return err
			}
		}
		return nil

	case "manifest":
		if err := parse(fs, args, true); err != nil {
			return err
		}
		if fs.NArg() == 0 {
			return fmt.Errorf("manifest: name the asset files")
		}
		return writeManifest(stdout, fs.Args())

	case "check-darwin":
		tagObject := fs.String("tag-object", "", "the tag object, as `git cat-file tag <tag>` prints it")
		assets := fs.String("assets", "", "directory holding every asset of the tag's draft release")
		if err := parse(fs, args, false, "tag", "tag-object", "assets"); err != nil {
			return err
		}
		cfg, err := loadGoreleaser(*root)
		if err != nil {
			return err
		}
		raw, err := os.ReadFile(*tagObject)
		if err != nil {
			return err
		}
		checked, err := checkDarwin(cfg, *tag, raw, *assets)
		if err != nil {
			return err
		}
		for _, p := range checked {
			if _, err := fmt.Fprintln(stdout, p); err != nil {
				return err
			}
		}
		return nil

	case "check-draft":
		tagObject := fs.String("tag-object", "", "the tag object, as `git cat-file tag <tag>` prints it")
		checksums := fs.String("checksums", "", "the draft's checksums.txt")
		bundle := fs.String("bundle", "", "the draft's checksums.txt.sigstore.json")
		if err := parse(fs, args, false, "tag", "tag-object", "checksums", "bundle"); err != nil {
			return err
		}
		cfg, err := loadGoreleaser(*root)
		if err != nil {
			return err
		}
		files, err := readFiles(*tagObject, *checksums, *bundle)
		if err != nil {
			return err
		}
		names, err := checkDraft(cfg, *tag, files[0], files[1], files[2], releasesig.VerifyAssetForTag)
		if err != nil {
			return err
		}
		fmt.Fprintf(os.Stderr, "releasekit: checksums.txt (sha256 %s) is %s's, Sigstore-signed by its release workflow, and its macOS lines are the tag's manifest\n",
			sha256Hex(files[1]), *tag)
		for _, n := range names {
			if _, err := fmt.Fprintln(stdout, n); err != nil {
				return err
			}
		}
		return nil

	case "owner-keys":
		require := fs.Bool("require", false, "refuse a release with no release_signers at this tag or the previous release's")
		if err := parse(fs, args, false, "tag"); err != nil {
			return err
		}
		g, err := loadOwnerGate(repoGit(*root), *tag, *require)
		if err != nil {
			return err
		}
		for _, n := range g.notes() {
			if _, err := fmt.Fprintln(stdout, n); err != nil {
				return err
			}
		}
		return nil

	case "verify-owner":
		checksums := fs.String("checksums", "", "the release's checksums.txt")
		sig := fs.String("sig", "", "the owner's signature over it, checksums.txt.sshsig")
		require := fs.Bool("require", false, "refuse a release with no release_signers at this tag or the previous release's")
		if err := parse(fs, args, false, "tag", "checksums", "sig"); err != nil {
			return err
		}
		g, err := loadOwnerGate(repoGit(*root), *tag, *require)
		if err != nil {
			return err
		}
		for _, n := range g.notes() {
			fmt.Fprintln(os.Stderr, "releasekit: "+n)
		}
		if !g.required {
			return nil
		}
		files, err := readFiles(*checksums, *sig)
		if err != nil {
			return err
		}
		v, err := g.verify(files[0], files[1])
		if err != nil {
			return err
		}
		_, err = fmt.Fprintf(stdout, "checksums.txt (sha256 %s) is signed by %s\n", sha256Hex(files[0]), releasesig.Describe(v))
		return err

	case "cask":
		checksums := fs.String("checksums", "", "the release's checksums.txt")
		bundle := fs.String("bundle", "", "the release's checksums.txt.sigstore.json")
		ownerSig := fs.String("owner-sig", "", "the release's checksums.txt.sshsig; required once release_signers exists at the tag")
		if err := parse(fs, args, false, "tag", "checksums", "bundle"); err != nil {
			return err
		}
		cfg, err := loadGoreleaser(*root)
		if err != nil {
			return err
		}
		sums, err := os.ReadFile(*checksums)
		if err != nil {
			return err
		}
		b, err := os.ReadFile(*bundle)
		if err != nil {
			return err
		}
		// The checksums the cask pins must be the release's own: the bundle
		// is checked against this tag's exact signing identity, the check
		// the updater makes (internal/releasesig).
		v, ok := update.ParseTag(*tag)
		if !ok {
			return fmt.Errorf("%s is not a published release's tag (vX.Y.Z); a prerelease gets no cask", *tag)
		}
		if err := releasesig.Embedded()(b, sums, v); err != nil {
			return fmt.Errorf("checksums.txt is not %s's: %w", *tag, err)
		}
		// And the owner's, once the repository names the owner's keys: the
		// same gate the publish job holds the release to.
		g, err := loadOwnerGate(repoGit(*root), *tag, false)
		if err != nil {
			return err
		}
		if g.required {
			if *ownerSig == "" {
				return fmt.Errorf("%s has %s, so the cask needs the release's %s (-owner-sig)", *tag, releaseSignersFile, update.OwnerSignatureAsset)
			}
			sig, err := os.ReadFile(*ownerSig)
			if err != nil {
				return err
			}
			ov, err := g.verify(sums, sig)
			if err != nil {
				return err
			}
			fmt.Fprintf(os.Stderr, "releasekit: checksums.txt is signed by %s\n", releasesig.Describe(ov))
		} else {
			fmt.Fprintf(os.Stderr, "releasekit: %s predates the owner's signature (no %s); Sigstore alone vouches for it\n", *tag, releaseSignersFile)
		}
		cask, err := renderCask(cfg, *tag, sums)
		if err != nil {
			return err
		}
		_, err = io.WriteString(stdout, cask)
		return err
	}
	return fmt.Errorf("unknown command %q; %s", cmd, usage)
}

// parse parses args, then refuses positional arguments where none belong
// and any of the required flags left empty.
func parse(fs *flag.FlagSet, args []string, positional bool, required ...string) error {
	if err := fs.Parse(args); err != nil {
		return err
	}
	if !positional && fs.NArg() > 0 {
		return fmt.Errorf("%s takes no arguments, got %q", fs.Name(), fs.Args())
	}
	var missing []string
	for _, n := range required {
		if f := fs.Lookup(n); f == nil || f.Value.String() == "" {
			missing = append(missing, "-"+n)
		}
	}
	if len(missing) > 0 {
		return fmt.Errorf("%s needs %s", fs.Name(), strings.Join(missing, " "))
	}
	return nil
}

// readFiles reads each path, in order.
func readFiles(paths ...string) ([][]byte, error) {
	out := make([][]byte, len(paths))
	for i, p := range paths {
		b, err := os.ReadFile(p)
		if err != nil {
			return nil, err
		}
		out[i] = b
	}
	return out, nil
}

// sha256Hex is the lowercase hex SHA-256 of b.
func sha256Hex(b []byte) string {
	s := sha256.Sum256(b)
	return hex.EncodeToString(s[:])
}

func load(root, tag string) (grConfig, release, error) {
	cfg, err := loadGoreleaser(root)
	if err != nil {
		return grConfig{}, release{}, err
	}
	r, err := readRelease(root, tag)
	if err != nil {
		return grConfig{}, release{}, err
	}
	return cfg, r, nil
}
