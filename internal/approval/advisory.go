package approval

// The in-session dialog's decision in the advisory tier (isolation.requests
// without peer_auth; plan fenced-sessions P9, owner decision D7 of the
// fenced-sessions design): Approve or Deny in the agent's own client decides a
// grant or move request.
//
// D7 relaxes the 2026-11-06 rule "a dialog answer alone never grants
// anything" for this tier only. Without peer_auth the bearer key decides every
// request already (ProofKey: seam approve's y/N, the console's Approve and
// Deny), and Seamless can tell no more about who answers a dialog than about
// who holds the key, so the dialog's decision is advisory exactly as theirs
// is. Under peer_auth the dialog still only starts a proof (Touch ID, a
// passkey), and both halves here are refused.

import (
	"context"
	"errors"
	"fmt"

	"github.com/arctop/seamless/internal/store"
)

// ProofDialog is the advisory tier's decision in the agent's in-session
// approval dialog: Approve there. Only Grants without peer_auth accept it, as
// only they accept ProofKey; the lock's approve core knows no such proof.
const ProofDialog = "dialog"

// DecidedByDialog is decided_by -- on the request, and on the grant or the
// move an approval writes -- for a decision made in the advisory tier's
// in-session dialog, Approve or Deny. It tells an owner reading the record
// which advisory surface decided: the dialog in the agent's own client, which
// any process holding the bearer key could answer and a client hook could
// answer by itself, or bearer-key (seam approve, the console).
const DecidedByDialog = "in-session-dialog"

// ErrDialogPeerAuth is a decision of the in-session dialog under peer_auth,
// where the dialog only starts a proof: it approves and denies nothing.
var ErrDialogPeerAuth = errors.New("under peer_auth the in-session dialog decides nothing: an approval needs the owner's " +
	"proof (seam approve in the owner's own terminal, a passkey or Touch ID), and a denial the owner's terminal or browser")

// DenyInDialog denies grant or move request id with the in-session dialog's
// Deny, the advisory tier's denial there; decided_by names the dialog
// (DecidedByDialog). Under peer_auth an agent's connection may not deny, so it
// is refused, and a lock's request is the lock's: only peer_auth files one.
func (g *Grants) DenyInDialog(ctx context.Context, id string) error {
	const op = "approval.Grants.DenyInDialog"
	if err := refuseRestricted(ctx); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if !g.advisory {
		return fmt.Errorf("%s: %w", op, ErrDialogPeerAuth)
	}
	a, err := store.AccessApprovalByID(ctx, g.svc.cfg.DB, id)
	if err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	if a.Kind != store.AccessKindGrant && a.Kind != store.AccessKindMove {
		return fmt.Errorf("%s: %w: %s", op, ErrNotAGrant, ShortID(a.ID))
	}
	if err := g.svc.Deny(ctx, a.ID, DecidedByDialog); err != nil {
		return fmt.Errorf("%s: %w", op, err)
	}
	return nil
}
