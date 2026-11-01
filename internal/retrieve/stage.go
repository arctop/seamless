package retrieve

import (
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// MemoryBodyReader reads a full memory (including its body) from a data-dir-
// relative file path. *files.Store satisfies it. It is optional on the Service:
// when unset, the briefing omits the pinned-stage section (index rows carry no
// body, so stage status cannot be parsed without it).
type MemoryBodyReader interface {
	ReadMemory(relPath string) (core.Memory, error)
}

// SetBodyReader enables the pinned-stage briefing section by giving the Service a
// way to read stage memory bodies (their Status/Gate header lives in the body).
func (s *Service) SetBodyReader(r MemoryBodyReader) { s.bodyReader = r }

// stageLine is one pinned stage rendered in the briefing.
type stageLine struct {
	id     string // memory ULID, for retrieval instrumentation
	name   string
	status string
	gate   string
}

// pinnedStages reads each stage memory's body, parses its Status/Gate header
// (core.ParseStageHeader), and returns the stages to pin in the briefing
// (newest-updated first, as passed in). Done stages are never pinned. A stage
// whose status is not a live gate (open/in_progress/blocked) -- missing,
// unparseable, or unrecognized -- is pinned only while its last update is
// within maxUnknownAgeDays: a grace window that surfaces the broken header
// without granting it the permanent pin a real gate earns. 0 disables the
// age-out (such stages then pin forever, the historical behavior). It returns
// nil when no body reader is configured, or skips a stage whose file cannot be
// read (best-effort; a missing stage body must not break the briefing).
func (s *Service) pinnedStages(stages []core.Memory, maxUnknownAgeDays int, now time.Time) []stageLine {
	if s.bodyReader == nil || len(stages) == 0 {
		return nil
	}
	var cutoff time.Time
	if maxUnknownAgeDays > 0 {
		cutoff = now.AddDate(0, 0, -maxUnknownAgeDays)
	}
	var out []stageLine
	for _, m := range stages {
		mem, err := s.bodyReader.ReadMemory(m.FilePath)
		if err != nil {
			s.logger.Warn("retrieve: read stage body", "name", m.Name, "error", err)
			continue
		}
		status, gate := core.ParseStageHeader(mem.Body)
		if status == core.StageStatusDone {
			continue // completed stages are not pinned
		}
		if !core.StageStatusLive(status) && maxUnknownAgeDays > 0 && !m.Updated.After(cutoff) {
			continue // no live gate and past the grace window
		}
		out = append(out, stageLine{id: m.ID, name: m.Name, status: status, gate: gate})
	}
	return out
}

// stagesSection renders the pinned Stages section for the briefing (right
// after constraints), or "" when there are none. The trailer names the depth
// command: a stage's gate details live in its body, which only memory_read
// reaches.
func stagesSection(stages []stageLine, marks map[string]string) string {
	if len(stages) == 0 {
		return ""
	}
	var b strings.Builder
	b.WriteString("\nStages (world-state gates):\n")
	for _, st := range stages {
		b.WriteString("- " + sanitizeName(st.name, 80) + " -- ")
		if st.status != "" {
			b.WriteString(sanitizeField(st.status, 40))
		} else {
			// A compact nudge, not just a shrug: name the missing header so the
			// reading agent knows what a fix looks like (open the body with
			// Status: open|in_progress|blocked|done).
			b.WriteString("status unknown (no Status: header)")
		}
		if st.gate != "" {
			b.WriteString(", gate " + sanitizeField(st.gate, 40))
		}
		b.WriteString(marks[st.id] + "\n")
	}
	b.WriteString("(details: memory_read name=<stage>)\n")
	return b.String()
}
