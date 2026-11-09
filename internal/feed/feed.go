package feed

import (
	"bytes"
	"context"
	"crypto/rand"
	"database/sql"
	"encoding/hex"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"log/slog"
	"net"
	"net/http"
	"os"
	"path/filepath"
	"regexp"
	"strings"
	"sync"
	"time"

	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/events"
	"github.com/arctop/seamless/internal/store"
)

// Config wires a Feed.
type Config struct {
	DB     *sql.DB
	Events *events.Recorder
	// Socket is the consumer's Unix socket (screening.feed_socket); "" means
	// the feed never starts.
	Socket string
	// Spool is the spool folder (screening.feed_spool, else <data_dir>/feed).
	Spool string
	// Host is the host name frames carry; it must be a lowercase host name.
	Host string
	// Enabled reports whether the screening feature is on: the feed spools
	// nothing while it is off.
	Enabled func(context.Context) bool
	// Locked reports whether peer_auth locks a project; such a project's slug
	// leaves only as a keyed hash, like a confidential or sealed project's. nil
	// means none.
	Locked func(project string) bool
	Logger *slog.Logger
	Now    func() time.Time
}

// Feed is the metadata feed: one goroutine tails the event log by a
// persisted cursor, maps each event to a frame, spools it, and delivers the
// spool to the consumer, one batch in flight at a time.
//
// It tails the log rather than reading the live fan-out: Recorder.Subscribe
// drops events for a subscriber whose channel is full, and a security feed
// must not lose one silently. The subscription is only the wake-up.
type Feed struct {
	cfg    Config
	client *http.Client
	mapper mapper

	limits  Limits
	runDone chan struct{}

	// The loop's timing (the constants below; tests shorten them).
	pullEvery    time.Duration
	backoffFloor time.Duration
	backoffCeil  time.Duration

	isoMu    sync.Mutex
	isoCache map[string]bool
	isoAt    time.Time
}

// New builds a Feed. It touches nothing until Run.
func New(cfg Config) *Feed {
	if cfg.Logger == nil {
		cfg.Logger = slog.Default()
	}
	if cfg.Now == nil {
		cfg.Now = time.Now
	}
	host := strings.ToLower(strings.TrimSpace(cfg.Host))
	if !hostRe.MatchString(host) || len(host) > 128 {
		host = "localhost"
	}
	f := &Feed{cfg: cfg, limits: DefaultLimits, pullEvery: pullEvery, backoffFloor: backoffFloor, backoffCeil: backoffCeiling}
	f.mapper = mapper{host: host, hashed: f.hashedProject}
	sock := cfg.Socket
	f.client = &http.Client{
		Timeout: 30 * time.Second,
		Transport: &http.Transport{
			DialContext: func(ctx context.Context, _, _ string) (net.Conn, error) {
				var d net.Dialer
				return d.DialContext(ctx, "unix", sock)
			},
			// One request per connection: a consumer closes an idle connection
			// after a few seconds and caps how many are open, and a fresh dial
			// on a local socket costs next to nothing.
			DisableKeepAlives: true,
		},
	}
	return f
}

// hashedProject reports whether a project's slug leaves only as a keyed hash:
// a confidential or sealed project, or a locked one. The isolation states are
// cached for a minute; an unreadable state hashes.
func (f *Feed) hashedProject(slug string) bool {
	if slug == "" {
		return false
	}
	if f.cfg.Locked != nil && f.cfg.Locked(slug) {
		return true
	}
	f.isoMu.Lock()
	defer f.isoMu.Unlock()
	now := f.cfg.Now()
	if f.isoCache == nil || now.Sub(f.isoAt) > time.Minute {
		f.isoCache, f.isoAt = map[string]bool{}, now
	}
	if v, ok := f.isoCache[slug]; ok {
		return v
	}
	iso, err := store.IsolationOf(context.Background(), f.cfg.DB, slug)
	hashed := err != nil || iso.FencesOutbound()
	f.isoCache[slug] = hashed
	return hashed
}

// Run starts the feed's loop when a socket is configured; Wait blocks until
// it returns after ctx ends.
func (f *Feed) Run(ctx context.Context) error {
	if f.cfg.Socket == "" {
		return nil
	}
	sp, err := openSpool(f.cfg.Spool)
	if err != nil {
		return err
	}
	key, err := loadKey(filepath.Join(f.cfg.Spool, "hash.key"))
	if err != nil {
		sp.close()
		return err
	}
	f.mapper.key = key
	f.runDone = make(chan struct{})
	go func() {
		defer close(f.runDone)
		defer sp.close()
		f.loop(ctx, sp)
	}()
	return nil
}

// Wait blocks until the loop has returned (at once when it never ran).
func (f *Feed) Wait() {
	if f.runDone != nil {
		<-f.runDone
	}
}

