package main

import (
	"context"
	"errors"
	"io"
	"log/slog"
	"net/http"
	"net/url"
	"os"
	"runtime"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/console"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/update"
)

// Owner decision R-D4 (2026-11-15), over the daemon's real route table and
// socket judgment: turning update checks or automatic updates off is the
// owner's own decision, refused to a process an agent started (and to a
// connection whose chain the kernel did not give), as every ownerHands route
// is. Turning them on stays open to that agent. The owner's own terminal on
// the socket and the TCP key do both. A reset is the owner's only where it
// would turn checks off. The console wires a real update.Checker over the
// daemon's database, so what the switches store is what the checker reads.

// hdNoFetch is the checker's release fetcher: nothing here asks GitHub, so a
// fetch is a failure of the test.
type hdNoFetch struct{ t *testing.T }

func (f hdNoFetch) Fetch(context.Context, string) (update.Page, error) {
	f.t.Error("the update checker fetched the release list")
	return update.Page{}, errors.New("hdNoFetch: no release list in this test")
}

// hdUpdatesDaemon is the console matrix's daemon with an update check running
// over its database: this test binary's build (a build from source unless the
// test stamped it a release), no file/env update block, its scheduled ticks
// held so that no check ever runs.
func hdUpdatesDaemon(t *testing.T) (*cmDaemon, *update.Checker) {
	t.Helper()
	var chk *update.Checker
	d := cmNewDaemon(t, access.Account{UID: cmUIDOwner, Name: cmOwnerName, Home: "/Users/" + cmOwnerName}, nil,
		func(c *console.Config) {
			db := c.WriteDB
			ticks := make(chan time.Time)
			chk = update.New(update.Deps{
				DataDir: t.TempDir(), Version: version, Distribution: distribution, GOOS: runtime.GOOS,
				Install:  update.Install{Kind: update.KindSource, Hint: update.Hint(update.KindSource)},
				Instance: "01HDUPDATESOWNERHANDS00000", PID: os.Getpid(),
				Override: func(ctx context.Context) (config.UpdateOverride, error) {
					o, _, err := store.UpdateOverride(ctx, db)
					return o, err
				},
				Fetcher: hdNoFetch{t},
				Ticks:   func() (<-chan time.Time, func()) { return ticks, func() {} },
				Log:     slog.New(slog.NewTextHandler(io.Discard, nil)),
			})
			c.Updates = chk
		})
	ctx, cancel := context.WithCancel(context.Background())
	chk.Start(ctx)
	t.Cleanup(func() { cancel(); chk.Wait() })
	return d, chk
}

// hdAgentHops is an agent's chain as the kernel names it: seam under claude.
func hdAgentHops() []access.ChainHop {
	return []access.ChainHop{
		{Token: access.ProcessToken{PID: 7101, Version: 7101000, Boot: cmBoot}, Name: "seam"},
		{Token: access.ProcessToken{PID: 7100, Version: 7100000, Boot: cmBoot}, Name: "claude"},
	}
}

// hdTerminalHops is the owner's own terminal: seam under a shell under the
// terminal app, no agent anywhere.
func hdTerminalHops() []access.ChainHop {
	return []access.ChainHop{
		{Token: access.ProcessToken{PID: 7201, Version: 7201000, Boot: cmBoot}, Name: "seam"},
		{Token: access.ProcessToken{PID: 7200, Version: 7200000, Boot: cmBoot}, Name: "zsh"},
		{Token: access.ProcessToken{PID: 7199, Version: 7199000, Boot: cmBoot}, Name: "ghostty"},
	}
}

// hdPost posts form to path as c and reads the answer: a refusal's 403, or the
// redirect-after-POST's flash (param "notice" or "error", and the message).
func hdPost(t *testing.T, c cmCaller, path string, form url.Values) (status int, param, msg, body string) {
	t.Helper()
	status, body, hdr := c.do(t, http.MethodPost, path, form, false)
	if status != http.StatusSeeOther {
		return status, "", "", body
	}
	u, err := url.Parse(hdr.Get("Location"))
	require.NoError(t, err)
	for _, p := range []string{"notice", "error"} {
		if m := u.Query().Get(p); m != "" {
			return status, p, m, body
		}
	}
	t.Fatalf("%s %s: a redirect with no flash: %s", c.name, path, hdr.Get("Location"))
	return
}

// hdRequireRefused checks the owner-hands refusal: 403, in the words every
// ownerHands route answers an agent's process with.
func hdRequireRefused(t *testing.T, c cmCaller, path string, form url.Values) {
	t.Helper()
	status, _, _, body := hdPost(t, c, path, form)
	require.Equal(t, http.StatusForbidden, status, "%s %s %s: %s", c.name, path, form.Encode(), body)
	require.Contains(t, body, "this is the owner's decision, and this request came from a process an agent started",
		"%s %s %s", c.name, path, form.Encode())
}

