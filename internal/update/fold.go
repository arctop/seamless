package update

import (
	"slices"
	"time"
)

// Folding an attempt: what an updater's record (attempt.json, or the history
// line it appended) says happened, and what follows from it for automatic
// updates -- blocks, backoff, the rollback count and the pause. Classify and
// applyOutcome are pure; the checker reads the files and calls them.

// Outcomes of a folded attempt (AttemptResult.Outcome, the update.failed
// payload). Each is a fixed word.
const (
	// OutcomeApplied: To is installed and the updater confirmed it running.
	OutcomeApplied = "applied"
	// OutcomeUnverified: To is running, but the updater stopped before it
	// confirmed so (killed, crashed or asleep after the swap). Plan 2.5:
	// running To means success, unverified.
	OutcomeUnverified = "unverified"
	// OutcomeRolledBack: the attempt failed at or after the install and From
	// was restored.
	OutcomeRolledBack = "rolled_back"
	// OutcomeFailed: the attempt failed with the install left as it was:
	// before anything changed (Stage.PreSwap, StageVerify, or StageSpawn when
	// the updater never started), or in StageInstall with the installer
	// leaving From in place.
	OutcomeFailed = "failed"
	// OutcomeBroken: the attempt failed at or after the install and could
	// not be rolled back cleanly -- or stopped where nothing vouches for the
	// install (a stage this release does not know, an unfinished rollback
	// with To running).
	OutcomeBroken = "broken"
	// OutcomeInterrupted: the updater stopped before it finished (killed,
	// crashed, asleep with its machine, or never started) and From still
	// runs. Retried after a backoff; a record that shows up later replaces
	// this outcome.
	OutcomeInterrupted = "interrupted"
	// OutcomeSuperseded: the running version is neither From nor To; the
	// install changed some other way and the attempt no longer matters.
	OutcomeSuperseded = "superseded"
)

// StageSpawn is the stage an AttemptResult and the update.failed payload name
// for an attempt whose updater never wrote a record: Spawn returned an error,
// or nothing showed up within AttemptStartWindow. It is not a Stage the
// updater moves through (Stages); the surfaces word it as the updater never
// starting.
const StageSpawn Stage = "spawn"

// spawnedByDaemon reports whether an attempt with why was started by a
// daemon's checker, so its outcome feeds blocks, backoff and the pause. A
// manual `seamlessd update` (WhyManual) never does, nor does a why this
// release does not know.
func spawnedByDaemon(why string) bool { return why == WhyAuto || why == WhyNow }

// Classify names the outcome of an attempt that is not Live, for a daemon
// running the version running. For a finished record it reads the fields the
// updater set (attempt.go); for an unfinished one -- the updater stopped --
// the running version is the evidence:
//
//   - finished OK: applied;
//   - finished RolledBack: rolled back;
//   - finished before StageInstall (Stage.PreSwap, StageVerify) or in
//     StageInstall itself (the installer failed and left From): failed;
//   - finished anywhere else -- StageRollback without RolledBack (the
//     rollback failed), StageConfirm, StageDone without OK, a stage this
//     release does not know: broken, since nothing vouches for the install;
//   - unfinished in StageRollback: rolled back when From runs (the rollback
//     got that far), else broken;
//   - unfinished elsewhere: unverified when To runs, interrupted when From
//     does, superseded when neither.
//
// An unfinished record with From running may have left To on disk (killed
// between the swap and the restart). The daemon cannot see the disk; the
// interrupted attempt is retried after its backoff, and that run reinstalls
// To and confirms it -- or rolls back -- through the normal path, so the
// install converges either way.
func Classify(a Attempt, running Version) string {
	if a.Finished() {
		switch {
		case a.OK:
			return OutcomeApplied
		case a.RolledBack:
			return OutcomeRolledBack
		case a.Stage.PreSwap(), a.Stage == StageVerify, a.Stage == StageInstall:
			return OutcomeFailed
		default:
			return OutcomeBroken
		}
	}
	switch {
	case a.Stage == StageRollback && running == a.From:
		return OutcomeRolledBack
	case a.Stage == StageRollback:
		return OutcomeBroken
	case running == a.To:
		return OutcomeUnverified
	case running == a.From:
		return OutcomeInterrupted
	default:
		return OutcomeSuperseded
	}
}

