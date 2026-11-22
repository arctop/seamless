package console

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"os"
	"regexp"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/devices"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/update"
)

// Turning update checks or automatic updates off keeps an install on an old
// release, so that direction is the owner's own decision (owner decision
// R-D4, 2026-11-15): an agent's process on the peer_auth socket and a paired
// device are refused it, as on every ownerHands route, and may turn both on.
// The owner -- a terminal of their own on the socket, the bearer key, the
// browser -- does both. A reset is the owner's only where it would turn
// checks off.

// uohCaller is one kind of caller of the Updates routes.
type uohCaller struct {
	name string
	// owner says the caller makes the owner's decisions.
	owner bool
	// device is set for a paired device, which is refused in its own words.
	device *devices.Credential
	// dress makes req the caller's.
	dress func(t *testing.T, req *http.Request) *http.Request
}

// uohCallers is every kind of caller: the ones refused "off", then the owner's.
func uohCallers(t *testing.T) []uohCaller {
	t.Helper()
	sock, ok := consolePolicy(t).PeerView(501)
	require.True(t, ok)
	socket := func(fromAgent func() bool) func(*testing.T, *http.Request) *http.Request {
		return func(_ *testing.T, req *http.Request) *http.Request {
			ctx := access.WithView(req.Context(), sock)
			if fromAgent != nil {
				ctx = access.WithAgentChain(ctx, fromAgent)
			}
			return req.WithContext(ctx)
		}
	}
	creds := con2Creds(t)
	relay, key := creds[0], creds[1]
	return []uohCaller{
		{name: "an agent's process on the socket", dress: socket(func() bool { return true })},
		{name: "a socket connection whose chain the kernel did not give", dress: socket(nil)},
		{name: "a mapped relay under peer_auth", device: &relay, dress: func(t *testing.T, req *http.Request) *http.Request {
			return req.WithContext(con2RelayView(t, devices.WithCredential(req.Context(), relay), relay))
		}},
		{name: "a relay without peer_auth", device: &relay, dress: func(_ *testing.T, req *http.Request) *http.Request {
			return req.WithContext(devices.WithCredential(req.Context(), relay))
		}},
		{name: "a key device", device: &key, dress: func(_ *testing.T, req *http.Request) *http.Request {
			return req.WithContext(devices.WithCredential(req.Context(), key))
		}},
		{name: "the owner's own terminal on the socket", owner: true, dress: socket(func() bool { return false })},
		{name: "the bearer key", owner: true, dress: func(_ *testing.T, req *http.Request) *http.Request { return authedReq(req) }},
		{name: "the owner's browser", owner: true, dress: func(_ *testing.T, req *http.Request) *http.Request {
			req.AddCookie(consoleCookie())
			req.Header.Set("Sec-Fetch-Site", "same-origin")
			return req
		}},
	}
}

// uohPost posts body to path as c.
func uohPost(t *testing.T, mux *http.ServeMux, c uohCaller, path, body string) *httptest.ResponseRecorder {
	t.Helper()
	req := httptest.NewRequest(http.MethodPost, "http://localhost:8081"+path, strings.NewReader(body))
	req.Header.Set("Content-Type", "application/x-www-form-urlencoded")
	return do(mux, c.dress(t, req))
}

// uohRequireRefused checks the owner-hands refusal c gets: 403 and the one
// JSON shape every ownerHands route answers with, in a device's words or an
// agent's.
func uohRequireRefused(t *testing.T, rr *httptest.ResponseRecorder, c uohCaller, what string) {
	t.Helper()
	if c.device != nil {
		con2RequireDeviceRefused(t, rr, *c.device, what)
		return
	}
	require.Equal(t, http.StatusForbidden, rr.Code, "%s: %s", what, rr.Body.String())
	var answer map[string]string
	require.NoError(t, json.Unmarshal(rr.Body.Bytes(), &answer), "%s: %s", what, rr.Body.String())
	require.Len(t, answer, 1, what)
	require.Equal(t, "this is the owner's decision, and this request came from a process an agent started: "+
		"run it in a terminal of your own "+access.NotBang+", or on the console", answer["error"], what)
}

