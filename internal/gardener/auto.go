package gardener

import (
	"context"
	"sync"

	"github.com/arctop/seamless/internal/store"
)

// Automatic apply (gardener.auto_apply, off by default): a new proposal of a
// kind the owner named, from the AutoSafe four, is applied at the end of the
// pass that raised it, in the name ByAuto. Each stays undoable from the
// console like any apply, and an undone one returns to the queue for the
// owner: only a pass's own new proposals are applied by themselves, never an
// older pending one.

// The gardener.action actions this file and the console's Settings record.
const (
	// ActionKeptForReview: a new proposal of an enabled kind was left
	// pending for the owner. Its reason is one of the AutoKept* codes.
	ActionKeptForReview = "kept_for_review"
	// ActionSwitches and ActionSwitchesReset: the owner saved or reset the
	// gardener's switches on Settings (by: console).
	ActionSwitches      = "switches"
	ActionSwitchesReset = "switches_reset"
	// ActionRunNow, ActionRequest and ActionSplit: someone asked for a pass,
	// for proposals in words, or for a split plan. Each only proposes.
	ActionRunNow  = "run_now"
	ActionRequest = "request"
	ActionSplit   = "split"
)

// NotDecisions are the actions a gardener.action event can carry by the
// console that decide no proposal: asking for a pass, for proposals or for a
// split, and changing the switches. The console's "since you last looked"
// leaves them out (an owner's Run a pass now is recorded after the pass, so
// it would hide what that pass applied by itself).
var NotDecisions = []string{ActionRunNow, ActionRequest, ActionSplit, ActionSwitches, ActionSwitchesReset}

// Why a new proposal of an enabled kind waits for the owner instead (the
// reason of an ActionKeptForReview event). Codes, never text.
const (
	// AutoKeptLockdown: write screening's lockdown is on, which only ever
	// reduces what agents receive.
	AutoKeptLockdown = "lockdown"
	// AutoKeptScreening: write screening would hold what the apply lands.
	AutoKeptScreening = "screening"
	// AutoKeptLocked: a project the proposal touches is locked (peer_auth).
	AutoKeptLocked = "locked"
	// AutoKeptFenced: a project the proposal touches is fenced from the
	// daemon's own read (a confidential or sealed project), or the proposal
	// cannot be attributed to a project at all.
	AutoKeptFenced = "fenced"
	// AutoKeptFailed: the apply did not land; the proposal stays pending as a
	// failed apply always does.
	AutoKeptFailed = "failed"
)

// AutoGuard is what automatic apply asks before it applies a proposal by
// itself (SetAutoGuard). Each func may be nil, which asks nothing. The daemon
// wires write screening and peer_auth's locks in here (cmd/seamlessd), since
// this package cannot import either.
type AutoGuard struct {
	// Lockdown reports whether write screening's lockdown is on now. It is
	// false whenever the screening feature is off.
	Lockdown func(context.Context) bool
	// Holds reports whether write screening, on now, would hold text: what
	// the apply would land (a task's title and body). False while it is off.
	Holds func(ctx context.Context, text string) bool
	// Locked reports whether peer_auth locks project.
	Locked func(project string) bool
}

// SetAutoGuard hands automatic apply what it asks before each apply. Call it
// before Start.
func (s *Service) SetAutoGuard(g AutoGuard) { s.guard = g }

// createdProposal is one proposal a pass raised.
type createdProposal struct{ id, kind string }

// createdLog collects the proposals one pass raises (createProposal notes
// each), so automatic apply takes exactly those.
type createdLog struct {
	mu  sync.Mutex
	ids []createdProposal
}

type createdLogKey struct{}

// withCreatedLog returns ctx carrying a fresh log, and the log.
func withCreatedLog(ctx context.Context) (context.Context, *createdLog) {
	l := &createdLog{}
	return context.WithValue(ctx, createdLogKey{}, l), l
}

// noteCreated adds a proposal to the log ctx carries, if any. A request's
// proposals (Request, Split) carry none.
func noteCreated(ctx context.Context, id, kind string) {
	if l, ok := ctx.Value(createdLogKey{}).(*createdLog); ok {
		l.mu.Lock()
		l.ids = append(l.ids, createdProposal{id: id, kind: kind})
		l.mu.Unlock()
	}
}

