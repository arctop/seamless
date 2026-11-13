package main

import (
	"archive/tar"
	"compress/gzip"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"os"
	"os/exec"
	"path"
	"path/filepath"
	"regexp"
	"slices"
	"strconv"
	"strings"
	"time"
)

// tagRe is a release tag: v, a version, and an optional semver prerelease.
var tagRe = regexp.MustCompile(`^v([0-9]+\.[0-9]+\.[0-9]+(?:-[0-9A-Za-z.-]+)?)$`)

// release is what a build stamps: the tag being cut and the commit it names.
type release struct {
	Tag, Version      string
	FullCommit, Short string
	CommitTime        time.Time
	Date              string // goreleaser's .Date: the build's start, RFC 3339 UTC
	Toolchain         string // GOTOOLCHAIN: go.mod's toolchain, what CI's setup-go installs
	repoRoot          string
}

// versionOf is the version a release tag names, without its v.
func versionOf(tag string) (string, error) {
	m := tagRe.FindStringSubmatch(tag)
	if m == nil {
		return "", fmt.Errorf("%q is not a release tag (vX.Y.Z or vX.Y.Z-pre)", tag)
	}
	return m[1], nil
}

// readRelease reads the commit to build from the repository at root (HEAD)
// and the toolchain go.mod names.
func readRelease(root, tag string) (release, error) {
	version, err := versionOf(tag)
	if err != nil {
		return release{}, err
	}
	r := release{Tag: tag, Version: version, repoRoot: root, Date: time.Now().UTC().Format(time.RFC3339)}
	if r.FullCommit, err = gitOut(root, "rev-parse", "HEAD"); err != nil {
		return release{}, err
	}
	if r.Short, err = gitOut(root, "rev-parse", "--short", "HEAD"); err != nil {
		return release{}, err
	}
	ct, err := gitOut(root, "log", "-1", "--format=%ct", "HEAD")
	if err != nil {
		return release{}, err
	}
	secs, err := strconv.ParseInt(ct, 10, 64)
	if err != nil {
		return release{}, fmt.Errorf("commit time %q: %w", ct, err)
	}
	r.CommitTime = time.Unix(secs, 0).UTC()
	if r.Toolchain, err = goModToolchain(root); err != nil {
		return release{}, err
	}
	return r, nil
}

func gitOut(root string, args ...string) (string, error) {
	cmd := exec.Command("git", append([]string{"-C", root}, args...)...)
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("git %s: %w", strings.Join(args, " "), err)
	}
	return strings.TrimSpace(string(out)), nil
}

// goModToolchain is the toolchain go.mod asks for: its toolchain line, else
// its go line. CI's setup-go (go-version-file: go.mod) installs the same one,
// so the darwin binaries are built by the Go the linux and windows ones are.
func goModToolchain(root string) (string, error) {
	cmd := exec.Command("go", "mod", "edit", "-json")
	cmd.Dir = root
	cmd.Stderr = os.Stderr
	out, err := cmd.Output()
	if err != nil {
		return "", fmt.Errorf("go mod edit -json: %w", err)
	}
	var mod struct {
		Go        string
		Toolchain string
	}
	if err := json.Unmarshal(out, &mod); err != nil {
		return "", fmt.Errorf("go mod edit -json: %w", err)
	}
	return toolchainOf(mod.Go, mod.Toolchain)
}

func toolchainOf(goLine, toolchainLine string) (string, error) {
	switch {
	case toolchainLine != "":
		return toolchainLine, nil
	case goLine != "":
		return "go" + goLine, nil
	}
	return "", errors.New("go.mod names no Go version")
}

// templateData is what a build's templates may name.
func (r release) templateData() map[string]string {
	return map[string]string{
		"Version":         r.Version,
		"Tag":             r.Tag,
		"ShortCommit":     r.Short,
		"FullCommit":      r.FullCommit,
		"Commit":          r.FullCommit,
		"Date":            r.Date,
		"CommitTimestamp": strconv.FormatInt(r.CommitTime.Unix(), 10),
	}
}

// darwinBinary is one built binary: <out>/<build id>_darwin_<arch>/<binary>.
type darwinBinary struct {
	Build grBuild
	Arch  string
	Path  string
}