// uohSwitches are the two switches: the route, its form field, and how to
// read the field in a stored override.
var uohSwitches = []struct {
	path, field string
	stored      func(config.UpdateOverride) *bool
}{
	{"/console/settings/updates", "check", func(o config.UpdateOverride) *bool { return o.Check }},
	{"/console/settings/updates/auto", "auto", func(o config.UpdateOverride) *bool { return o.Auto }},
}

// Off is refused from an agent's process and from a paired device, with
// nothing stored and the checker untouched; on is stored for every caller;
// the owner's callers do both.
func TestUpdatesOff_IsTheOwnersDecision(t *testing.T) {
	now := time.Now()
	for _, sw := range uohSwitches {
		for _, c := range uohCallers(t) {
			t.Run(sw.field+"/"+c.name, func(t *testing.T) {
				st := autoStatus(now)
				db, mux, fake := newUpdatesConsole(t, autoBase, &st)

				rr := uohPost(t, mux, c, sw.path, sw.field+"=off")
				if c.owner {
					param, msg := locationOf(t, rr)
					require.Equal(t, "notice", param, msg)
					require.Contains(t, msg, "turned off")
					o, found := storedOverride(t, db)
					require.True(t, found)
					require.Equal(t, boolPtr(false), sw.stored(o), "the owner turned it off")
				} else {
					uohRequireRefused(t, rr, c, "off")
					_, found := storedOverride(t, db)
					require.False(t, found, "nothing is stored")
					_, refreshes := fake.counts()
					require.Zero(t, refreshes, "the checker was not asked")
					require.True(t, fake.Status().Settings.Check)
					require.True(t, fake.Status().Settings.Auto)
				}

				rr = uohPost(t, mux, c, sw.path, sw.field+"=on")
				param, msg := locationOf(t, rr)
				require.Equal(t, "notice", param, msg)
				require.Contains(t, msg, "turned on")
				o, found := storedOverride(t, db)
				require.True(t, found)
				require.Equal(t, boolPtr(true), sw.stored(o), "on is open to every caller")
				require.True(t, fake.Status().Settings.Check)
				require.True(t, fake.Status().Settings.Auto)
			})
		}
	}
}

// Off is the owner's whatever the switch reads now: a stored off outlasts the
// state it changes nothing in today. With checks off in this console, an
// agent's auto=off would keep automatic updates off once the owner turns
// checks back on.
func TestUpdatesOff_RefusedEvenWhereItChangesNothingToday(t *testing.T) {
	st := autoStatus(time.Now())
	db, mux, fake := newUpdatesConsole(t, autoBase, &st)
	require.NoError(t, store.SetUpdateOverride(context.Background(), db, config.UpdateOverride{Check: boolPtr(false)}))
	_, err := fake.Refresh(context.Background())
	require.NoError(t, err)
	require.False(t, fake.Status().Settings.Auto, "automatic updates are off with the checks")

	agent := uohCallers(t)[0]
	for _, sw := range uohSwitches {
		uohRequireRefused(t, uohPost(t, mux, agent, sw.path, sw.field+"=off"), agent, sw.field)
	}
	o, _ := storedOverride(t, db)
	require.Equal(t, config.UpdateOverride{Check: boolPtr(false)}, o, "the override is the owner's, unchanged")
}

