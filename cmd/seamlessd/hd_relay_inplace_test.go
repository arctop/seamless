//go:build darwin || linux

package main

// The relay matrix's approval-in-place rows (owner decision R-D6): serve's
// TCP stack under peer_auth, a relay paired with its one-time code alone
// while peer_auth was off, now mapped in peer_auth.relays. Until the owner's
// passkey approves it in place it is refused exactly as before; no agent
// caller can approve it; the owner's browser approves it on the Devices page,
// keeping its device id and certificate; from its next request it is judged
// as a passkey-approved pairing. A revoked relay is not offered: it pairs
// again.

import (
	"context"
	"encoding/json"
	"net/http"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/devices"
	"github.com/arctop/seamless/internal/passkey/passkeytest"
	"github.com/arctop/seamless/internal/store"
)

// hdrOrigin is the rig's passkey origin (config.RelyingParty's default).
const hdrOrigin = "http://localhost:8081"

// hdrBrowser signs a browser in to the rig's console with the key, as the
// login form does, and returns the headers its same-origin requests carry.
func (d *relayvDaemon) hdrBrowser(t *testing.T) map[string]string {
	t.Helper()
	client := d.httpClient(nil)
	req, err := http.NewRequest(http.MethodPost, d.tcp.URL+"/console/login", strings.NewReader(url.Values{"key": {relayvKey}}.Encode()))
	require.NoError(t, err)
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	resp, err := client.Do(req)
	require.NoError(t, err)
	require.NoError(t, resp.Body.Close())
	var cookies []string
	for _, c := range resp.Cookies() {
		cookies = append(cookies, c.Name+"="+c.Value)
	}
	require.NotEmpty(t, cookies, "the login sets the console's cookie")
	return map[string]string{"Cookie": strings.Join(cookies, "; "), "Sec-Fetch-Site": "same-origin"}
}

// hdrEnrollPasskey enrolls a virtual passkey with the daemon's approval
// service, a code minted for its key as `seamlessd approvers enroll-code`
// mints one.
func (d *relayvDaemon) hdrEnrollPasskey(t *testing.T) *passkeytest.Authenticator {
	t.Helper()
	ctx := context.Background()
	key, err := passkeytest.New(true)
	require.NoError(t, err)
	begun, err := d.svc.BeginPasskey(ctx, "security key")
	require.NoError(t, err)
	created, err := key.Create(begun.Options, hdrOrigin)
	require.NoError(t, err)
	fin, err := d.svc.FinishPasskey(ctx, begun.ID, created)
	require.NoError(t, err)
	code, _, err := store.MintEnrollCode(ctx, d.db, store.ApproverPasskey, fin.Fingerprint, time.Now())
	require.NoError(t, err)
	_, err = d.svc.ConfirmPasskeyCode(ctx, begun.ID, code)
	require.NoError(t, err)
	return key
}

// hdrDevicesPage reads the Devices page's JSON with the key.
func (d *relayvDaemon) hdrDevicesPage(t *testing.T) (inPlace, paired, ended []string) {
	t.Helper()
	status, body := d.do(t, d.httpClient(nil), http.MethodGet, "/console/devices?format=json", nil,
		map[string]string{"Authorization": "Bearer " + relayvKey})
	require.Equal(t, http.StatusOK, status, body)
	var page struct {
		InPlace []store.Device `json:"in_place"`
		Paired  []store.Device `json:"paired"`
		Ended   []store.Device `json:"ended"`
	}
	relayvJSON(t, body, &page)
	ids := func(ds []store.Device) []string {
		out := []string{}
		for _, x := range ds {
			out = append(out, x.ID)
		}
		return out
	}
	return ids(page.InPlace), ids(page.Paired), ids(page.Ended)
}