// resultOf is the AttemptResult for a record classified as outcome. The
// refusal is copied as is; Refused decides what a reader makes of it.
func resultOf(a Attempt, outcome string, now time.Time) AttemptResult {
	return AttemptResult{
		ID: a.ID, From: a.From, To: a.To, Why: a.Why,
		Outcome: outcome, Stage: a.Stage, RolledBack: a.RolledBack,
		Warnings: outcome == OutcomeApplied && a.Error != "",
		Error:    a.Error, Refusal: a.Refusal, Signature: a.Signature, LogPath: a.LogPath,
		StartedAt: a.StartedAt, FinishedAt: a.FinishedAt, FoldedAt: now,
	}
}

// Refused is the gate refusal behind a failed attempt, a Refusal* word, or ""
// when there is none this release knows: no refusal, an outcome other than
// failed, or a newer updater's word -- every one of which reads as a plain
// failure. The fold itself never reads it: a refused attempt backs off like
// any other failure before anything changed, so blocks, backoff and the pause
// come out the same on every release.
func (r AttemptResult) Refused() string {
	if r.Outcome != OutcomeFailed || !KnownRefusal(r.Refusal) {
		return ""
	}
	return r.Refusal
}

// SignatureRefused is the Signature* word behind a failed attempt that
// update.release_signers refused, or "" when there is none this release
// knows: no word, an outcome other than failed, or a newer updater's word.
func (r AttemptResult) SignatureRefused() string {
	if r.Outcome != OutcomeFailed || r.Stage != StageVerify || !KnownSignature(r.Signature) {
		return ""
	}
	return r.Signature
}

// applyOutcome records res as the last attempt and applies what follows from
// it to st, returning whether it is a failure worth an update.failed event.
// Only an attempt a daemon started (WhyAuto, WhyNow) has consequences; a
// manual one is only shown.
//
//   - applied, unverified: the backoff, the rollback count, the installer
//     failure count and the pause all clear. update.applied was recorded when
//     the new version started, so nothing more is.
//   - rolled back: To is blocked. For WhyAuto, To joins the rollbacks in a
//     row, and rollbacksToPause distinct ones pause automatic updates. (Update
//     now is attended: the owner sees its failure and does not need auto
//     turned off for it.)
//   - failed in StageVerify: To is blocked -- a bad signature, identity,
//     checksum or pin, or a release pulled since, is not retried; one
//     update.release_signers refused (SignatureRefused) is blocked as
//     BlockSignature.
//   - failed in StageInstall: backoff, and installFailuresToBlock in a row on
//     the same To block it.
//   - failed before anything changed, or never started: backoff, no block.
//   - broken: To is blocked and automatic updates pause.
//   - interrupted: backoff, no block (plan 2.4: an hour first).
//   - superseded: nothing.
//
// recount is false for a late record replacing an "interrupted" fold of the
// same attempt (laptop sleep, a Windows rename retried): that fold already
// counted the attempt toward the backoff, so this one only adds what the real
// outcome adds beyond it.
func applyOutcome(st *State, res AttemptResult, now time.Time, recount bool) (failed bool) {
	st.LastAttempt = &res
	if w := res.SignatureRefused(); w != "" {
		st.SignatureRefusal = &SignatureRefusal{Version: res.To, From: res.From, Signature: w, Attempt: res.ID, At: now}
	}
	if !spawnedByDaemon(res.Why) {
		return false
	}
	switch res.Outcome {
	case OutcomeApplied, OutcomeUnverified:
		st.Backoff, st.Rollbacks, st.InstallFailures, st.Paused = nil, nil, nil, nil
		return false
	case OutcomeSuperseded:
		return false
	case OutcomeRolledBack:
		st.block(res.To, BlockRolledBack, res.ID, now)
		if res.Why == WhyAuto && !slices.Contains(st.Rollbacks, res.To) {
			st.Rollbacks = append(st.Rollbacks, res.To)
		}
		if len(st.Rollbacks) >= rollbacksToPause {
			st.pause(PauseRollbacks, slices.Clone(st.Rollbacks[len(st.Rollbacks)-rollbacksToPause:]), now)
		}
	case OutcomeBroken:
		st.block(res.To, BlockBroken, res.ID, now)
		st.pause(PauseBroken, []Version{res.To}, now)
	case OutcomeFailed:
		switch res.Stage {
		case StageVerify:
			reason := BlockVerify
			if res.SignatureRefused() != "" {
				reason = BlockSignature
			}
			st.block(res.To, reason, res.ID, now)
		case StageInstall:
			if recount {
				st.backoff(res.To, res.Outcome, now)
			}
			if f := st.InstallFailures; f != nil && f.Version == res.To {
				f.Count++
			} else {
				st.InstallFailures = &InstallFailures{Version: res.To, Count: 1}
			}
			if st.InstallFailures.Count >= installFailuresToBlock {
				st.block(res.To, BlockInstall, res.ID, now)
				st.InstallFailures = nil
			}
		default:
			if recount {
				st.backoff(res.To, res.Outcome, now)
			}
		}
	case OutcomeInterrupted:
		if recount {
			st.backoff(res.To, res.Outcome, now)
		}
	}
	return true
}

