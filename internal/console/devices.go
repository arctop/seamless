package console

// The Devices page (plan networked-relay): the computers paired with this
// server, relays and key devices together, how to pair another, and the
// owner's approval of a pairing under peer_auth. It exists only on a server
// that serves other computers (Config.Devices set); on a default install
// every route here is a 404 and no page names it.
//
// A pairing code is minted here (or with `seamlessd devices code` on the
// server) and shown once, in the answer to the request that minted it: it is
// never put in a URL. Without peer_auth the code is the approval, so the page
// says so. Under peer_auth a pairing waits here until the owner approves it
// with a passkey, from a browser on the relying party's origin, after
// comparing the fingerprint with the one the other computer shows.

import (
	"context"
	"errors"
	"net/http"
	"net/url"
	"slices"
	"time"

	"github.com/arctop/seamless/internal/approval"
	"github.com/arctop/seamless/internal/devices"
	"github.com/arctop/seamless/internal/passkey"
	"github.com/arctop/seamless/internal/store"
)

// DevicesView is what the Devices page needs of the daemon's device pairing
// (*devices.Service).
type DevicesView interface {
	List(ctx context.Context) ([]store.Device, error)
	MintCode(ctx context.Context, by string) (devices.Code, error)
	Revoke(ctx context.Context, id, by string) (store.Device, error)
	PeerAuth() bool
	Server() devices.ServerFacts
	// Approvals is the passkey approval under peer_auth, nil otherwise.
	Approvals() devices.Approver
	BeginApproval(ctx context.Context, id string) (devices.Ceremony, error)
	FinishApproval(ctx context.Context, id, ticket string, response []byte) (store.Device, error)
}

// devicesScreen is the Devices page's registry entry. Like the lock screens
// it is not in screens: a console that pairs nothing has no such page, so its
// sidebar, palette and docs matrix never name it. withDevicesScreen places it.
var devicesScreen = screen{ID: "devices", Label: "Devices", Icon: "link", Href: "/console/devices", Group: groupServer, Key: "d",
	Min: levelBasic, NavRow: true}

// withDevicesScreen returns visible with the Devices screen inserted ahead of
// the pinned tail when on, or visible itself.
func withDevicesScreen(visible []screen, on bool) []screen {
	if !on {
		return visible
	}
	return beforeTail(visible, []screen{devicesScreen})
}

// devicesOff reports that this console pairs no devices, having answered a
// 404.
func (s *Service) devicesOff(w http.ResponseWriter, r *http.Request) bool {
	if s.cfg.Devices == nil {
		s.notFound(w, r, "this server pairs no devices: it serves no other computers")
		return true
	}
	return false
}

// deviceRow is one device as the page shows it.
type deviceRow struct {
	store.Device
	// Now is its status as of the request (a pending one past its deadline
	// reads as expired).
	Now string `json:"status_now"`
}

// KindLabel names the device's kind for a person.
func (d deviceRow) KindLabel() string {
	if d.Kind == store.DeviceRelayKind {
		return "Relay"
	}
	return "Key device"
}

// Ident is what the owner recognizes it by: a relay's fingerprint, a key
// device's key prefix (its fingerprint while it waits).
func (d deviceRow) Ident() string {
	if d.Kind == store.DeviceKeyKind && d.KeyPrefix != "" {
		return d.KeyPrefix + "…"
	}
	return d.Fingerprint
}

// ApprovedHow says what approved it.
func (d deviceRow) ApprovedHow() string {
	if d.ApprovedBy != "" {
		return "your passkey"
	}
	return "its one-time code"
}

// devicesData is the Devices page.
type devicesData struct {
	Server   devices.ServerFacts `json:"server"`
	PeerAuth bool                `json:"peer_auth"`
	// NeedsTLS says no other computer can pair with this server
	// (devices.ServerFacts.PairingNeedsTLS): the page offers no code.
	NeedsTLS bool        `json:"needs_tls,omitempty"`
	Pending  []deviceRow `json:"pending"`
	Paired   []deviceRow `json:"paired"`
	Ended    []deviceRow `json:"ended"`
	// Code is a code just minted, for a browser that posted the form without
	// the page's script; nil otherwise. It is never in a URL.
	Code *devices.Code `json:"code,omitempty"`
	// CodeMinutes is how long a code lasts.
	CodeMinutes int `json:"-"`
	// UnixCommand and WindowsCommand pair another computer, with a
	// placeholder where the code goes (or the code itself, with Code set).
	UnixCommand    string `json:"-"`
	WindowsCommand string `json:"-"`
	// The passkey approval's preconditions (peer_auth): a browser, passkeys
	// configured, one enrolled, and this page on the relying party's origin.
	Browser    bool   `json:"-"`
	PasskeysOn bool   `json:"-"`
	HasPasskey bool   `json:"-"`
	OnOrigin   bool   `json:"-"`
	Origin     string `json:"-"`
}