// hdRequireNotice checks a stored change: the section's notice, containing want.
func hdRequireNotice(t *testing.T, c cmCaller, path string, form url.Values, want string) {
	t.Helper()
	status, param, msg, body := hdPost(t, c, path, form)
	require.Equal(t, http.StatusSeeOther, status, "%s %s %s: %s", c.name, path, form.Encode(), body)
	require.Equal(t, "notice", param, "%s %s %s: %s", c.name, path, form.Encode(), msg)
	require.Contains(t, msg, want, "%s %s %s", c.name, path, form.Encode())
}

func TestHDUpdatesOff_IsTheOwnersDecisionOnTheSocket(t *testing.T) {
	d, chk := hdUpdatesDaemon(t)
	ctx := context.Background()
	stored := func() string {
		t.Helper()
		raw, found, err := store.GetSetting(ctx, d.db, store.SettingUpdateConfig)
		require.NoError(t, err)
		if !found {
			return ""
		}
		return strings.TrimSpace(raw)
	}
	settings := func() update.Settings { return chk.Status().Settings }
	require.False(t, chk.Status().Release, "this test binary is a build from source")
	require.False(t, settings().Check, "a source build does not check by default")

	agent := cmSocketCaller("an agent's process on the socket", d.socketHops(t, cmUIDOwner, hdAgentHops()...))
	unread := cmSocketCaller("a connection whose chain the kernel did not give", d.socketHops(t, cmUIDOwner))
	terminal := cmSocketCaller("the owner's own terminal on the socket", d.socketHops(t, cmUIDOwner, hdTerminalHops()...))
	key := d.bearer()
	check := func(v string) url.Values { return url.Values{"check": {v}} }
	auto := func(v string) url.Values { return url.Values{"auto": {v}} }
	const (
		checks = "/console/settings/updates"
		autos  = "/console/settings/updates/auto"
		reset  = "/console/settings/updates/reset"
	)

	// On is open to an agent: it turns checks on, then automatic updates.
	hdRequireNotice(t, agent, checks, check("on"), "Update checks turned on.")
	hdRequireNotice(t, agent, autos, auto("on"), "Automatic updates turned on.")
	require.True(t, settings().Check)
	require.True(t, settings().Auto)
	want := stored()
	require.JSONEq(t, `{"check":true,"auto":true}`, want)

	// Off is not, and nor is the reset, which would turn checks off on this
	// build: neither caller stores anything, and the checker keeps both on.
	for _, c := range []cmCaller{agent, unread} {
		hdRequireRefused(t, c, checks, check("off"))
		hdRequireRefused(t, c, autos, auto("off"))
		hdRequireRefused(t, c, reset, url.Values{})
		require.Equal(t, want, stored(), c.name)
		require.True(t, settings().Check, c.name)
		require.True(t, settings().Auto, c.name)
		// On is still open to both.
		hdRequireNotice(t, c, checks, check("on"), "Update checks turned on.")
	}

	// The owner's own terminal turns both off, and back on.
	hdRequireNotice(t, terminal, autos, auto("off"), "Automatic updates turned off")
	require.True(t, settings().Check)
	require.False(t, settings().Auto)
	hdRequireNotice(t, terminal, checks, check("off"), "Update checks turned off")
	require.False(t, settings().Check)
	require.JSONEq(t, `{"check":false,"auto":false}`, stored())

	// With both off, a reset turns nothing off on this build: open to an agent.
	hdRequireNotice(t, agent, reset, url.Values{}, "Update checks follow the config file again: they are off.")
	require.Empty(t, stored())

	// The TCP key does everything, the reset that turns checks off included.
	hdRequireNotice(t, key, checks, check("on"), "Update checks turned on.")
	hdRequireNotice(t, key, autos, auto("off"), "Automatic updates turned off")
	hdRequireNotice(t, key, checks, check("off"), "Update checks turned off")
	hdRequireNotice(t, key, checks, check("on"), "Update checks turned on.")
	hdRequireRefused(t, agent, reset, url.Values{})
	hdRequireNotice(t, key, reset, url.Values{}, "they are off")
	require.Empty(t, stored())
	require.False(t, settings().Check)
}

// On a release build checks are on by default, so no reset turns them off:
// it stays open to an agent, whatever the console's override said.
func TestHDUpdatesReset_OpenToAnAgentOnARelease(t *testing.T) {
	asRelease(t, "0.7.2")
	d, chk := hdUpdatesDaemon(t)
	require.True(t, chk.Status().Release)
	require.True(t, chk.Status().Settings.Check, "a release checks by default")
	agent := cmSocketCaller("an agent's process on the socket", d.socketHops(t, cmUIDOwner, hdAgentHops()...))
	terminal := cmSocketCaller("the owner's own terminal on the socket", d.socketHops(t, cmUIDOwner, hdTerminalHops()...))

	hdRequireRefused(t, agent, "/console/settings/updates", url.Values{"check": {"off"}})
	hdRequireNotice(t, terminal, "/console/settings/updates", url.Values{"check": {"off"}}, "Update checks turned off")
	hdRequireNotice(t, agent, "/console/settings/updates", url.Values{"check": {"on"}}, "Update checks turned on.")
	hdRequireNotice(t, agent, "/console/settings/updates/reset", url.Values{}, "they are on")
	require.True(t, chk.Status().Settings.Check)
	require.True(t, chk.Status().Settings.Auto)
}
