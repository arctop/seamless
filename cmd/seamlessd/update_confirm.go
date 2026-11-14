package main

// Confirming an update by observation, not by the installer's exit code: the
// release counts as installed only once the daemon answering this install's
// /healthz is a NEW process (an instance id that did not serve before the
// install) of that release, the same process the update state names as
// running -- which rules out another user's daemon on the same port -- and it
// stays the same process across two polls at least ten seconds apart, which
// rules out one that starts and then crash-loops.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"net/http"
	"os"
	"path/filepath"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/update"
)

// confirmTiming paces a confirmation.
type confirmTiming struct {
	// poll is the time between /healthz requests, and request one request's
	// whole timeout.
	poll    time.Duration
	request time.Duration
	// stable is how far apart two polls must see the same new process.
	stable time.Duration
	// deadline is how long the new release has to show up; starting extends
	// it while a new daemon is alive but not listening yet: it holds the data
	// dir's lock (seamlessd.lock) from before it opens the store, and the
	// store migration and the corpus reconcile both run before its listener.
	deadline time.Duration
	starting time.Duration
}

// defaultConfirmTiming is the plan's confirmation: polls 2s apart, stable for
// 10s, within 120s -- or 5 minutes while a new daemon is starting.
var defaultConfirmTiming = confirmTiming{
	poll: 2 * time.Second, request: 5 * time.Second,
	stable: 10 * time.Second, deadline: 120 * time.Second, starting: 5 * time.Minute,
}

// healthAnswer is the part of a /healthz body a confirmation reads.
type healthAnswer struct {
	Version  string `json:"version"`
	Instance string `json:"instance"`
}

// probeHealthz asks the daemon at baseURL for /healthz. Only a 200 counts: a
// 503 is a daemon whose database does not answer, which is not one that
// came up.
func probeHealthz(ctx context.Context, client *http.Client, baseURL string) (healthAnswer, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, baseURL+"/healthz", nil)
	if err != nil {
		return healthAnswer{}, err
	}
	resp, err := client.Do(req)
	if err != nil {
		return healthAnswer{}, err
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return healthAnswer{}, fmt.Errorf("/healthz answered %s", resp.Status)
	}
	var a healthAnswer
	if err := json.NewDecoder(io.LimitReader(resp.Body, 64<<10)).Decode(&a); err != nil {
		return healthAnswer{}, fmt.Errorf("decode /healthz: %w", err)
	}
	return a, nil
}

// servedRelease is the release a /healthz version names: a release build
// serves "0.8.0+1a2b3c4" (version + commit), so the build suffix goes, the
// same reading docs/install's served_version makes. ok is false for anything
// that is not then a clean X.Y.Z.
func servedRelease(v string) (update.Version, bool) {
	base, _, _ := strings.Cut(strings.TrimSpace(v), "+")
	return update.Parse(base)
}

// dataDirLockHolder reports whether a process holds the data dir's
// one-daemon lock, and the PID it recorded when one is readable, without ever
// holding the lock across anything: the file is opened read-only and never
// created, and a lock this probe manages to take is released at once. A
// starting daemon that finds it taken for that instant retries 100ms later
// (dataDirLockPoll), well inside its 30s wait.
//
// No lock file is no holder. A lock that cannot be tried at all (a file
// system that refuses locking, an unreadable file) reads as held by an
// unknown PID: "free" is a claim a caller may move the database on, so only a
// lock this probe actually took proves it.
func dataDirLockHolder(dataDir string) (held bool, pid int) {
	path := filepath.Join(dataDir, dataDirLockName)
	f, err := os.Open(path)
	if errors.Is(err, os.ErrNotExist) {
		return false, 0
	}
	if err != nil {
		return true, 0
	}
	defer func() { _ = f.Close() }()
	if err := lockFile(f); err != nil {
		if errors.Is(err, errLockHeld) {
			pid, _ := lockHolderPID(path)
			return true, pid
		}
		return true, 0
	}
	_ = unlockFile(f) //nolint:errcheck // the close below drops the lock regardless
	return false, 0
}

