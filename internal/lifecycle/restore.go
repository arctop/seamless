package lifecycle

import (
	"context"
	"errors"
	"fmt"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// ErrLiveSuccessor is returned when restoring a superseded memory while a
// memory that replaced it is still active: both would then answer in every
// briefing and recall, two versions of one claim. The error is a
// *SuccessorError naming that memory.
var ErrLiveSuccessor = errors.New("a memory that replaced it is still active")

// SuccessorError is a refused restore's ErrLiveSuccessor, carrying the active
// memory at the end of the supersession chain so the caller can point at it.
type SuccessorError struct{ Successor core.Memory }

func (e *SuccessorError) Error() string {
	return fmt.Sprintf("%s replaced it and is still active", MemoryRef(e.Successor.Project, e.Successor.Name))
}

// Unwrap makes errors.Is(err, ErrLiveSuccessor) hold.
func (e *SuccessorError) Unwrap() error { return ErrLiveSuccessor }

// MemoryLookup resolves a memory id to its index row; found=false means no
// such memory. LiveSuccessor and Restore follow superseded_by edges with it.
type MemoryLookup func(ctx context.Context, id string) (m core.Memory, found bool, err error)

// maxSupersessionChain bounds LiveSuccessor's walk. Real chains are a handful
// of links; a longer one is a corrupt corpus, not history.
const maxSupersessionChain = 64

// LiveSuccessor follows m's superseded_by edges and returns the active memory
// the chain ends at: what replaced m, or what replaced that, and so on.
// found=false when m is not superseded, or when its chain ends at an archived
// memory or at one that no longer exists (memory_delete leaves the edge
// dangling). A chain that loops or runs past maxSupersessionChain is an
// error: an edge that cannot be followed to its end proves nothing about
// whether a replacement is live, so a guard built on it must refuse.
func LiveSuccessor(ctx context.Context, lookup MemoryLookup, m core.Memory) (core.Memory, bool, error) {
	seen := map[string]bool{m.ID: true}
	for id, hops := m.SupersededBy, 0; id != ""; hops++ {
		if hops == maxSupersessionChain {
			return core.Memory{}, false, fmt.Errorf("lifecycle.LiveSuccessor: %s: the supersession chain runs past %d memories", m.Name, maxSupersessionChain)
		}
		if seen[id] {
			return core.Memory{}, false, fmt.Errorf("lifecycle.LiveSuccessor: %s: the supersession chain loops at %s", m.Name, id)
		}
		seen[id] = true
		next, found, err := lookup(ctx, id)
		if err != nil {
			return core.Memory{}, false, fmt.Errorf("lifecycle.LiveSuccessor: %s: %w", m.Name, err)
		}
		if !found {
			return core.Memory{}, false, nil
		}
		if next.Active() {
			return next, true, nil
		}
		id = next.SupersededBy
	}
	return core.Memory{}, false, nil
}

// Restore brings an inactive memory back into the active set, whichever way it
// left: an archived memory through Unarchive, a superseded one through
// Unsupersede. A superseded memory is restored only while nothing along its
// supersession chain is active (LiveSuccessor through lookup); otherwise the
// error is a *SuccessorError and nothing is written. An active memory is
// refused with ErrNotArchived. old must carry its full Body, read under its
// file's lock (files.Manager.Mutate), as every caller of the two it
// dispatches to must.
//
// The chain is read from the index, outside the successors' own locks: a
// successor restored in the same instant can race the check, and the result
// is then two active memories the owner sees side by side, never a lost
// write.
func Restore(ctx context.Context, w MemoryWriter, lookup MemoryLookup, old core.Memory, now time.Time) (core.Memory, error) {
	if old.InvalidAt == nil || old.SupersededBy == "" {
		return Unarchive(ctx, w, old, now)
	}
	live, found, err := LiveSuccessor(ctx, lookup, old)
	if err != nil {
		return core.Memory{}, fmt.Errorf("lifecycle.Restore: %w", err)
	}
	if found {
		return core.Memory{}, fmt.Errorf("lifecycle.Restore: %s: %w", old.Name, &SuccessorError{Successor: live})
	}
	return Unsupersede(ctx, w, old, now)
}

// Unarchive is the inverse of Archive: it clears invalid_at, strips the archive
// tombstone the archive appended, and rewrites the file (which re-indexes the
// memory back into the active set). old must carry its full Body.
//
// It refuses a memory that is not archived. A superseded memory in particular is
// not "archived with a note attached": undoing that is Unsupersede's job, and
// silently accepting it here would drop the superseded_by edge on the floor.
func Unarchive(ctx context.Context, w MemoryWriter, old core.Memory, now time.Time) (core.Memory, error) {
	if old.InvalidAt == nil {
		return core.Memory{}, fmt.Errorf("lifecycle.Unarchive: %s: %w", old.Name, ErrNotArchived)
	}
	if old.SupersededBy != "" {
		return core.Memory{}, fmt.Errorf("lifecycle.Unarchive: %s: %w", old.Name, ErrNotArchived)
	}
	return restore(ctx, w, old, "> Archived", now)
}

// Unsupersede is the inverse of Supersede: it clears invalid_at and
// superseded_by, strips the supersession tombstone, and rewrites the file. old
// must carry its full Body. The replacement memory is untouched -- supersession
// only ever marked the old one.
//
// It refuses a memory that is not superseded (an active or plainly archived one).
func Unsupersede(ctx context.Context, w MemoryWriter, old core.Memory, now time.Time) (core.Memory, error) {
	if old.InvalidAt == nil || old.SupersededBy == "" {
		return core.Memory{}, fmt.Errorf("lifecycle.Unsupersede: %s: %w", old.Name, ErrNotSuperseded)
	}
	return restore(ctx, w, old, supersededPrefix, now)
}

// restore reactivates a memory: invalid_at and superseded_by are cleared, the
// trailing tombstone line carrying prefix is removed, and the file is rewritten.
func restore(ctx context.Context, w MemoryWriter, old core.Memory, prefix string, now time.Time) (core.Memory, error) {
	old.InvalidAt = nil
	old.SupersededBy = ""
	old.Updated = now.UTC()
	old.Body = stripTombstone(old.Body, prefix)
	return w.WriteMemory(ctx, old)
}

// stripTombstone removes the trailing blockquote line a retirement appended,
// restoring the body a reader saw before it. It only ever touches the LAST
// non-empty line and only when that line carries prefix, so a body whose own
// prose happens to quote a tombstone earlier on is left alone -- and a memory
// retired twice over its life keeps the older tombstones as history.
func stripTombstone(body, prefix string) string {
	trimmed := strings.TrimRight(body, "\n")
	idx := strings.LastIndex(trimmed, "\n")
	last := trimmed[idx+1:] // idx == -1 => the whole body is one line
	if !strings.HasPrefix(strings.TrimSpace(last), prefix) {
		return body
	}
	if idx < 0 {
		return ""
	}
	return strings.TrimRight(trimmed[:idx], "\n") + "\n"
}