// block adds (or refreshes) a block on v, keeping at most maxBlocks.
func (s *State) block(v Version, reason, attempt string, now time.Time) {
	b := Block{Version: v, Reason: reason, Attempt: attempt, At: now}
	if i := slices.IndexFunc(s.Blocks, func(x Block) bool { return x.Version == v }); i >= 0 {
		s.Blocks[i] = b
		return
	}
	s.Blocks = append(s.Blocks, b)
	if n := len(s.Blocks); n > maxBlocks {
		s.Blocks = slices.Clone(s.Blocks[n-maxBlocks:])
	}
}

// pause sets the auto-off latch; a broken install outranks rollbacks, and an
// existing pause keeps its time.
func (s *State) pause(reason string, versions []Version, now time.Time) {
	if p := s.Paused; p != nil && (p.Reason == reason || p.Reason == PauseBroken) {
		return
	}
	s.Paused = &Pause{Reason: reason, Versions: versions, At: now}
}

// backoff counts one more failure in a row on to and schedules the next
// attempt after backoffWait; a failure on a different release than the
// running count's starts it over.
func (s *State) backoff(to Version, reason string, now time.Time) {
	n := 1
	if b := s.Backoff; b != nil && b.Version == to {
		n = b.Count + 1
	}
	s.Backoff = &Backoff{Until: now.Add(backoffWait(n)), Count: n, Version: to, Reason: reason}
}

// tidy drops what the running version made moot: a pending deadline tied to
// another version and, once no spawn is unsettled, a hold the running version
// reached and the installer-failure count on a release at or below it. While
// a spawn is unsettled the running version may not last -- the new one
// starts before the updater confirms it, and a rollback brings the old one
// back -- so a hold survives an update that rolls back. Blocks are never
// dropped by version: one at or below the running version is inert (Target
// only looks above it, and Status lists only those), and keeping it means a
// rollback cannot resurrect a release that failed before; maxBlocks bounds
// the list. It reports whether anything changed.
func (s *State) tidy(running Version) bool {
	changed := false
	if p := s.Pending; p != nil && p.Running != running {
		s.Pending, changed = nil, true
	}
	if s.Spawn != nil {
		return changed
	}
	if f := s.InstallFailures; f != nil && f.Version.Compare(running) <= 0 {
		s.InstallFailures, changed = nil, true
	}
	if h := s.Hold; h != nil && running.Compare(h.Through) >= 0 {
		s.Hold, changed = nil, true
	}
	return changed
}
