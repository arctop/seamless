package retrieve

import (
	"context"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/store"
)

// attachStamps sets each hit's provenance stamps (Hit.MayContain) from
// item_provenance -- one lookup per kind among the hits -- naming only the
// projects the caller's View may list: a restricted account is never told
// the name of a project it cannot reach (MayContainWithheld counts those).
// A stamp for the hit's own project is never shown. Hits with none are left
// as they were.
func (s *Service) attachStamps(ctx context.Context, hits []Hit) error {
	if len(hits) == 0 {
		return nil
	}
	byKind := map[string][]string{}
	for _, h := range hits {
		byKind[h.Kind] = append(byKind[h.Kind], h.ID)
	}
	stamps := make(map[string]map[string][]core.Stamp, len(byKind))
	for kind, ids := range byKind {
		got, err := store.ProvenanceForItems(ctx, s.db, kind, ids)
		if err != nil {
			return err
		}
		stamps[kind] = got
	}
	v, _ := access.FromContext(ctx)
	for i := range hits {
		h := &hits[i]
		for _, st := range core.StampsShown(stamps[h.Kind][h.ID], h.Project) {
			if !v.Listed(st.Project) {
				h.MayContainWithheld++
				continue
			}
			h.MayContain = append(h.MayContain, st)
		}
	}
	return nil
}