// The route judges the form exactly as the handler reads it: a form the
// handler refuses is answered with the handler's own flash for an agent too,
// and stores nothing either way.
func TestUpdatesOff_OnlyAValidOffIsJudged(t *testing.T) {
	agent := uohCallers(t)[0]
	for _, sw := range uohSwitches {
		for body, want := range map[string]string{
			"":                                     sw.field + " is required: valid values are on, off",
			sw.field + "=OFF":                      sw.field + ` invalid "OFF": valid values are on, off`,
			sw.field + "=":                         sw.field + ` invalid "": valid values are on, off`,
			sw.field + "=off&" + sw.field + "=on":  sw.field + " must be given exactly once: valid values are on, off",
			sw.field + "=off&" + sw.field + "=off": sw.field + " must be given exactly once: valid values are on, off",
		} {
			st := autoStatus(time.Now())
			db, mux, _ := newUpdatesConsole(t, autoBase, &st)
			param, msg := locationOf(t, uohPost(t, mux, agent, sw.path, body))
			require.Equal(t, "error", param, body)
			require.Equal(t, want, msg, body)
			_, found := storedOverride(t, db)
			require.False(t, found, body)
		}
		// The switch is read from the body alone, as the handler reads it: an
		// off in the query string is no off.
		st := autoStatus(time.Now())
		db, mux, _ := newUpdatesConsole(t, autoBase, &st)
		param, _ := locationOf(t, uohPost(t, mux, agent, sw.path+"?"+sw.field+"=off", sw.field+"=on"))
		require.Equal(t, "notice", param)
		o, _ := storedOverride(t, db)
		require.Equal(t, boolPtr(true), sw.stored(o))
	}
}

// In a process that runs no update check the refusal still comes first: off is
// the owner's whatever this process runs. Everything else keeps its flash.
func TestUpdatesOff_RefusedWhereNoUpdateCheckRuns(t *testing.T) {
	db, mux, _ := newUpdatesConsole(t, config.Update{}, nil)
	callers := uohCallers(t)
	for _, sw := range uohSwitches {
		for _, c := range callers {
			rr := uohPost(t, mux, c, sw.path, sw.field+"=off")
			if c.owner {
				param, msg := locationOf(t, rr)
				require.Equal(t, "error", param, c.name)
				require.Equal(t, updatesNotRunning, msg, c.name)
			} else {
				uohRequireRefused(t, rr, c, c.name)
			}
			param, msg := locationOf(t, uohPost(t, mux, c, sw.path, sw.field+"=on"))
			require.Equal(t, "error", param, c.name)
			require.Equal(t, updatesNotRunning, msg, c.name)
		}
	}
	for _, c := range callers {
		param, msg := locationOf(t, uohPost(t, mux, c, "/console/settings/updates/reset", ""))
		require.Equal(t, "error", param, c.name)
		require.Equal(t, updatesNotRunning, msg, c.name)
	}
	_, found := storedOverride(t, db)
	require.False(t, found)
}

// uohSetOverride stores raw as the console's update override ("" clears it)
// and has the fake checker re-read it over base on a release or source build.
func uohSetOverride(t *testing.T, db *sql.DB, fake *fakeUpdates, base config.Update, release bool, raw string) {
	t.Helper()
	ctx := context.Background()
	if raw == "" {
		require.NoError(t, store.ClearUpdateOverride(ctx, db))
	} else {
		require.NoError(t, store.SetSetting(ctx, db, store.SettingUpdateConfig, raw))
	}
	fake.mu.Lock()
	fake.base = base
	fake.status.Release = release
	if !release {
		fake.status.Distribution = ""
	}
	o, _, err := store.UpdateOverride(ctx, db)
	if err != nil {
		o = config.UpdateOverride{} // the checker ignores a row that does not decode
	}
	fake.status.Settings = update.Effective(base, o, release)
	fake.mu.Unlock()
}

// uohRawOverride is the stored override row as text, "" when there is none.
func uohRawOverride(t *testing.T, db *sql.DB) string {
	t.Helper()
	raw, found, err := store.GetSetting(context.Background(), db, store.SettingUpdateConfig)
	require.NoError(t, err)
	if !found {
		return ""
	}
	return raw
}