// binaryPath is where darwinBuild writes build b for arch under dir, and
// where darwinArchive looks for it.
func binaryPath(dir string, b grBuild, arch string) string {
	return filepath.Join(dir, b.ID+"_darwin_"+arch, b.Binary)
}

// buildCommand is the go build of b for darwin/arch, as goreleaser runs it:
// its flags, its ldflags joined into one -ldflags, its env. The environment
// is the caller's, less every variable that would change the code a build
// produces, which CI's runner does not set either.
func buildCommand(r release, b grBuild, arch, out string) (*exec.Cmd, error) {
	data := r.templateData()
	data["Os"], data["Arch"] = "darwin", arch
	var ldflags []string
	for _, f := range b.Ldflags {
		s, err := render(b.ID+" ldflags", f, data)
		if err != nil {
			return nil, err
		}
		ldflags = append(ldflags, s)
	}
	args := append([]string{"build"}, b.Flags...)
	if len(ldflags) > 0 {
		args = append(args, "-ldflags="+strings.Join(ldflags, " "))
	}
	args = append(args, "-o", out, b.Main)

	var env []string
	for _, kv := range os.Environ() {
		name, _, _ := strings.Cut(kv, "=")
		switch name {
		case "GOOS", "GOARCH", "GOFLAGS", "GOAMD64", "GOARM64", "GOEXPERIMENT", "CGO_ENABLED", "GOTOOLCHAIN", "GOWORK":
			continue
		}
		env = append(env, kv)
	}
	for _, e := range b.Env {
		s, err := render(b.ID+" env", e, data)
		if err != nil {
			return nil, err
		}
		env = append(env, s)
	}
	env = append(env, "GOOS=darwin", "GOARCH="+arch, "GOWORK=off", "GOTOOLCHAIN="+r.Toolchain)
	cmd := exec.Command("go", args...)
	cmd.Dir = r.repoRoot
	cmd.Env = env
	return cmd, nil
}

// darwinBuild builds every build entry for every darwin architecture it
// names into dir, then sets each binary's modification time as
// mod_timestamp says.
func darwinBuild(cfg grConfig, r release, dir string) ([]darwinBinary, error) {
	var built []darwinBinary
	for _, b := range cfg.Builds {
		for _, arch := range darwinArches {
			if !slices.Contains(b.Goarch, arch) {
				continue
			}
			out := binaryPath(dir, b, arch)
			if err := os.MkdirAll(filepath.Dir(out), 0o755); err != nil {
				return nil, err
			}
			cmd, err := buildCommand(r, b, arch, out)
			if err != nil {
				return nil, err
			}
			cmd.Stdout, cmd.Stderr = os.Stderr, os.Stderr
			if err := cmd.Run(); err != nil {
				return nil, fmt.Errorf("go build %s darwin/%s: %w", b.ID, arch, err)
			}
			if b.ModTimestamp != "" {
				data := r.templateData()
				s, err := render(b.ID+" mod_timestamp", b.ModTimestamp, data)
				if err != nil {
					return nil, err
				}
				secs, err := strconv.ParseInt(s, 10, 64)
				if err != nil {
					return nil, fmt.Errorf("%s mod_timestamp %q: %w", b.ID, s, err)
				}
				t := time.Unix(secs, 0)
				if err := os.Chtimes(out, t, t); err != nil {
					return nil, err
				}
			}
			built = append(built, darwinBinary{Build: b, Arch: arch, Path: out})
		}
	}
	return built, nil
}

// archiveEntry is one file an archive holds: its name inside the archive and
// the file it is read from.
type archiveEntry struct {
	Name, Src string
	Mode      int64
}

