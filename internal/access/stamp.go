package access

import "context"

// Stamp is one provenance stamp a write carries (plan fenced-sessions P6):
// the item may contain content from Project, which the conversation holds,
// cleared into the item's project by Via Ref -- the flow's record as the
// store named it (an approval's kind and id, or the conversation's own
// session for a fenced home's native flow). It names a project and an
// approval, never content, and it judges nothing.
type Stamp struct {
	Project, Via, Ref string
}

// Stamps returns the stamps a write into dest carries: one for every project
// in the conversation's label other than dest, named by the flow that clears
// it into dest. A write into dest is cleared only when each of them has one
// (Cleared), so for a write that lands the list is complete; a label project
// with no flow into dest gets none, and its write is refused before it lands.
// Nil for an empty label, the global scope, a legacy judged state and the zero
// value: with isolation.requests off nothing is stamped.
func (g Grants) Stamps(dest string) []Stamp {
	if !g.judged || dest == "" || len(g.label) == 0 {
		return nil
	}
	var out []Stamp
	for _, p := range g.label {
		if p == dest {
			continue
		}
		i, found := g.flowIndex(p, dest)
		if !found {
			continue
		}
		f := g.flows[i]
		out = append(out, Stamp{Project: p, Via: f.Via, Ref: f.Ref})
	}
	return out
}

// StampsFor is Grants.Stamps for the state ctx carries: nil when it carries
// none (isolation.requests off, or a request no conversation identifies).
func StampsFor(ctx context.Context, dest string) []Stamp {
	g, ok := GrantsFrom(ctx)
	if !ok {
		return nil
	}
	return g.Stamps(dest)
}
