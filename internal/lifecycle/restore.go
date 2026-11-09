package lifecycle

import (
	"context"
	"fmt"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/core"
)

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