// loadKey reads the keyed-hash key, making it (32 random bytes, 0600) the
// first time. One key per spool keeps a project's hash stable across
// restarts.
func loadKey(path string) ([]byte, error) {
	raw, err := os.ReadFile(path)
	if err == nil {
		key, derr := hex.DecodeString(strings.TrimSpace(string(raw)))
		if derr != nil || len(key) < 32 {
			return nil, fmt.Errorf("feed: %s is not a 32-byte hex key; remove it to make a new one (project hashes change)", path)
		}
		return key, nil
	}
	if !errors.Is(err, fs.ErrNotExist) {
		return nil, fmt.Errorf("feed: hash key: %w", err)
	}
	key := make([]byte, 32)
	if _, err := rand.Read(key); err != nil {
		return nil, fmt.Errorf("feed: hash key: %w", err)
	}
	if err := os.WriteFile(path, []byte(hex.EncodeToString(key)+"\n"), 0o600); err != nil {
		return nil, fmt.Errorf("feed: hash key: %w", err)
	}
	return key, nil
}

// Loop timing.
const (
	pullEvery      = 5 * time.Second  // tail the log at least this often (the subscription wakes it sooner)
	pullBatch      = 500              // events read per query
	limitsEvery    = time.Hour        // re-read the consumer's limits
	backoffFloor   = 5 * time.Second  // first wait after the consumer is unreachable
	backoffCeiling = 5 * time.Minute  // longest wait
	maxBatchesTurn = 20               // batches delivered per turn before tailing again
	minHeartbeat   = 10 * time.Second // never heartbeat more often than this
)

func (f *Feed) loop(ctx context.Context, sp *spool) {
	var wake <-chan core.Event
	if f.cfg.Events != nil {
		ch, unsub := f.cfg.Events.Subscribe()
		defer unsub()
		wake = ch
	}
	tick := time.NewTicker(f.pullEvery)
	defer tick.Stop()
	var nextDelivery, nextHeartbeat, nextLimits time.Time
	backoff := time.Duration(0)
	for {
		now := f.cfg.Now()
		active := f.cfg.Enabled != nil && f.cfg.Enabled(ctx)
		if active {
			if !now.Before(nextLimits) {
				f.readLimits(ctx)
				nextLimits = now.Add(limitsEvery)
			}
			if err := f.pull(ctx, sp); err != nil {
				f.cfg.Logger.Warn("feed: tail the event log", "error", err)
			}
			if !now.Before(nextHeartbeat) {
				every := max(time.Duration(f.limits.HeartbeatS)*time.Second/3, minHeartbeat)
				if sp.empty() {
					if _, err := sp.add(f.mapper.heartbeat(now, every, sp.stats())); err != nil {
						f.cfg.Logger.Warn("feed: spool heartbeat", "error", err)
					}
				}
				nextHeartbeat = now.Add(every)
			}
			if !now.Before(nextDelivery) {
				out := f.deliver(ctx, sp)
				switch out.outcome {
				case "unreachable", "retry":
					backoff = min(max(backoff*2, f.backoffFloor), f.backoffCeil)
					if out.retryAfter > 0 {
						backoff = min(max(out.retryAfter, f.backoffFloor), f.backoffCeil)
					}
					nextDelivery = now.Add(backoff)
				default:
					backoff = 0
					nextDelivery = time.Time{}
				}
			}
		} else if err := f.idle(sp); err != nil {
			f.cfg.Logger.Warn("feed: mark idle", "error", err)
		}
		select {
		case <-ctx.Done():
			return
		case _, ok := <-wake:
			if !ok {
				wake = nil
			}
		case <-tick.C:
		}
	}
}

// idle records that the feed is off, so that when it comes back it starts
// from the newest event rather than replaying what happened while it was off.
func (f *Feed) idle(sp *spool) error {
	if !sp.st.Active {
		return nil
	}
	return sp.setCursor(sp.st.CursorRow, sp.st.CursorID, false)
}

// pull spools every event after the cursor, in the order the log took them. A
// feed coming back from off (or starting for the first time) starts at the
// newest event.
func (f *Feed) pull(ctx context.Context, sp *spool) error {
	if !sp.st.Active {
		row, id, err := f.cfg.Events.NewestRow(ctx)
		if err != nil {
			return err
		}
		return sp.setCursor(row, id, true)
	}
	if err := f.checkCursor(ctx, sp); err != nil {
		return err
	}
	for {
		evs, err := f.cfg.Events.Tail(ctx, sp.st.CursorRow, pullBatch)
		if err != nil {
			return err
		}
		if len(evs) == 0 {
			return nil
		}
		sessions := f.sessionsOf(ctx, evs)
		for _, e := range evs {
			fr := f.mapper.frame(e.Event, sessions[e.SessionID])
			if verr := fr.validate(); verr != nil {
				f.cfg.Logger.Warn("feed: frame refused before spooling", "kind", string(e.Kind), "error", verr)
				sp.mu.Lock()
				sp.st.Dropped++
				sp.mu.Unlock()
				continue
			}
			if _, err := sp.add(fr); err != nil {
				return err
			}
		}
		last := evs[len(evs)-1]
		if err := sp.setCursor(last.Row, last.ID, true); err != nil {
			return err
		}
		if len(evs) < pullBatch {
			return nil
		}
	}
}

