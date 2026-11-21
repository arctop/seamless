package hooks

import (
	"context"
	"sync"

	"github.com/arctop/seamless/internal/access"
)

// sniffSkipsMax bounds how many conversations sniffSkips remembers. Past it the
// set starts over, so a daemon that serves many remote conversations records
// at most one more skip for each of them: never one a prompt.
const sniffSkipsMax = 4096

// modelSniffOK is localDiskOK for the model sniff (setAmbientModel). The sniff
// runs at a session's start, on every prompt and at every Stop, so a
// conversation on another machine records its skip the first time this daemon
// skips it, and its later prompts and turns are skipped without a record. A
// conversation on this machine, or any under peer_auth (where localDiskOK
// records nothing), takes localDiskOK's answer unchanged.
func (h *Handler) modelSniffOK(ctx context.Context, client Client, host, claudeSessionID string) bool {
	if _, peerAuth := access.FromContext(ctx); !peerAuth && !h.sameHost(host) {
		if !h.sniffSkips.first(liveKey(client, claudeSessionID)) {
			return false
		}
	}
	return h.localDiskOK(ctx, "model-sniff", client, host)
}

// onceSet remembers keys, so a record is made once a key. It holds at most
// sniffSkipsMax keys: a new key that finds it full clears it first. The zero
// value is ready to use.
type onceSet struct {
	mu   sync.Mutex
	seen map[string]struct{}
}

// first reports whether key is new to the set, and adds it.
func (o *onceSet) first(key string) bool {
	o.mu.Lock()
	defer o.mu.Unlock()
	if _, ok := o.seen[key]; ok {
		return false
	}
	if o.seen == nil || len(o.seen) >= sniffSkipsMax {
		o.seen = make(map[string]struct{})
	}
	o.seen[key] = struct{}{}
	return true
}
