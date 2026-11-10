package retrieve

import (
	"context"
	"database/sql"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/screening"
	"github.com/arctop/seamless/internal/store"
)

// By-name reads and a restricted account's project memories
// (peer_auth.restricted).
//
// A by-name read resolves the project scope first and falls back to the global
// scope, so a project memory answers in place of a global memory of the same
// name: the owner's intended project-local override. A restricted account may
// not CREATE such a project memory over a live global rule of a
// briefing-injected kind (mcp's refuseGlobalShadow), but that is judged at
// write time only. A project memory the account wrote before the owner's
// global rule of that name existed would still answer every agent the briefing
// sent to read the rule. So the reads judge it too: while a restricted
// account's words are a project memory's content (core.Memory.Account) and a
// live global memory of the same name is a briefing-injected kind
// (screening.IsBriefingKind), the by-name read answers with the global memory,
// and the surface that can say so tells the reader the project memory exists
// and how to read it (by id). An owner's project memory carries no account and
// keeps shadowing globals as before; so does an account's project memory of a
// name whose global memory is of a kind no briefing injects.

// ShadowedRule is the live global memory of a briefing-injected kind whose
// name m, a restricted account's project memory, would answer by-name reads in
// place of. ok is false for every other memory: a global one, the owner's
// (no account), an inactive one, or one whose name no global rule holds. The
// lookup runs only for a restricted account's project memory, so an install
// without restricted accounts never makes it. The caller judges first whether
// its reader may read the global scope at all.
func ShadowedRule(ctx context.Context, db *sql.DB, m core.Memory) (core.Memory, bool, error) {
	if m.Project == "" || m.Account == "" || !m.Active() {
		return core.Memory{}, false, nil
	}
	g, ok, err := store.MemoryByName(ctx, db, "", m.Name)
	if err != nil || !ok || !screening.IsBriefingKind(g.Kind) {
		return core.Memory{}, false, err
	}
	return g, true, nil
}

// NameRead is a by-name read's answer (MemoryByNameRead).
type NameRead struct {
	Memory core.Memory // the memory the read answers with
	Found  bool
	// Passed is the restricted account's project memory of the same name the
	// read passed over for the global rule (ShadowedRule); its ID is "" when
	// the read passed over nothing.
	Passed core.Memory
}

// MemoryByNameRead resolves a by-name read of an active memory: the project
// scope first, then the global scope when globalVisible (a sealed project may
// not read it) and the project holds none, except that a restricted account's
// project memory steps aside for the global rule it would shadow (the block
// comment above). project "" reads the global scope alone.
func MemoryByNameRead(ctx context.Context, db *sql.DB, project, name string, globalVisible bool) (NameRead, error) {
	m, ok, err := store.MemoryByName(ctx, db, project, name)
	if err != nil {
		return NameRead{}, err
	}
	if ok {
		if !globalVisible {
			return NameRead{Memory: m, Found: true}, nil
		}
		g, shadowed, err := ShadowedRule(ctx, db, m)
		if err != nil {
			return NameRead{}, err
		}
		if shadowed {
			return NameRead{Memory: g, Found: true, Passed: m}, nil
		}
		return NameRead{Memory: m, Found: true}, nil
	}
	if project == "" || !globalVisible {
		return NameRead{}, nil
	}
	g, ok, err := store.MemoryByName(ctx, db, "", name)
	if err != nil || !ok {
		return NameRead{}, err
	}
	return NameRead{Memory: g, Found: true}, nil
}
