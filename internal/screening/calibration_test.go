package screening

import (
	"fmt"
	"io/fs"
	"os"
	"path/filepath"
	"sort"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/files"
)

// TestCalibration_Corpus runs the screens over a folder of memory and note
// markdown files and prints how often each verdict fires, per code and per
// file -- never the matched text, which stays in the files. It is how an owner
// measures false positives on a real corpus before turning the feature on.
//
// It is skipped unless SEAMLESS_SCREEN_CORPUS names a directory (searched
// recursively for *.md). SEAMLESS_SCREEN_ALLOWLIST (comma-separated hosts)
// and SEAMLESS_SCREEN_BLOB_MIN_LENGTH set the rules; unset, they are the
// defaults (no URL screen, 200).
//
//	SEAMLESS_SCREEN_CORPUS=/path/to/corpus go test ./internal/screening -run TestCalibration_Corpus -v
func TestCalibration_Corpus(t *testing.T) {
	dir := os.Getenv("SEAMLESS_SCREEN_CORPUS")
	if dir == "" {
		t.Skip("set SEAMLESS_SCREEN_CORPUS=<dir> to calibrate the screens on a corpus")
	}
	rules := Rules{BlobMinLength: config.DefaultScreeningBlobMinLength}
	if v := os.Getenv("SEAMLESS_SCREEN_ALLOWLIST"); v != "" {
		for h := range strings.SplitSeq(v, ",") {
			if h = strings.TrimSpace(h); h != "" {
				rules.URLAllowlist = append(rules.URLAllowlist, h)
			}
		}
	}
	if v := os.Getenv("SEAMLESS_SCREEN_BLOB_MIN_LENGTH"); v != "" {
		var n int
		_, err := fmt.Sscanf(v, "%d", &n)
		require.NoError(t, err, "SEAMLESS_SCREEN_BLOB_MIN_LENGTH must be a number")
		rules.BlobMinLength = n
	}

	totals := map[Code]int{}
	filesWith := map[Code]int{}
	var scanned, held, unreadable int
	var lines []string
	err := filepath.WalkDir(dir, func(path string, d fs.DirEntry, walkErr error) error {
		if walkErr != nil {
			return walkErr
		}
		if d.Type()&fs.ModeSymlink != 0 || d.IsDir() || filepath.Ext(path) != ".md" {
			return nil
		}
		raw, err := os.ReadFile(path)
		if err != nil {
			unreadable++
			return nil
		}
		rel, err := filepath.Rel(dir, path)
		if err != nil {
			rel = filepath.Base(path)
		}
		text := corpusText(string(raw), filepath.ToSlash(rel))
		found := Scan(text, rules)
		scanned++
		codes := found.Codes()
		if len(HoldCodes(codes)) > 0 {
			held++
		}
		if len(codes) == 0 {
			return nil
		}
		parts := make([]string, 0, len(codes))
		for _, c := range codes {
			totals[c] += found[c]
			filesWith[c]++
			parts = append(parts, fmt.Sprintf("%s=%d", c, found[c]))
		}
		lines = append(lines, fmt.Sprintf("  %s: %s", filepath.ToSlash(rel), strings.Join(parts, " ")))
		return nil
	})
	require.NoError(t, err)

	sort.Strings(lines)
	t.Logf("screened %d files (%d unreadable); %d would be held", scanned, unreadable, held)
	t.Logf("verdicts per code (matches, files):")
	for _, c := range Codes {
		if totals[c] > 0 {
			hold := "holds"
			if !c.Holds() {
				hold = "records"
			}
			t.Logf("  %-26s %5d match(es) in %4d file(s)  [%s]", c, totals[c], filesWith[c], hold)
		}
	}
	t.Logf("verdicts per file:")
	for _, l := range lines {
		t.Log(l)
	}
}

// corpusText is what the write-time screen would read of a corpus file: a
// memory's description, body and tags, or a note's title, description, source
// URL, body and tags. A file that parses as neither is screened whole.
func corpusText(content, rel string) string {
	if mem, err := files.ParseMemory(content, rel); err == nil && mem.Kind != "" {
		return MemoryText(mem)
	}
	if note, err := files.ParseNote(content, rel); err == nil && (note.Title != "" || note.Slug != "") {
		return NoteText(note)
	}
	return content
}
