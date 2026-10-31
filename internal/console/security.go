package console

import (
	"net/http"
	"net/url"
	"strings"

	"github.com/arctop/seamless/internal/access"
)

// This file holds the console's browser-boundary defenses from the 2026-07-19
// audit: response headers every console reply carries (I3) and the origin check
// that guards cookie-authenticated writes (M2).
//
// Both are deliberately header-only. The console is a single-user localhost UI
// with server-rendered forms, and the heavier versions of these controls --
// per-request CSRF tokens threaded through every form, a strict CSP the inline
// JS would have to be rewritten around -- buy little here and cost maintenance
// forever. What follows is the part that is free and invisible.

// setSecurityHeaders applies the response headers that hold for every console
// route, public or authenticated.
//
//   - nosniff: the console serves CSS, JS, SVG, JSON and HTML from the same
//     origin, and agent-authored text ends up inside several of them. Content
//     sniffing is what turns "a memory body that happens to start with <html>"
//     into a document the browser renders in this origin.
//   - DENY: nothing in the console is meant to be embedded, so framing it can
//     only be clickjacking -- and every state change here is a one-click POST
//     (archive a memory, approve a plan, apply a gardener proposal), which is
//     precisely what a clickjack aims at. This is the other half of the M2 fix:
//     the origin check below stops a forged POST, and this stops the operator
//     being tricked into making a genuine one.
//   - no-referrer: console URLs carry memory, note, and session ids in the
//     path, and rendered agent markdown may link off-site. Without this,
//     following such a link hands those ids to a third party in the Referer.
func setSecurityHeaders(h http.Header) {
	h.Set("X-Content-Type-Options", "nosniff")
	h.Set("X-Frame-Options", "DENY")
	h.Set("Referrer-Policy", "no-referrer")
}

// secured wraps a console handler so its response carries the security headers.
// Register routes every console handler -- authenticated or not -- through this,
// so coverage is a property of registration rather than something each new route
// has to remember.
func secured(next http.HandlerFunc) http.HandlerFunc {
	return func(w http.ResponseWriter, r *http.Request) {
		setSecurityHeaders(w.Header())
		next(w, r)
	}
}

// credential names how a request proved itself, which the origin check needs:
// the whole CSRF question is "did the browser attach this credential on its
// own?", and only the cookie is ambient in that way.
type credential int

const (
	credNone credential = iota
	credBearer
	credCookie
)

// classify reports which credential authenticated the request, if any. A
// peer_auth socket request the kernel already authenticated counts as the
// bearer path: no browser can reach a Unix socket, so it carries no CSRF risk.
// Whether that caller may use the console at all is auth's question
// (access.OwnerOnly), not this one's.
func (s *Service) classify(r *http.Request) credential {
	if access.PeerAuthenticated(r.Context()) {
		return credBearer
	}
	key := s.cfg.APIKey
	if key == "" {
		return credNone
	}
	if bearerEquals(r, key) {
		return credBearer
	}
	if cookieEquals(r, key) {
		return credCookie
	}
	return credNone
}

// safeMethod reports whether a method is read-only by definition and so cannot
// be the target of a CSRF write.
func safeMethod(m string) bool {
	switch m {
	case http.MethodGet, http.MethodHead, http.MethodOptions:
		return true
	}
	return false
}

// sameOriginRequest reports whether a request demonstrably originated from the
// console's own origin.
//
// This exists because SameSite=Lax -- the console cookie's only cross-site
// defense until now -- computes "site" from the registrable domain and ignores
// the port. On loopback that makes every 127.0.0.1:<port> in existence
// same-site: any other local dev server, or any page the operator opens from
// one, can POST here and the browser will attach the console cookie. The two
// signals below are the ones that do respect the port.
//
// Preference order is deliberate. Sec-Fetch-Site is set by the browser itself
// and cannot be forged by page script; Origin is the compatibility fallback.
// "none" means the navigation had no initiator at all (a typed URL or a
// bookmark), which no attacker page can produce.
//
// A request with neither header is rejected rather than trusted. Every browser
// that can reach this code sends at least one of them on a POST, so the only
// callers this turns away are scripted ones -- and a scripted client should be
// presenting the bearer key, which skips this check entirely.
func sameOriginRequest(r *http.Request) bool {
	switch r.Header.Get("Sec-Fetch-Site") {
	case "same-origin", "none":
		return true
	case "same-site", "cross-site":
		return false
	}

	origin := r.Header.Get("Origin")
	if origin == "" {
		return false
	}
	u, err := url.Parse(origin)
	if err != nil || u.Host == "" {
		return false
	}
	return strings.EqualFold(u.Host, r.Host)
}
