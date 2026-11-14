package lifecycle

import (
	"context"
	"fmt"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/files"
)

// MemoryMutator is the subset of files.Manager UnionStamps needs: a
// read-modify-write of one memory under its file's lock.
type MemoryMutator interface {
	MutateMemory(ctx context.Context, relPath string, fn func(context.Context, core.Memory) (core.Memory, error)) (core.Memory, error)
}

// UnionStamps gives replacement the provenance stamps of a memory it
// superseded (core.Stamp, plan fenced-sessions P6): supersession keeps the
// union, so what the old memory may have held from a fenced project stays
// named on what replaced it. It runs after Supersede, under the
// replacement's own lock (never inside the old file's: one lock at a time,
// so two supersessions in opposite directions cannot deadlock), on the file
// as it stands. A stamp the replacement already names for a project keeps
// its approval, one for the replacement's own project is not added, and a
// replacement that gains nothing is not rewritten. With no stamps to add it
// touches nothing, so a corpus that never carried a stamp sees no extra
// write.
func UnionStamps(ctx context.Context, m MemoryMutator, replacement core.Memory, stamps []core.Stamp) (core.Memory, error) {
	if len(stamps) == 0 {
		return replacement, nil
	}
	if replacement.FilePath == "" {
		return core.Memory{}, fmt.Errorf("lifecycle.UnionStamps: %s has no file path", replacement.Name)
	}
	out, err := m.MutateMemory(ctx, replacement.FilePath, func(_ context.Context, cur core.Memory) (core.Memory, error) {
		if cur.ID != replacement.ID {
			return core.Memory{}, fmt.Errorf("%s holds memory %s, not the replacement %s", replacement.FilePath, cur.ID, replacement.ID)
		}
		merged, added := core.MergeStamps(cur.MayContain, stamps, cur.Project)
		if !added {
			return core.Memory{}, files.ErrNoChange
		}
		cur.MayContain = merged
		return cur, nil
	})
	if err != nil {
		return core.Memory{}, fmt.Errorf("lifecycle.UnionStamps: %w", err)
	}
	return out, nil
}

// compile-time assurance that *files.Manager satisfies MemoryMutator.
var _ MemoryMutator = (*files.Manager)(nil)
