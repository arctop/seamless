package console

import (
	"context"
	"net/http"
	"slices"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/features"
	"github.com/arctop/seamless/internal/screening"
	"github.com/arctop/seamless/internal/store"
)

// The held queue as the restricted accounts' proposals queue
// (Config.RestrictedAccounts).
//
// A restricted local account (peer_auth.restricted) never lands a change to a
// constraint, convention, protocol or stage: the write waits in the held queue
// as its proposal (reason screening.ReasonRestrictedAccount, the account in
// Detail[screening.DetailAccount]) whether or not write screening is on. The
// owner promotes it -- the queue's Release -- or discards it.
//
// So the Held screen has three modes. Screening on: the whole screen, as
// before, with a proposal's card saying who proposed it and its release button
// reading Promote. Screening off with restricted accounts configured: the
// queue only, and only its proposals; the screening-only parts (lockdown, the
// review, drift, session marks) stay behind the feature, and so do any writes
// screening held before it was switched off -- turning screening off releases
// none of them, and they wait for it to come back on. Neither: the
// switched-off page, exactly as before.

// heldMode is which Held screen a request gets.
type heldMode int

const (
	heldOff       heldMode = iota // the switched-off page
	heldProposals                 // screening off, restricted accounts: proposals only
	heldScreening                 // screening on: the whole screen
)

// heldModeFor answers the mode for an effective feature state.
func (s *Service) heldModeFor(feats config.Features) heldMode {
	switch {
	case features.Enabled(feats, features.Screening):
		return heldScreening
	case s.cfg.RestrictedAccounts:
		return heldProposals
	default:
		return heldOff
	}
}

// heldGate serves the held queue's own routes (the list, a hold's URL,
// release, discard) while the screen is offered in either mode, and answers
// the switched-off page otherwise -- the same answer gate gives every other
// screening route.
func (s *Service) heldGate(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		if s.heldModeFor(s.effectiveFeatures(r.Context())) == heldOff {
			s.featureOff(w, r, features.Screening)
			return
		}
		next(w, r)
	}
}

// pendingProposals lists the pending holds that are restricted accounts'
// proposals, newest first, read through the console's handle.
func (s *Service) pendingProposals(ctx context.Context) ([]store.HeldWrite, error) {
	pending, err := store.HeldWrites(ctx, s.db(ctx), store.HeldStatusHeld, 0)
	if err != nil {
		return nil, err
	}
	return onlyProposals(pending), nil
}

// onlyProposals keeps the holds whose reasons make them proposals.
func onlyProposals(holds []store.HeldWrite) []store.HeldWrite {
	return slices.DeleteFunc(holds, func(h store.HeldWrite) bool { return !screening.IsProposal(h.Reasons) })
}

// withProposalScreen offers the Held screen while screening is off but
// restricted accounts are configured, at its registry place: their proposals
// wait there. Any other state returns visible itself.
func (s *Service) withProposalScreen(visible []screen, feats config.Features, lvl level) []screen {
	if s.heldModeFor(feats) != heldProposals || slices.ContainsFunc(visible, func(sc screen) bool { return sc.ID == "held" }) {
		return visible
	}
	out := make([]screen, 0, len(visible)+1)
	for _, sc := range screens {
		if sc.visibleAt(feats, lvl) || (sc.ID == "held" && lvl >= sc.Min) {
			out = append(out, sc)
		}
	}
	return out
}
