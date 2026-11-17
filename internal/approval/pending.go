package approval

import "time"

// maxPending caps each kind of ceremony the Service holds in memory between
// its requests -- passkey enrollments, lock changes, restore approvals -- so a
// caller looping on a begin route cannot grow any of them without bound.
const maxPending = 32

// pendingEntry is one ceremony waiting between its requests: the scope a cap
// counts it in, and the deadline it waits until.
type pendingEntry[S comparable] interface {
	scope() S
	deadline() time.Time
}

// makeRoom drops from pending, under the Service's mu, the ceremony a new one
// in scope own displaces, so the new one fits: own's oldest when own holds
// perScope, else, when maxPending wait in all, the oldest of the scope holding
// the most, the one whose oldest is oldest on a tie. Every entry of a kind
// waits as long, so the earlier deadline is the older, then the smaller id.
//
// A begin never waits for room. A caller looping on begin crowds out only the
// scope it loops on, and only while it keeps looping: no burst holds every
// ceremony of the kind off until its entries expire, as a refusal once full
// did.
func makeRoom[S comparable, P pendingEntry[S]](pending map[string]P, own S, perScope int) {
	older := func(a, b string) bool {
		da, db := pending[a].deadline(), pending[b].deadline()
		return da.Before(db) || (da.Equal(db) && a < b)
	}
	sizes := map[S]int{}
	oldest := map[S]string{}
	for id, p := range pending {
		sc := p.scope()
		sizes[sc]++
		if o, ok := oldest[sc]; !ok || older(id, o) {
			oldest[sc] = id
		}
	}
	if sizes[own] >= perScope {
		delete(pending, oldest[own])
		return
	}
	if len(pending) < maxPending {
		return
	}
	var victim S
	found := false
	for sc, n := range sizes {
		if !found || n > sizes[victim] || (n == sizes[victim] && older(oldest[sc], oldest[victim])) {
			victim, found = sc, true
		}
	}
	delete(pending, oldest[victim])
}