// A reset is the owner's decision only where it would turn update checks off:
// the console's override is what keeps them on, and without it the build's
// default is off. On a release build no reset turns anything off, so every
// caller may reset.
func TestUpdatesReset_OwnersOnlyWhereItTurnsChecksOff(t *testing.T) {
	yes, no := boolPtr(true), boolPtr(false)
	for _, tc := range []struct {
		name    string
		release bool
		base    config.Update
		raw     string
		// owners: the reset is refused from an agent and a device.
		owners bool
	}{
		{"release build, both switched on here", true, config.Update{}, `{"check":true,"auto":true}`, false},
		{"release build, both switched off here: a reset turns them on", true, config.Update{}, `{"check":false,"auto":false}`, false},
		{"release build, the file keeps automatic updates off", true, config.Update{Auto: no}, `{"check":true,"auto":true}`, false},
		{"release build, the file says check: true", true, config.Update{Check: yes}, `{"check":true}`, false},
		{"source build, checks switched on here", false, config.Update{}, `{"check":true}`, true},
		{"source build, checks on and automatic updates off here", false, config.Update{}, `{"check":true,"auto":false}`, true},
		{"source build, checks switched off here", false, config.Update{}, `{"check":false}`, false},
		{"source build, only automatic updates switched here", false, config.Update{}, `{"auto":true}`, false},
		{"source build, the file locks checks off", false, config.Update{Check: no}, `{"check":true}`, false},
		{"source build, a stored row that does not decode", false, config.Update{}, `{"check":"yes"}`, false},
		{"source build, a truncated stored row", false, config.Update{}, `{"check":tr`, false},
		// The console holds no file/env update block, so it cannot tell this
		// from an unset update.check: it counts the reset as the owner's.
		{"source build, the file says check: true too", false, config.Update{Check: yes}, `{"check":true}`, true},
		{"source build, the file says check: true, switched off here", false, config.Update{Check: yes}, `{"check":false}`, false},
	} {
		for _, c := range uohCallers(t) {
			t.Run(tc.name+"/"+c.name, func(t *testing.T) {
				st := autoStatus(time.Now())
				db, mux, fake := newUpdatesConsole(t, tc.base, &st)
				uohSetOverride(t, db, fake, tc.base, tc.release, tc.raw)
				before := fake.Status().Settings

				rr := uohPost(t, mux, c, "/console/settings/updates/reset", "")
				if tc.owners && !c.owner {
					uohRequireRefused(t, rr, c, "reset")
					require.Equal(t, tc.raw, uohRawOverride(t, db), "the override stays")
					_, refreshes := fake.counts()
					require.Zero(t, refreshes)
					require.Equal(t, before, fake.Status().Settings)
					return
				}
				param, msg := locationOf(t, rr)
				require.Equal(t, "notice", param, msg)
				require.Empty(t, uohRawOverride(t, db), "the override is cleared")
				after := fake.Status().Settings
				if !c.owner {
					require.False(t, before.Check && !after.Check, "an open reset turned update checks off")
					require.False(t, before.Auto && !after.Auto, "an open reset turned automatic updates off")
				}
			})
		}
	}
}