// list is the log's proposals, in the order they were raised.
func (l *createdLog) list() []createdProposal {
	l.mu.Lock()
	defer l.mu.Unlock()
	return append([]createdProposal(nil), l.ids...)
}

// autoApplyNew applies, in the name ByAuto, each proposal of created whose
// kind the switches in force name, unless autoHold keeps it for the owner. It
// reads the switches once, now, so a change on Settings applies from the next
// pass without a restart; unreadable switches apply nothing. It returns how
// many it applied and how many of an enabled kind it left pending.
func (s *Service) autoApplyNew(ctx context.Context, created []createdProposal) (applied, kept int) {
	if !anyAutoSafe(created) {
		return 0, 0
	}
	sw, err := s.Switches(ctx)
	if err != nil {
		s.logger.Warn("gardener: automatic apply: the switches could not be read; nothing was applied by itself", "error", err)
		return 0, 0
	}
	if len(sw.AutoApply) == 0 {
		return 0, 0
	}
	actx := WithActor(ctx, ByAuto)
	for _, c := range created {
		if !sw.AutoApplies(c.kind) {
			continue
		}
		reason, err := s.autoHold(ctx, c.id)
		if err != nil {
			s.logger.Warn("gardener: automatic apply: could not judge a proposal; it waits for the owner", "id", c.id, "error", err)
			reason = AutoKeptFailed
		}
		if reason == "" {
			res, aerr := s.Apply(actx, c.id)
			switch {
			case aerr != nil:
				s.logger.Warn("gardener: automatic apply failed; the proposal waits for the owner", "id", c.id, "kind", c.kind, "error", aerr)
				reason = AutoKeptFailed
			case res["status"] == store.ProposalApplied:
				applied++
				continue
			default:
				continue // settled as moot, by auto: nothing left to apply or review
			}
		}
		kept++
		s.record(actx, c.id, map[string]any{"action": ActionKeptForReview, "kind": c.kind, "reason": reason})
	}
	return applied, kept
}

// anyAutoSafe reports whether created holds a kind automatic apply could take,
// so a pass that raised none never reads the switches.
func anyAutoSafe(created []createdProposal) bool {
	for _, c := range created {
		if AutoSafe(c.kind) {
			return true
		}
	}
	return false
}

// autoHold says why the proposal must wait for the owner instead of being
// applied by itself, "" when nothing does: write screening's lockdown, a
// project it touches that peer_auth locks or the daemon's own read is fenced
// from (an unattributable payload counts as fenced), or text the apply would
// land that write screening would hold.
func (s *Service) autoHold(ctx context.Context, id string) (string, error) {
	if s.guard.Lockdown != nil && s.guard.Lockdown(ctx) {
		return AutoKeptLockdown, nil
	}
	p, ok, err := store.ProposalByID(ctx, s.db, id)
	if err != nil {
		return "", err
	}
	if !ok || p.Status != store.ProposalPending {
		return AutoKeptFailed, nil
	}
	scope, err := ResolveProposalScope(ctx, s.db, p)
	if err != nil {
		return "", err
	}
	if !scope.Resolved {
		return AutoKeptFenced, nil
	}
	if s.guard.Locked != nil {
		for _, project := range scope.Projects {
			if project != "" && s.guard.Locked(project) {
				return AutoKeptLocked, nil
			}
		}
	}
	// The daemon's own read is the global scope's: it may not read what a
	// confidential or sealed project keeps to itself (store.CanRead).
	gate := newScopeGate(s.db, "")
	for _, project := range scope.Projects {
		readable, err := gate.readable(ctx, project)
		if err != nil {
			return "", err
		}
		if !readable {
			return AutoKeptFenced, nil
		}
	}
	if text := autoLandedText(p); text != "" && s.guard.Holds != nil && s.guard.Holds(ctx, text) {
		return AutoKeptScreening, nil
	}
	return "", nil
}

// autoLandedText is the text an automatic apply of p would land: a task's
// topic and body for the two kinds that open one, written from what agents
// searched for or the errors they hit. The plan settlements retag a note and
// land no text.
func autoLandedText(p store.Proposal) string {
	switch p.Kind {
	case store.ProposalMemoryWanted:
		return payloadString(p.Payload, "suggested_title") + "\n" + memoryWantedTaskBody(p.Payload)
	case store.ProposalToolError:
		return payloadString(p.Payload, "suggested_title") + "\n" + toolErrorTaskBody(p.Payload)
	}
	return ""
}