// devicesCodePlaceholder stands in for a code in the commands the page shows
// before one is minted.
const devicesCodePlaceholder = "XXXX-XXXX-XXXX-XXXX"

// devicesFor assembles the page for this request.
func (s *Service) devicesFor(r *http.Request, code *devices.Code) (devicesData, error) {
	ctx := r.Context()
	list, err := s.cfg.Devices.List(ctx)
	if err != nil {
		return devicesData{}, err
	}
	now := time.Now()
	d := devicesData{Server: s.cfg.Devices.Server(), PeerAuth: s.cfg.Devices.PeerAuth(), Code: code,
		CodeMinutes: int(store.PairingCodeTTL / time.Minute), Browser: s.classify(r) == credCookie}
	d.NeedsTLS = d.Server.PairingNeedsTLS()
	for _, dev := range list {
		row := deviceRow{Device: dev, Now: dev.StatusAt(now)}
		switch row.Now {
		case store.DevicePending:
			d.Pending = append(d.Pending, row)
		case store.DeviceActive:
			d.Paired = append(d.Paired, row)
		default:
			d.Ended = append(d.Ended, row)
		}
	}
	shown := devicesCodePlaceholder
	if code != nil {
		shown = code.Code
	}
	d.UnixCommand, d.WindowsCommand = devices.ClientCommands(d.Server, shown)
	if a := s.cfg.Devices.Approvals(); a != nil {
		d.PasskeysOn = a.PasskeysOn()
		creds, err := store.ApproverCredentials(ctx, s.writeDB(), store.ApproverPasskey)
		if err != nil {
			return devicesData{}, err
		}
		d.HasPasskey = len(creds) > 0
		if origins := a.Origins(); len(origins) > 0 {
			d.Origin = origins[0]
			d.OnOrigin = slices.Contains(origins, requestOrigin(r))
		}
	}
	return d, nil
}

// devicesPage is GET /console/devices.
func (s *Service) devicesPage(w http.ResponseWriter, r *http.Request) {
	if s.devicesOff(w, r) {
		return
	}
	d, err := s.devicesFor(r, nil)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, "devices", pageData{Title: "Devices", Active: "devices", Data: d})
}

// devicesCode is POST /console/devices/code: a fresh pairing code, shown
// once, in this answer: JSON for the page's script and the CLI, the page
// itself for a browser without the script. A server no other computer can
// pair with (an address of plain http to another host) mints none: 409
// (devices.ErrPairingNeedsTLS).
func (s *Service) devicesCode(w http.ResponseWriter, r *http.Request) {
	if s.devicesOff(w, r) {
		return
	}
	if s.cfg.Devices.Server().PairingNeedsTLS() {
		s.formError(w, r, http.StatusConflict, devices.ErrPairingNeedsTLS.Error())
		return
	}
	code, err := s.cfg.Devices.MintCode(r.Context(), store.DeviceByConsole)
	if errors.Is(err, store.ErrTooManyPairingCodes) {
		s.formError(w, r, http.StatusTooManyRequests, "too many pairing codes are waiting to be used: use one, or wait ten minutes")
		return
	}
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	w.Header().Set("Cache-Control", "no-store")
	if wantsJSON(r) {
		unix, windows := devices.ClientCommands(s.cfg.Devices.Server(), code.Code)
		writeJSON(w, http.StatusOK, map[string]any{"code": code.Code, "expires_at": code.ExpiresAt.UTC().Format(time.RFC3339),
			"approval": code.Approval, "unix": unix, "windows": windows})
		return
	}
	d, err := s.devicesFor(r, &code)
	if err != nil {
		s.serverError(w, r, err)
		return
	}
	s.render(w, r, "devices", pageData{Title: "Devices", Active: "devices", Data: d})
}

