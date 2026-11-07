package harvest

import (
	"bytes"
	"encoding/json"
	"io"
	"os"
	"strings"
)

// ModelSniffWindow is how many trailing bytes of a Claude Code transcript the
// model sniff reads. Assistant entries land every turn, so the window nearly
// always contains at least one; a window dominated by a single giant
// tool-result line just yields "" and the session keeps its previous
// attribution.
const ModelSniffWindow = 1 << 20

// syntheticModel is the placeholder Claude Code stamps on synthetic assistant
// entries (error placeholders and the like); it is not a real producer and
// must never become a session's attribution.
const syntheticModel = "<synthetic>"

// TranscriptModel tail-reads a Claude Code transcript (JSONL) and returns the
// model id of the last main-thread assistant entry, verbatim as the provider
// names it ("claude-fable-5"), or "" when none is found. Sidechain (subagent)
// entries are skipped: a subagent may run a different model, and its turns
// must not re-attribute the parent session. Best-effort and never errors,
// matching Findings: a blank path, unreadable file, or window without an
// assistant entry all yield "" -- the caller then leaves the session's
// previous attribution in place.
func TranscriptModel(path string) string {
	if strings.TrimSpace(path) == "" {
		return ""
	}
	f, err := os.Open(path)
	if err != nil {
		return ""
	}
	defer func() { _ = f.Close() }()

	tail, err := readTail(f, ModelSniffWindow)
	if err != nil {
		return ""
	}

	model := ""
	// When the window starts mid-file its first line is a fragment; it fails the
	// JSON parse and is skipped like any malformed line.
	for line := range bytes.SplitSeq(tail, []byte("\n")) {
		if !bytes.Contains(line, []byte(`"assistant"`)) {
			continue // cheap prefilter before a full JSON parse
		}
		var entry struct {
			Type        string `json:"type"`
			IsSidechain bool   `json:"isSidechain"`
			Message     struct {
				Model string `json:"model"`
			} `json:"message"`
		}
		if err := json.Unmarshal(line, &entry); err != nil {
			continue
		}
		if entry.Type != "assistant" || entry.IsSidechain {
			continue
		}
		if m := strings.TrimSpace(entry.Message.Model); m != "" && m != syntheticModel {
			model = m
		}
	}
	return model
}

// readTail returns up to the last window bytes of f.
func readTail(f *os.File, window int64) ([]byte, error) {
	info, err := f.Stat()
	if err != nil {
		return nil, err
	}
	if size := info.Size(); size > window {
		if _, err := f.Seek(size-window, io.SeekStart); err != nil {
			return nil, err
		}
	}
	return io.ReadAll(f)
}