// confirmParams is one confirmation to run.
type confirmParams struct {
	want    update.Version // the release that must be serving
	client  *http.Client   // cfg.HTTPClient: the daemon dial, tls.ca_file included
	baseURL string         // cfg.ServerURL()
	dataDir string
	// prevInstance served before the install and so never confirms it;
	// prevPID is its PID: the data dir lock held by any other PID is a new
	// daemon starting.
	prevInstance string
	prevPID      int
	timing       confirmTiming
	now          func() time.Time
	ticker       func(time.Duration) (<-chan time.Time, func())
	// starting reports a new daemon alive but not listening yet; nil reads the
	// data dir's lock (dataDirLockHolder).
	starting func() bool
}

// confirmResult is a confirmation's verdict.
type confirmResult struct {
	ok bool
	// instance is the confirmed process, or the last new one seen.
	instance string
	// reason says why not, from the last poll, built from parsed versions.
	reason string
}

// confirmServing polls until want is confirmed serving -- the base version
// /healthz serves (+commit stripped) is want; its instance is not
// prevInstance and is the one update.ReadState names as Running, with
// Running.Version want; and the same instance answers again at least
// timing.stable later with no failed poll between -- or until the deadline.
func confirmServing(ctx context.Context, p confirmParams) confirmResult {
	starting := p.starting
	if starting == nil {
		starting = func() bool {
			held, pid := dataDirLockHolder(p.dataDir)
			return held && pid != 0 && pid != p.prevPID
		}
	}
	tick, stop := p.ticker(p.timing.poll)
	defer stop()

	start := p.now()
	var (
		firstAt  time.Time
		firstID  string
		lastSeen string
		reason   = "no poll ran"
	)
	for {
		ans, err := probeHealthz(ctx, p.client, p.baseURL)
		now := p.now()
		matched := false
		switch got, ok := servedRelease(ans.Version); {
		case err != nil:
			reason = fmt.Sprintf("no answer from %s/healthz: %v", config.RedactedURL(p.baseURL), err)
		case !ok:
			reason = fmt.Sprintf("%s/healthz serves a version that is not a release", config.RedactedURL(p.baseURL))
		case got != p.want:
			reason = fmt.Sprintf("%s/healthz serves v%s, not v%s", config.RedactedURL(p.baseURL), got, p.want)
		case ans.Instance == "" || ans.Instance == p.prevInstance:
			reason = fmt.Sprintf("v%s answers, but not from a process started since the install", p.want)
		default:
			lastSeen = ans.Instance
			st, serr := update.ReadState(p.dataDir)
			switch {
			case serr != nil:
				reason = fmt.Sprintf("read the update state: %v", serr)
			case st.Running.Instance != ans.Instance || st.Running.Version != p.want:
				reason = fmt.Sprintf("the update state does not name the v%s process answering /healthz as the running daemon", p.want)
			default:
				matched = true
			}
		}
		if matched {
			if firstID == ans.Instance {
				if now.Sub(firstAt) >= p.timing.stable {
					return confirmResult{ok: true, instance: ans.Instance}
				}
			} else {
				firstAt, firstID = now, ans.Instance
			}
		} else {
			firstAt, firstID = time.Time{}, ""
		}

		elapsed := now.Sub(start)
		finishing := firstID != "" && now.Sub(firstAt) < p.timing.stable+2*p.timing.poll
		if elapsed >= p.timing.deadline && !finishing {
			if elapsed >= p.timing.starting || !starting() {
				return confirmResult{instance: lastSeen, reason: fmt.Sprintf("not confirmed after %s: %s", elapsed.Round(time.Second), reason)}
			}
		}
		select {
		case <-ctx.Done():
			return confirmResult{instance: lastSeen, reason: ctx.Err().Error()}
		case <-tick:
		}
	}
}