// checkCursor makes sure the cursor's row still holds the event it named. A
// log that was rewritten (a restore from an archive, a VACUUM) can renumber
// rows: the cursor is then re-found by the event's id, or, when that event is
// gone too, moved to the newest event -- what happened in between cannot be
// told apart from what was already sent, so it is skipped and logged rather
// than sent twice or left to stall the feed.
func (f *Feed) checkCursor(ctx context.Context, sp *spool) error {
	if sp.st.CursorRow == 0 || sp.st.CursorID == "" {
		return nil
	}
	id, err := f.cfg.Events.IDAt(ctx, sp.st.CursorRow)
	if err != nil || id == sp.st.CursorID {
		return err
	}
	row, found, err := f.cfg.Events.RowOf(ctx, sp.st.CursorID)
	if err != nil {
		return err
	}
	if found {
		return sp.setCursor(row, sp.st.CursorID, true)
	}
	newest, newestID, err := f.cfg.Events.NewestRow(ctx)
	if err != nil {
		return err
	}
	f.cfg.Logger.Warn("feed: the event log was rewritten and the cursor's event is gone; resuming at the newest event")
	return sp.setCursor(newest, newestID, true)
}

// sessionsOf resolves the sessions a run of events names: display name and
// agent process.
func (f *Feed) sessionsOf(ctx context.Context, evs []events.Logged) map[string]session {
	out := map[string]session{}
	var ids []string
	for _, e := range evs {
		if e.SessionID != "" {
			if _, ok := out[e.SessionID]; !ok {
				out[e.SessionID] = session{}
				ids = append(ids, e.SessionID)
			}
		}
	}
	if len(ids) == 0 {
		return out
	}
	procs, err := store.SessionAgentProcesses(ctx, f.cfg.DB, ids)
	if err != nil {
		f.cfg.Logger.Warn("feed: session lookup", "error", err)
		return out
	}
	for id, p := range procs {
		out[id] = session{name: p.Name, agentProcess: p.AgentProcess}
	}
	return out
}

// deliverOutcome is what one delivery turn came to.
type deliverOutcome struct {
	outcome    string // empty, more, retry, unreachable
	delivered  int
	discarded  int
	retryAfter time.Duration
}

// deliver sends the spool's batches, oldest first, until it is empty, the
// turn's quota is spent, the consumer asks for a retry, or it cannot be
// reached.
func (f *Feed) deliver(ctx context.Context, sp *spool) deliverOutcome {
	var out deliverOutcome
	for out.delivered+out.discarded < maxBatchesTurn {
		b, err := sp.next()
		if err != nil {
			f.cfg.Logger.Warn("feed: spool", "error", err)
			out.outcome = "retry"
			return out
		}
		if b == nil {
			out.outcome = "empty"
			return out
		}
		status, body, err := f.post(ctx, b.body)
		if err != nil {
			out.outcome = "unreachable"
			return out
		}
		r := readReceipt(status, body, b.id)
		act, err := sp.settle(r)
		if err != nil {
			f.cfg.Logger.Warn("feed: spool settle", "error", err)
			out.outcome = "retry"
			return out
		}
		switch act {
		case ActionSettle:
			out.delivered++
		case ActionDiscard:
			out.discarded++
			f.cfg.Logger.Warn("feed: the consumer discarded a batch", "batch", b.id, "rule", r.Rule)
		default:
			out.outcome = "retry"
			out.retryAfter = time.Duration(r.RetryAfterSec) * time.Second
			return out
		}
	}
	out.outcome = "more"
	return out
}

func (f *Feed) post(ctx context.Context, body []byte) (int, []byte, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodPost, "http://localhost"+BatchPath, bytes.NewReader(body))
	if err != nil {
		return 0, nil, err
	}
	req.Header.Set("Content-Type", NDJSON)
	resp, err := f.client.Do(req)
	if err != nil {
		return 0, nil, err
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 1<<20))
	if err != nil {
		return 0, nil, err
	}
	return resp.StatusCode, raw, nil
}

// readLimits asks the consumer for its limits; a failure keeps the ones in
// force.
func (f *Feed) readLimits(ctx context.Context) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, "http://localhost"+LimitsPath, nil)
	if err != nil {
		return
	}
	resp, err := f.client.Do(req)
	if err != nil {
		return
	}
	defer func() { _ = resp.Body.Close() }()
	raw, err := io.ReadAll(io.LimitReader(resp.Body, 64<<10))
	if err != nil || resp.StatusCode != http.StatusOK {
		return
	}
	l, err := parseLimits(raw)
	if err != nil {
		f.cfg.Logger.Warn("feed: consumer limits unreadable; keeping the defaults", "error", err)
		return
	}
	f.limits = l
}

// hostNameRe is the host part of a host name the feed accepts as configured.
var hostNameRe = regexp.MustCompile(`^[a-z0-9.-]+$`)

// HostName returns host lowercased if it can be a frame's host, else
// "localhost".
func HostName(host string) string {
	h := strings.ToLower(strings.TrimSpace(host))
	if !hostNameRe.MatchString(h) || !hostRe.MatchString(h) {
		return "localhost"
	}
	return h
}
