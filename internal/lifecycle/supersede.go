// Package lifecycle carries a memory's bi-temporal lifecycle: supersession (one
// memory replacing another) and provenance. A superseded memory is not deleted;
// it is stamped invalid_at + superseded_by and keeps a tombstone line in its
// file body, so the on-disk source of truth stays honest while the memory leaves
// every active index (briefing, prompt corpus, recall).
package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/files"
)

// Domain errors guarding the supersession invariants: invalid_at is stamped
// exactly once, and superseded_by edges always point from a newly invalidated
// memory to an active one, so cycles cannot form.
var (
	// ErrAlreadyInvalid is returned when superseding or archiving a memory whose
	// invalid_at is already set; re-stamping would rewrite supersession history.
	ErrAlreadyInvalid = errors.New("memory is already superseded or archived")
	// ErrSelfSupersede is returned when a memory would supersede itself, which
	// would create a superseded_by self-cycle.
	ErrSelfSupersede = errors.New("memory cannot supersede itself")
	// ErrInvalidReplacement is returned when the replacement is missing an id or
	// is itself invalid; pointing superseded_by at an inactive memory could form
	// a cycle or a dangling chain.
	ErrInvalidReplacement = errors.New("replacement memory is not active")
	// ErrNotArchived is returned when unarchiving a memory that is not archived
	// -- either still active or superseded (whose undo is Unsupersede).
	ErrNotArchived = errors.New("memory is not archived")
	// ErrNotSuperseded is returned when unsuperseding a memory that carries no
	// superseded_by edge -- either still active or plainly archived.
	ErrNotSuperseded = errors.New("memory is not superseded")
)

// MemoryWriter is the subset of files.Manager that Supersede needs, so the flow
// is testable against a real files.Manager without importing the whole world.
type MemoryWriter interface {
	WriteMemory(ctx context.Context, mem core.Memory) (core.Memory, error)
}

// Supersede marks old as replaced by replacement: it sets old.InvalidAt = now and
// old.SupersededBy = replacement.ID, appends a tombstone line to old's body, and
// rewrites old's file (which re-indexes it out of the active set). old must carry
// its full Body (read the file first, since index rows have no body). It returns
// the updated old memory.
//
// old and replacement must be distinct memories with distinct file paths;
// same-name replacement is handled as an in-place update by memory_write and must
// not reach here (guarded by the caller).
func Supersede(ctx context.Context, w MemoryWriter, old, replacement core.Memory, now time.Time) (core.Memory, error) {
	if old.ID == replacement.ID {
		return core.Memory{}, fmt.Errorf("lifecycle.Supersede: %s: %w", old.Name, ErrSelfSupersede)
	}
	if old.InvalidAt != nil {
		return core.Memory{}, fmt.Errorf("lifecycle.Supersede: %s: %w", old.Name, ErrAlreadyInvalid)
	}
	if replacement.ID == "" || replacement.InvalidAt != nil {
		return core.Memory{}, fmt.Errorf("lifecycle.Supersede: %s: %w", replacement.Name, ErrInvalidReplacement)
	}
	at := now.UTC()
	old.InvalidAt = &at
	old.SupersededBy = replacement.ID
	old.Updated = at
	old.Body = strings.TrimRight(old.Body, "\n") + "\n\n" + tombstone(replacement, at) + "\n"
	return w.WriteMemory(ctx, old)
}

// supersededPrefix opens the tombstone line Supersede appends.
const supersededPrefix = "> Superseded by"

// tombstone is the body line appended to a superseded memory, keeping the file
// truthful about what replaced it.
func tombstone(replacement core.Memory, at time.Time) string {
	return fmt.Sprintf(supersededPrefix+" %s (%s) on %s",
		MemoryRef(replacement.Project, replacement.Name), replacement.ID, at.Format("2006-01-02"))
}

// WithoutTombstone is body without the tombstone line naming replacementID, for
// a reader who may not read that replacement: the line carries its project, its
// name and its id. The line is found by the id rather than by being last, so a
// file edited by hand after the supersede is still covered, and prose that only
// looks like a tombstone stays. It changes the reader's copy, never the file.
func WithoutTombstone(body, replacementID string) string {
	if replacementID == "" {
		return body
	}
	id := "(" + replacementID + ")"
	lines := strings.Split(body, "\n")
	kept := make([]string, 0, len(lines))
	for _, line := range lines {
		if t := strings.TrimSpace(line); strings.HasPrefix(t, supersededPrefix) && strings.Contains(t, id) {
			continue
		}
		kept = append(kept, line)
	}
	if len(kept) == len(lines) {
		return body
	}
	out := strings.TrimRight(strings.Join(kept, "\n"), "\n")
	if out == "" {
		return ""
	}
	return out + "\n"
}

// MemoryRef renders a project-qualified memory reference (project/name), or just
// name for a global memory. Used in tombstones and read warnings.
func MemoryRef(project, name string) string {
	if project == "" {
		return name
	}
	return project + "/" + name
}

// compile-time assurance that *files.Manager satisfies MemoryWriter.
var _ MemoryWriter = (*files.Manager)(nil)
