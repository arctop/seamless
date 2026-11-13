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
//	releasekit cask           -tag vX.Y.Z -checksums FILE -bundle FILE
//	                                                         print the Homebrew cask of a published release
package main

import (
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

const usage = "usage: releasekit darwin-build|darwin-archive|manifest|check-darwin|cask [flags]"

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

	case "cask":
		checksums := fs.String("checksums", "", "the release's checksums.txt")
		bundle := fs.String("bundle", "", "the release's checksums.txt.sigstore.json")
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
