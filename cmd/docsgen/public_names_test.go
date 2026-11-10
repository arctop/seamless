package main

import (
	"bytes"
	"fmt"
	"os"
	"os/exec"
	"path/filepath"
	"regexp"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// publicNamesRule is the constraint a failure of the guard below breaks.
const publicNamesRule = "constraint seamless-is-public-owner-language-optional-features"

// deploymentNames matches the account, host, path and project names of one
// deployment. It is built from fragments so this file does not flag itself. The
// organization's name alone is legitimate: the repository, the module path, the
// service labels and the tap all carry it.
var deploymentNames = func() *regexp.Regexp {
	org := "arc" + "top"
	return regexp.MustCompile("(?i)" + strings.Join([]string{
		"_" + org + "_",
		"/opt/" + org,
		"kata" + "ta",
		`\b` + "bo" + "ron" + `\b`,
		`\b` + "ar" + "gon" + `\b`,
		"eit" + "ans-macbook",
		"fin" + "ance-os",
		org + "-(app|ios|android|mobile-apps)",
	}, "|"))
}()

// looksBinary applies git's own test: a NUL in the first 8000 bytes.
func looksBinary(raw []byte) bool {
	return bytes.IndexByte(raw[:min(len(raw), 8000)], 0) >= 0
}

// deploymentNameHits returns one "path:line: match" per offending line of raw.
func deploymentNameHits(path string, raw []byte) []string {
	if looksBinary(raw) || !deploymentNames.Match(raw) {
		return nil
	}
	var hits []string
	for i, line := range bytes.Split(raw, []byte("\n")) {
		if match := deploymentNames.Find(line); match != nil {
			hits = append(hits, fmt.Sprintf("%s:%d: %s", path, i+1, match))
		}
	}
	return hits
}

// TestTrackedFilesNameNoDeployment keeps the public tree free of one
// deployment's names. It reads git's file list rather than walking the
// directory, because other checkouts live under the working tree, and it leaves
// out docs/, the generated site, whose sources are checked here.
func TestTrackedFilesNameNoDeployment(t *testing.T) {
	if _, err := exec.LookPath("git"); err != nil {
		t.Skip("git is not installed")
	}
	top, err := exec.Command("git", "rev-parse", "--show-toplevel").Output()
	if err != nil {
		t.Skipf("not inside a git checkout: %v", err)
	}
	root := strings.TrimSpace(string(top))
	if _, err := os.Stat(filepath.Join(root, "go.mod")); err != nil {
		t.Skipf("%s is not this repository's root: %v", root, err)
	}
	list := exec.Command("git", "ls-files", "-z")
	list.Dir = root
	out, err := list.Output()
	if err != nil {
		t.Skipf("git ls-files failed: %v", err)
	}

	var hits []string
	for _, path := range strings.Split(string(out), "\x00") {
		if path == "" || strings.HasPrefix(path, "docs/") {
			continue
		}
		full := filepath.Join(root, filepath.FromSlash(path))
		info, err := os.Lstat(full)
		if err != nil || !info.Mode().IsRegular() {
			continue // removed from the working tree but not yet from the index
		}
		raw, err := os.ReadFile(full)
		require.NoError(t, err, path)
		hits = append(hits, deploymentNameHits(path, raw)...)
	}
	if len(hits) > 0 {
		t.Errorf("tracked files name one deployment; use neutral example names (%s):\n%s", publicNamesRule, strings.Join(hits, "\n"))
	}
}

// TestDeploymentNameHits pins what the guard flags and what it must leave alone.
func TestDeploymentNameHits(t *testing.T) {
	org := "arc" + "top"
	for _, flagged := range []string{
		"x_" + org + "_y",
		"/opt/" + org + "/data",
		"owner: kata" + "ta",
		"ssh bo" + "ron",
		"Bo" + "ron.local",
		"host: ar" + "gon",
		"eit" + "ans-macbook13",
		"/work/fin" + "ance-os",
		org + "-app",
		org + "-ios",
		org + "-android",
		org + "-mobile-apps",
	} {
		require.Equal(t, []string{"a.go:2: " + deploymentNames.FindString(flagged)},
			deploymentNameHits("a.go", []byte("clean\n"+flagged+"\n")), flagged)
	}
	for _, clean := range []string{
		"github.com/" + org + "/seamless",
		"com." + org + ".seamless",
		org + "/tap",
		"plan:move-to-" + org,
		"carbo" + "ron",
		"bo" + "rons",
		"ar" + "gon2id",
		"example-app and _example_owner",
	} {
		require.Empty(t, deploymentNameHits("a.go", []byte(clean)), clean)
	}
	require.Empty(t, deploymentNameHits("a.bin", []byte("\x00 owner: kata"+"ta")), "binary files are skipped")
}