// archiveFiles are the archive's non-binary entries, as goreleaser lays them
// out: a bare path keeps its path, a src entry lands under dst (by its base
// name with strip_parent). Each must match at least one regular file.
// v0.8.2's darwin archives are the reference: TestDarwinArchive_IsLaidOutAsV082
// holds the layout to their listing.
func archiveFiles(root string, files []grFile) ([]archiveEntry, error) {
	var out []archiveEntry
	for _, f := range files {
		matches, err := filepath.Glob(filepath.Join(root, filepath.FromSlash(f.Src)))
		if err != nil {
			return nil, fmt.Errorf("archive file %s: %w", f.Src, err)
		}
		if len(matches) == 0 {
			return nil, fmt.Errorf("archive file %s matches nothing", f.Src)
		}
		slices.Sort(matches)
		for _, m := range matches {
			info, err := os.Lstat(m)
			if err != nil {
				return nil, err
			}
			if !info.Mode().IsRegular() {
				return nil, fmt.Errorf("archive file %s is not a regular file", m)
			}
			rel, err := filepath.Rel(root, m)
			if err != nil {
				return nil, err
			}
			name := filepath.ToSlash(rel)
			if f.StripParent {
				name = path.Base(name)
			}
			if f.Dst != "" {
				name = path.Join(f.Dst, name)
			}
			out = append(out, archiveEntry{Name: name, Src: m, Mode: 0o644})
		}
	}
	// goreleaser writes them sorted by their name in the archive, then the
	// binaries in build order (v0.8.2's archives: LICENSE, README.md, the
	// skills, seamless.yaml.example, seamlessd, seam).
	slices.SortFunc(out, func(a, b archiveEntry) int { return strings.Compare(a.Name, b.Name) })
	return out, nil
}

// darwinArchive packs, per darwin architecture, the archive files and that
// architecture's binaries (from dir, where darwinBuild left them and the
// signing step signed them) into <out>/<name_template>.tar.gz. Every entry
// is stamped with the commit time and owned by uid 0, so the archive is
// a function of its inputs.
func darwinArchive(cfg grConfig, r release, dir, out string) ([]string, error) {
	files, err := archiveFiles(r.repoRoot, cfg.Archive.Files)
	if err != nil {
		return nil, err
	}
	if err := os.MkdirAll(out, 0o755); err != nil {
		return nil, err
	}
	var written []string
	for _, arch := range darwinArches {
		if !cfg.buildsArch(arch) {
			continue
		}
		entries := slices.Clone(files)
		for _, b := range cfg.Builds {
			if slices.Contains(b.Goarch, arch) {
				entries = append(entries, archiveEntry{Name: b.Binary, Src: binaryPath(dir, b, arch), Mode: 0o755})
			}
		}
		seen := map[string]bool{}
		for _, e := range entries {
			if seen[e.Name] {
				return nil, fmt.Errorf("the darwin/%s archive would hold %s twice", arch, e.Name)
			}
			seen[e.Name] = true
		}
		name, err := cfg.archiveName(r.Version, "darwin", arch)
		if err != nil {
			return nil, err
		}
		dst := filepath.Join(out, name)
		if err := writeTarGz(dst, entries, r.CommitTime); err != nil {
			return nil, fmt.Errorf("%s: %w", name, err)
		}
		written = append(written, dst)
	}
	return written, nil
}

func writeTarGz(dst string, entries []archiveEntry, mtime time.Time) (err error) {
	f, err := os.Create(dst)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := f.Close(); err == nil {
			err = cerr
		}
		if err != nil {
			err = errors.Join(err, os.Remove(dst))
		}
	}()
	gz := gzip.NewWriter(f)
	tw := tar.NewWriter(gz)
	for _, e := range entries {
		if err := addFile(tw, e, mtime); err != nil {
			return err
		}
	}
	if err := tw.Close(); err != nil {
		return err
	}
	return gz.Close()
}

func addFile(tw *tar.Writer, e archiveEntry, mtime time.Time) (err error) {
	src, err := os.Open(e.Src)
	if err != nil {
		return err
	}
	defer func() {
		if cerr := src.Close(); err == nil {
			err = cerr
		}
	}()
	info, err := src.Stat()
	if err != nil {
		return err
	}
	if !info.Mode().IsRegular() {
		return fmt.Errorf("%s is not a regular file", e.Src)
	}
	hdr := &tar.Header{
		Typeflag: tar.TypeReg,
		Name:     e.Name,
		Mode:     e.Mode,
		Size:     info.Size(),
		ModTime:  mtime,
	}
	if err := tw.WriteHeader(hdr); err != nil {
		return err
	}
	n, err := io.Copy(tw, src)
	if err != nil {
		return err
	}
	if n != info.Size() {
		return fmt.Errorf("%s changed while it was archived", e.Src)
	}
	return nil
}