func TestHDRelayInPlace_Matrix(t *testing.T) {
	d := newRelayvDaemon(t)
	ctx := context.Background()
	laptop, laptopID := d.pair(t, relayvLaptop)
	old, oldID := d.pairCodeOnly(t, relayvOld)
	// A second code-only relay, mapped, that the owner revokes: it pairs again.
	gone, _ := d.pairCodeOnly(t, relayvDesk)
	_, err := d.pairing.Revoke(ctx, gone.ID, store.DeviceByConsole)
	require.NoError(t, err)
	key := d.hdrEnrollPasskey(t)
	agentB := relayvCallerB.relay2Headers(relay2Named)

	t.Run("before its approval the relay is refused, as before", func(t *testing.T) {
		status, body := d.do(t, d.httpClient(oldID), http.MethodGet, "/console/?format=json", nil, agentB)
		require.Equal(t, http.StatusForbidden, status, body)
		require.Contains(t, body, "approve it with a passkey on the server's Devices page, where it waits for you")
		require.Equal(t, http.StatusForbidden, d.initStatus(t, d.httpClient(oldID), agentB))
	})
	t.Run("the Devices page offers it, and only it", func(t *testing.T) {
		inPlace, paired, ended := d.hdrDevicesPage(t)
		require.Equal(t, []string{old.ID}, inPlace)
		require.Contains(t, paired, laptop.ID)
		require.NotContains(t, paired, old.ID)
		require.Contains(t, ended, gone.ID, "a revoked relay is not offered")
	})
	t.Run("no agent caller approves it", func(t *testing.T) {
		begin := "/console/devices/" + old.ID + "/approve/begin"
		for name, call := range map[string]func() (int, string){
			"the bearer key": func() (int, string) {
				return d.do(t, d.httpClient(nil), http.MethodPost, begin, url.Values{}, map[string]string{"Authorization": "Bearer " + relayvKey})
			},
			"an agent through another relay": func() (int, string) {
				return d.do(t, d.httpClient(laptopID), http.MethodPost, begin, url.Values{}, agentB)
			},
			"the owner's own hands through another relay": func() (int, string) {
				return d.do(t, d.httpClient(laptopID), http.MethodPost, begin, url.Values{}, hdrTerminal.relay2Headers(hdrNames))
			},
			"the relay itself": func() (int, string) {
				return d.do(t, d.httpClient(oldID), http.MethodPost, begin, url.Values{}, hdrTerminal.relay2Headers(hdrNames))
			},
		} {
			status, body := call()
			require.Equal(t, http.StatusForbidden, status, "%s: %s", name, body)
		}
		local := d.socket(t, relayvOwnerUID, relayvLocalTok(5210), relayvLocalTok(5200))
		req, err := http.NewRequest(http.MethodPost, local.URL+begin, strings.NewReader(""))
		require.NoError(t, err)
		req.Header.Set("Accept", "application/json")
		req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
		resp, err := http.DefaultClient.Do(req)
		require.NoError(t, err)
		require.NoError(t, resp.Body.Close())
		require.Equal(t, http.StatusForbidden, resp.StatusCode, "an agent on this machine's socket")
		still, err := store.DeviceByID(ctx, d.db, old.ID)
		require.NoError(t, err)
		require.Empty(t, still.ApprovedBy)
	})
	t.Run("the owner's browser approves it in place with a passkey", func(t *testing.T) {
		browser := d.hdrBrowser(t)
		base := "/console/devices/" + old.ID + "/approve"
		status, body := d.do(t, d.httpClient(nil), http.MethodPost, base+"/begin", url.Values{}, browser)
		require.Equal(t, http.StatusOK, status, body)
		var begun devices.Ceremony
		require.NoError(t, json.Unmarshal([]byte(body), &begun))
		resp, err := key.Get(begun.Options, hdrOrigin)
		require.NoError(t, err)
		status, body = d.do(t, d.httpClient(nil), http.MethodPost, base+"/finish",
			url.Values{"ticket": {begun.Ticket}, "response": {string(resp)}}, browser)
		require.Equal(t, http.StatusOK, status, body)
		approved, err := store.DeviceByID(ctx, d.db, old.ID)
		require.NoError(t, err)
		require.NotEmpty(t, approved.ApprovedBy)
		require.Equal(t, old.ID, approved.ID)
		require.Equal(t, old.Fingerprint, approved.Fingerprint, "the same certificate")
		inPlace, paired, _ := d.hdrDevicesPage(t)
		require.Empty(t, inPlace)
		require.Contains(t, paired, old.ID)
	})
	t.Run("from its next request it is judged as a passkey-approved pairing", func(t *testing.T) {
		status, body := d.do(t, d.httpClient(oldID), http.MethodGet, "/console/?format=json", nil, agentB)
		require.Equal(t, http.StatusOK, status, body)
		b := d.mcpVia(t, oldID, "", agentB)
		text, refused := relayvRead(t, b, relayvFin, d.finMem)
		require.True(t, refused, "the lock still holds")
		require.NotContains(t, text, relayvSecret)
		short := lmShortRe.FindString(text)
		require.NotEmpty(t, short, "its agent files its own request, as through any approved relay: %s", text)
		a, err := d.svc.Resolve(ctx, short)
		require.NoError(t, err)
		require.True(t, strings.HasPrefix(a.ProcessKey, access.RelayKeyPrefix(old.ID)), "keyed on this relay, its id kept")
		text, refused = relayvRead(t, b, relayvOpen, d.openMem)
		require.False(t, refused, text)
		require.Contains(t, text, relayvOpenToken)
	})
	t.Run("a revoked relay is not approved, and pairs again", func(t *testing.T) {
		browser := d.hdrBrowser(t)
		status, body := d.do(t, d.httpClient(nil), http.MethodPost, "/console/devices/"+gone.ID+"/approve/begin", url.Values{}, browser)
		require.Equal(t, http.StatusConflict, status, body)
		require.Contains(t, body, "not waiting for approval")
	})
}
