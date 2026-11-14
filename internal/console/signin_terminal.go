package console

// The terminal sign-in (console.sign_in: passkey): the way in for an owner
// with no passkey left, in the browser-first pattern of a terminal unlock
// (unlock.go). The login page begins it: the daemon sets this browser a fresh
// seamless_console cookie, bound together with the browser's key in a row that
// waits for its code (store/signin_terminal.go), and the page shows the code
// and the command that approves it. The owner runs that command in a terminal
// on the server, as the daemon's account under peer_auth (`seamlessd
// console-open --sign-in --code`); the page, asking after its row with its
// cookie and its proof, then finds itself signed in. Nothing the terminal
// shows is a credential: the code names the row, and the row signs in only
// the browser holding both the cookie and the key it was begun with
// (constraint console-unlock-begun-by-the-browser-it-opens).
//
// Both routes sit behind signInRoute: a 404 while the key signs browsers in,
// the owner only, never over the socket, and a write only from this origin.
//
// signInStateJSON is setup's view of the daemon's sign-in (`seamlessd setup`,
// "Sign in with passkeys and retire the key"): the owner's own process asks it
// over the socket, as the daemon's config is not the owner's to read.

import (
	"errors"
	"net/http"
	"net/url"
	"strconv"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/store"
)

// terminalSignInCommand is the server's half of a terminal sign-in: the
// approval of the code the page shows, as the daemon's account when the
// daemon knows how that is reached.
func (s *Service) terminalSignInCommand(code string) string {
	return s.daemonCommand("console-open --sign-in --code " + code)
}

// terminalSignInBegin begins a terminal sign-in for the browser whose key the
// request proves, under a fresh cookie, and answers the code and the command.
// A cookie the browser held before ends with it: it is signing in again.
func (s *Service) terminalSignInBegin(w http.ResponseWriter, r *http.Request) {
	key, ok := s.signInKeyOf(w, r)
	if !ok {
		return
	}
	ctx := r.Context()
	begun, err := s.cfg.SignIn.BeginTerminalSignIn(ctx, key)
	if err != nil {
		s.signInError(w, r, err)
		return
	}
	if c, cerr := r.Cookie(cookieName); cerr == nil && c.Value != "" {
		if _, err := store.EndConsoleSignInByCookie(ctx, s.writeDB(), c.Value, time.Now()); err != nil {
			s.logger.Warn("console: end the browser's earlier sign-in", "error", err)
		}
	}
	// The cookie lasts as long as the sign-in could: the code's wait, then a
	// whole sign-in, so a page closed before it saw the approval still finds
	// itself signed in. Once approved, the status answer trims it to the
	// sign-in's own expiry. Unapproved, it names a row that never goes live.
	s.signInCookie(w, begun.Cookie, time.Until(begun.PendingUntil)+s.cfg.SignIn.Lifetime())
	writeJSON(w, http.StatusOK, map[string]any{
		"id": begun.ID, "code": begun.Code, "command": s.terminalSignInCommand(begun.Code),
		"pending_until": begun.PendingUntil.UTC().Format(time.RFC3339),
		"minutes":       int(store.TerminalSignInTTL / time.Minute),
	})
}

// terminalSignInStatus answers the page waiting on its code with the state of
// the terminal sign-in it began (?id=), read with its own cookie and proof:
// another browser, or this cookie without its key, finds nothing. Once the code
// is approved the answer carries the cookie again, valid until the sign-in
// expires, and where to go.
func (s *Service) terminalSignInStatus(w http.ResponseWriter, r *http.Request) {
	key, ok := s.signInKeyOf(w, r)
	if !ok {
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	cookie := ""
	if c, err := r.Cookie(cookieName); err == nil {
		cookie = c.Value
	}
	row, state, err := s.cfg.SignIn.TerminalSignInState(r.Context(), r.URL.Query().Get("id"), cookie, key)
	if errors.Is(err, store.ErrTerminalSignInNotFound) {
		writeJSON(w, http.StatusNotFound, map[string]string{"state": string(store.TerminalSignInExpired),
			"error": "this browser began no terminal sign-in that is still waiting: start again for a new code"})
		return
	}
	if err != nil {
		s.signInError(w, r, err)
		return
	}
	if state != store.TerminalSignInLive {
		writeJSON(w, http.StatusOK, map[string]string{"state": string(state)})
		return
	}
	s.signInCookie(w, cookie, time.Until(row.ExpiresAt))
	writeJSON(w, http.StatusOK, map[string]any{
		"state": string(state), "next": safeNext(r.URL.Query().Get("next")),
		"expires_at": row.ExpiresAt.UTC().Format(time.RFC3339),
	})
}

// SignInState is what GET /console/signin/state answers the owner: how the
// console signs a browser in, whether the daemon still holds the bearer key,
// the passkeys that can sign in, and the passkey sign-ins made since a time.
type SignInState struct {
	// SignIn is config.ConsoleSignInPasskey or config.ConsoleSignInKey.
	SignIn string `json:"sign_in"`
	// Key is whether the daemon holds a bearer key (mcp.api_key).
	Key bool `json:"key"`
	// RPID and Origin are the passkeys' relying party, "" when the daemon
	// has none (no peer_auth and no passkey sign-in).
	RPID   string `json:"rp_id,omitempty"`
	Origin string `json:"origin,omitempty"`
	// Passkeys counts the enrolled passkeys made for RPID, and UnknownRP those
	// enrolled before a passkey's relying party was recorded.
	Passkeys  int `json:"passkeys"`
	UnknownRP int `json:"passkeys_unknown_rp"`
	// SignIns counts the passkey sign-ins made at or after ?since (unix
	// seconds; 0 when absent).
	SignIns int `json:"passkey_sign_ins"`
}

// signInStateJSON answers SignInState: owner-only (it sits behind auth), and
// read on the main handle, as every sign-in read is.
func (s *Service) signInStateJSON(w http.ResponseWriter, r *http.Request) {
	ctx := r.Context()
	st := SignInState{SignIn: config.ConsoleSignInKey, Key: strings.TrimSpace(s.cfg.APIKey) != ""}
	var origins []string
	switch {
	case s.cfg.SignIn != nil:
		st.SignIn = config.ConsoleSignInPasskey
		st.RPID, origins = s.cfg.SignIn.RPID(), s.cfg.SignIn.Origins()
	case s.cfg.Approvals != nil:
		origins = s.cfg.Approvals.Origins()
	}
	if len(origins) > 0 {
		st.Origin = origins[0]
		if st.RPID == "" {
			if u, err := url.Parse(origins[0]); err == nil {
				st.RPID = strings.ToLower(u.Hostname())
			}
		}
	}
	creds, err := store.ApproverCredentials(ctx, s.writeDB(), store.ApproverPasskey)
	if err != nil {
		s.serverErrorBare(w, r, err)
		return
	}
	for _, c := range creds {
		switch {
		case c.RPID == "":
			st.UnknownRP++
		case st.RPID != "" && strings.EqualFold(c.RPID, st.RPID):
			st.Passkeys++
		}
	}
	if raw := r.URL.Query().Get("since"); raw != "" {
		sec, perr := strconv.ParseInt(raw, 10, 64)
		if perr != nil || sec < 0 {
			writeJSON(w, http.StatusBadRequest, map[string]string{"error": "since is a time in unix seconds"})
			return
		}
		if st.SignIns, err = store.PasskeySignInsSince(ctx, s.writeDB(), time.Unix(sec, 0)); err != nil {
			s.serverErrorBare(w, r, err)
			return
		}
	}
	w.Header().Set("Cache-Control", "no-store")
	writeJSON(w, http.StatusOK, st)
}