// devicesRevoke is POST /console/devices/{id}/revoke: an active device is
// revoked, a pending pairing denied.
func (s *Service) devicesRevoke(w http.ResponseWriter, r *http.Request) {
	if s.devicesOff(w, r) {
		return
	}
	d, err := s.cfg.Devices.Revoke(r.Context(), r.PathValue("id"), store.DeviceByConsole)
	switch {
	case errors.Is(err, store.ErrDeviceNotFound):
		s.notFound(w, r, "no such device")
		return
	case errors.Is(err, store.ErrDeviceEnded):
		s.formError(w, r, http.StatusConflict, "that device is already revoked")
		return
	case err != nil:
		s.serverError(w, r, err)
		return
	}
	if wantsJSON(r) {
		writeJSON(w, http.StatusOK, deviceRow{Device: d, Now: d.Status})
		return
	}
	notice := "Revoked " + d.Name + ": it is refused from its next request."
	if d.Status == store.DeviceDenied {
		notice = "Denied the pairing of " + d.Name + "."
	}
	http.Redirect(w, r, "/console/devices?notice="+url.QueryEscape(notice), http.StatusSeeOther)
}

// devicesApproval reads what both approval routes share, and writes the
// answer when the request cannot go on.
func (s *Service) devicesApproval(w http.ResponseWriter, r *http.Request) (string, bool) {
	if s.devicesOff(w, r) {
		return "", false
	}
	if s.cfg.Devices.Approvals() == nil {
		writeJSON(w, http.StatusConflict, map[string]string{"error": "pairings on this server need no passkey: the one-time code is the approval"})
		return "", false
	}
	if s.classify(r) != credCookie {
		writeJSON(w, http.StatusForbidden, map[string]string{"error": "approving a device needs a passkey: open this page in the browser you sign in to the console with"})
		return "", false
	}
	return r.PathValue("id"), true
}

// devicesApproveBegin is POST /console/devices/{id}/approve/begin.
func (s *Service) devicesApproveBegin(w http.ResponseWriter, r *http.Request) {
	id, ok := s.devicesApproval(w, r)
	if !ok {
		return
	}
	begun, err := s.cfg.Devices.BeginApproval(r.Context(), id)
	if err != nil {
		s.deviceApprovalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, begun)
}

// devicesApproveFinish is POST /console/devices/{id}/approve/finish.
func (s *Service) devicesApproveFinish(w http.ResponseWriter, r *http.Request) {
	id, ok := s.devicesApproval(w, r)
	if !ok {
		return
	}
	d, err := s.cfg.Devices.FinishApproval(r.Context(), id, r.PostForm.Get("ticket"), []byte(r.PostForm.Get("response")))
	if err != nil {
		s.deviceApprovalError(w, r, err)
		return
	}
	writeJSON(w, http.StatusOK, deviceRow{Device: d, Now: d.Status})
}

// deviceApprovalError answers a refused approval in the owner's words.
func (s *Service) deviceApprovalError(w http.ResponseWriter, r *http.Request, err error) {
	msg, status := "", http.StatusConflict
	switch {
	case errors.Is(err, store.ErrDeviceNotFound):
		msg, status = "no such device", http.StatusNotFound
	case errors.Is(err, approval.ErrDeviceNotWaiting):
		msg = "that device is not waiting for approval: it was approved, denied or revoked, or its ten minutes ran out"
	case errors.Is(err, approval.ErrNoPasskey):
		msg = "no passkey is enrolled: add one on the Approvers page first"
	case errors.Is(err, approval.ErrSignInTicket):
		msg = "this approval expired or was already used; start again"
	case errors.Is(err, approval.ErrPasskeysOff):
		msg = "passkeys are not configured on this daemon"
	case errors.Is(err, passkey.ErrUnknownCredential):
		msg = "that passkey is not enrolled here"
	case errors.Is(err, approval.ErrRestricted):
		msg, status = "a restricted account approves nothing", http.StatusForbidden
	}
	if msg != "" {
		writeJSON(w, status, map[string]string{"error": msg})
		return
	}
	s.approverError(w, r, err)
}