// The rule against update.Effective itself, over every file/env block, every
// stored override and both kinds of build: a reset that turns update checks
// or automatic updates off is always the owner's, a reset on a release build
// or under a locked check never is, and the only reset counted as the owner's
// that turns nothing off is the one the console cannot tell apart (a source
// build whose file says check: true beside an override that says it too).
func TestUpdatesReset_NeverOpenWhereItTurnsOneOff(t *testing.T) {
	st := autoStatus(time.Now())
	db, mux, fake := newUpdatesConsole(t, config.Update{}, &st)
	agent := uohCallers(t)[0]
	tri := []*bool{nil, boolPtr(true), boolPtr(false)}
	word := func(b *bool) string {
		if b == nil {
			return "unset"
		}
		return fmt.Sprint(*b)
	}
	combos, owners, cautious := 0, 0, 0
	for _, release := range []bool{true, false} {
		for _, baseCheck := range tri {
			for _, baseAuto := range tri {
				for _, oCheck := range tri {
					for _, oAuto := range tri {
						base := config.Update{Check: baseCheck, Auto: baseAuto}
						o := config.UpdateOverride{Check: oCheck, Auto: oAuto}
						what := fmt.Sprintf("release=%v file check=%s auto=%s, console check=%s auto=%s",
							release, word(baseCheck), word(baseAuto), word(oCheck), word(oAuto))
						raw, err := json.Marshal(o)
						require.NoError(t, err)
						if oCheck == nil && oAuto == nil {
							raw = nil
						}
						uohSetOverride(t, db, fake, base, release, string(raw))
						now := update.Effective(base, o, release)
						then := update.Effective(base, config.UpdateOverride{}, release)
						turnsOff := (now.Check && !then.Check) || (now.Auto && !then.Auto)

						rr := uohPost(t, mux, agent, "/console/settings/updates/reset", "")
						refused := rr.Code == http.StatusForbidden
						if !refused {
							require.Equal(t, http.StatusSeeOther, rr.Code, what)
						}
						combos++
						switch {
						case turnsOff:
							require.True(t, refused, "%s: this reset turns one off and was open to an agent", what)
							owners++
						case refused:
							require.True(t, !release && baseCheck != nil && *baseCheck && oCheck != nil && *oCheck,
								"%s: refused though it turns nothing off", what)
							cautious++
						}
						if release || (baseCheck != nil && !*baseCheck) {
							require.False(t, refused, what)
						}
					}
				}
			}
		}
	}
	require.Equal(t, 162, combos)
	require.Equal(t, 9, owners, "a source build, update.check unset, checks switched on here: 3 file autos x 3 console autos")
	require.Equal(t, 9, cautious, "the same with update.check: true in the file")
}

// A reset whose override the console cannot read is the owner's: it cannot
// tell what the reset would drop. The owner's reset is not judged at all.
func TestUpdatesReset_AnUnreadableStoreIsTheOwners(t *testing.T) {
	st := autoStatus(time.Now())
	db, mux, fake := newUpdatesConsole(t, config.Update{}, &st)
	uohSetOverride(t, db, fake, config.Update{}, false, `{"check":true}`)
	require.NoError(t, db.Close())
	callers := uohCallers(t)
	agent := callers[0]
	uohRequireRefused(t, uohPost(t, mux, agent, "/console/settings/updates/reset", ""), agent, "reset")
}

// The routes judged conditionally, read from the source: the Gardener's
// decisions while agents may not apply (settings_gardener_test.go), and the
// Updates routes in one direction only. A route added through ownerHandsIf
// later has to be named here, and tested.
func TestUpdates_TheConditionalOwnerHandsRoutes(t *testing.T) {
	src, err := os.ReadFile("console.go")
	require.NoError(t, err)
	re := regexp.MustCompile(`(?m)^\s*post\("POST (/console/[^"]+)",.*\bownerHandsIf\(`)
	var got []string
	for _, m := range re.FindAllStringSubmatch(string(src), -1) {
		got = append(got, m[1])
	}
	require.Equal(t, []string{
		"/console/gardener/group/dismiss",
		"/console/gardener/plan/{slug}/apply",
		"/console/gardener/{id}/apply",
		"/console/gardener/{id}/dismiss",
		"/console/gardener/{id}/hide",
		"/console/gardener/{id}/unhide",
		"/console/gardener/{id}/retarget",
		"/console/gardener/{id}/undo",
		"/console/settings/updates",
		"/console/settings/updates/reset",
		"/console/settings/updates/auto",
	}, got)
	require.Equal(t, len(got), strings.Count(string(src), "ownerHandsIf("), "every use is a route registration this reads")
	// They are no ownerHands route: an empty form there is the handler's to
	// answer, so the device test's enumeration must not pick them up.
	for _, rt := range con2OwnerHandsRoutes(t) {
		require.NotContains(t, got, rt.path)
	}
}
