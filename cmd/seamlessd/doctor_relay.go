package main

// doctor's relay report (plan networked-relay; internal/relay): on a computer
// with a relay -- its data directory exists -- the account it runs as, the
// layout its preflight holds it to, its pairing (which server, which pinned
// certificate), its socket, and whether the server answers it. Anywhere else
// there is no row at all.
//
// Run doctor as the relay's account (sudo -u) to see everything: its data
// directory is 0700, so another account sees the account, the layout, the
// socket and the path through it, but not the pairing.

import (
	"context"
	"encoding/json"
	"errors"
	"fmt"
	"io"
	"io/fs"
	"net"
	"net/http"
	"os/user"
	"strconv"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/access"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/devices"
	"github.com/arctop/seamless/internal/relay"
)

// relayDoctorEnv is where doctor looks for a relay; tests point it at a temp
// layout and play its account.
type relayDoctorEnv struct {
	layout relay.Layout
	env    relay.Env
	// account names a uid for the report.
	account func(uid uint32) string
	// timeout bounds each probe.
	timeout time.Duration
}

// defaultRelayDoctorEnv is this machine's: the root-owned layout.
func defaultRelayDoctorEnv() relayDoctorEnv {
	return relayDoctorEnv{layout: relay.DefaultLayout(), env: relay.HostEnv(), account: relayAccountName, timeout: 5 * time.Second}
}

// relayAccountName is uid's account name, or "uid N" when it has none.
func relayAccountName(uid uint32) string {
	if u, err := user.LookupId(strconv.FormatUint(uint64(uid), 10)); err == nil && u.Username != "" {
		return u.Username
	}
	return "uid " + strconv.FormatUint(uint64(uid), 10)
}

// relayDoctorChecks is the relay report: no rows unless a relay's data
// directory is there.
func relayDoctorChecks(ctx context.Context, e relayDoctorEnv) []check {
	const name = "relay"
	if !relay.Supported {
		return nil
	}
	dir := e.layout.DataDir
	facts, err := e.env.Inspect(dir)
	if errors.Is(err, fs.ErrNotExist) {
		return nil
	}
	if err != nil {
		return []check{{statusFail, name, fmt.Sprintf("the relay's data directory %s: %v", dir, err)}}
	}
	if e.account == nil {
		e.account = relayAccountName
	}
	if e.timeout <= 0 {
		e.timeout = 5 * time.Second
	}
	// The relay's account is the one that owns its data directory: the relay
	// refuses to run as any other. The checks are made as that account; the
	// binary only when doctor runs as it, since no other can tell which binary
	// the relay's service runs.
	acct := facts.UID
	asRelay := e.env.EUID == int(acct)
	judge := e.env
	judge.EUID = int(acct)
	if !asRelay {
		judge.Exe = nil
	}
	out := []check{relayAccountRow(judge, acct, e.account(acct))}
	problems := judge.Problems(e.layout, false)
	for _, p := range problems {
		out = append(out, check{statusFail, name, p})
	}
	if len(problems) == 0 {
		what := "the account, the data directory, the socket's directory and the pairing files"
		if judge.Exe != nil {
			what += " and this binary"
		}
		out = append(out, check{statusOK, name, what + " pass the relay's preflight"})
	}
	st, pairing := relayPairingRow(dir, acct, e.account(acct))
	out = append(out, pairing)
	out = append(out, relaySocketRow(ctx, e, acct, asRelay))
	if row, ok := relayAccessRow(e); ok {
		out = append(out, row)
	}
	if st != nil {
		out = append(out, relayServerRow(ctx, e, *st))
	}
	return out
}

// relayAccountRow says which account the relay runs as, and how the agents'
// clients will judge a socket it serves: by root's pin of the daemon's
// account, or with none, as a system account.
func relayAccountRow(e relay.Env, uid uint32, account string) check {
	const name = "relay.account"
	who := fmt.Sprintf("%s (uid %d)", account, uid)
	if e.DaemonAccount == nil {
		e.DaemonAccount = access.DaemonAccount
	}
	pin, pinned, err := e.DaemonAccount()
	switch {
	case err != nil:
		return check{statusFail, name, fmt.Sprintf("the relay runs as %s, but %v: seam, its hooks, its bridge and the Touch ID helper "+
			"trust no socket while it stands", who, err)}
	case pinned && pin.UID == uid:
		return check{statusOK, name, fmt.Sprintf("the relay runs as %s, the account %s pins: seam, its hooks, its bridge and the "+
			"Touch ID helper trust no other account answering the socket", who, pin.Path)}
	case pinned:
		return check{statusFail, name, fmt.Sprintf("the relay's data directory belongs to %s, but %s pins %s: the agents' clients "+
			"refuse a socket the relay serves as another account", who, pin.Path, pin)}
	}
	cmd := ""
	if access.ValidDaemonAccountName(account) {
		cmd = ": " + pinCommand(account)
	}
	return check{statusWarn, name, fmt.Sprintf("the relay runs as %s, and no pin at %s names it, so seam, its hooks, its bridge and "+
		"the Touch ID helper accept any system account answering the socket: pin it, as an administrator%s", who, access.DaemonAccountPath, cmd)}
}

// relayPairingRow reads the pairing: which server, which certificate it pins,
// and which device the server enrolled. The state comes back when it loads.
func relayPairingRow(dir string, uid uint32, account string) (*relay.State, check) {
	const name = "relay.pairing"
	st, err := relay.LoadState(dir)
	switch {
	case errors.Is(err, relay.ErrNotPaired):
		return nil, check{statusFail, name, fmt.Sprintf("not paired: pair it as %s with seamlessd relay pair --server URL --code CODE "+
			"(the code comes from the server's Devices page or seamlessd devices code)", account)}
	case errors.Is(err, fs.ErrPermission):
		return nil, check{statusInfo, name, fmt.Sprintf("%s is readable by the relay's account alone (%s, uid %d): run doctor as it "+
			"to see which server it is paired with and the certificate it pins", dir, account, uid)}
	case err != nil:
		return nil, check{statusFail, name, err.Error() + " -- pair it again with seamlessd relay pair --replace"}
	}
	return &st, check{statusOK, name, fmt.Sprintf("paired with %s as %s (device %s) on %s; the server's certificate is pinned "+
		"(SHA-256 %s); this relay's key is %s", st.ServerURL, st.Name, st.DeviceID, st.PairedAt.Format("2006-01-02"),
		st.ServerFingerprint, st.Fingerprint)}
}

// relaySocketRow checks the socket agents dial: the relay's own, and
// answering. Doctor run as another account (the owner) also sends /healthz
// through it, which the relay forwards: an answer is the whole path working.
func relaySocketRow(ctx context.Context, e relayDoctorEnv, acct uint32, asRelay bool) check {
	const name = "relay.socket"
	sock := e.layout.Socket
	f, err := e.env.Inspect(sock)
	switch {
	case errors.Is(err, fs.ErrNotExist):
		return check{statusFail, name, fmt.Sprintf("no socket at %s: the relay is not running -- start its service", sock)}
	case err != nil:
		return check{statusFail, name, fmt.Sprintf("%s: %v", sock, err)}
	case !f.Socket:
		return check{statusFail, name, fmt.Sprintf("%s is not a socket: the relay cannot serve the path agents dial", sock)}
	case f.UID != acct:
		return check{statusFail, name, fmt.Sprintf("%s belongs to uid %d, not the relay's account (uid %d): another process holds the "+
			"path agents dial", sock, f.UID, acct)}
	}
	cfg := config.Defaults()
	cfg.AdvertisedURL = "unix://" + sock
	client, err := cfg.HTTPClient(e.timeout)
	if err != nil {
		return check{statusFail, name, err.Error()}
	}
	if asRelay {
		// The relay's own account is not one the server serves: the dial is
		// the check, and relay.server below the server's side.
		conn, err := (&net.Dialer{Timeout: e.timeout}).DialContext(ctx, "unix", sock)
		if err != nil {
			return check{statusFail, name, fmt.Sprintf("the relay does not answer at %s: %v", sock, err)}
		}
		_ = conn.Close() //nolint:errcheck // a probe: the connection carried nothing
		return check{statusOK, name, sock + " is the relay's, and it answers"}
	}
	client.CheckRedirect = config.RefuseRedirect
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	status, line, err := relayHealthz(ctx, client, config.SocketBaseURL+"/healthz")
	switch {
	case err != nil:
		return check{statusFail, name, fmt.Sprintf("the relay does not answer at %s: %v", sock, err)}
	case status == http.StatusOK:
		return check{statusOK, name, sock + " is the relay's, and the server behind it answered /healthz through it"}
	case strings.HasPrefix(relayErrorText(line), "seamless relay: "):
		return check{statusFail, name, relayErrorText(line)}
	case status == http.StatusUnauthorized:
		return check{statusFail, name, fmt.Sprintf("the server refuses this relay (%s): pair it again", line)}
	}
	return check{statusWarn, name, fmt.Sprintf("the server answered /healthz through the relay with %d: %s", status, line)}
}

// relayAccessRow says who may connect to the relay's socket, and what that
// means: a server under peer_auth judges each uid the relay reports (for a
// relay peer_auth.relays maps), and one without it takes every caller for the
// owner, so on a computer with other accounts the socket's mode and group are
// the only thing between them and the owner's reach on the server.
func relayAccessRow(e relayDoctorEnv) (check, bool) {
	const name = "relay.access"
	f, err := e.env.Inspect(e.layout.Socket)
	if err != nil || !f.Socket {
		return check{}, false
	}
	if f.Mode.Perm()&0o006 == 0 {
		return check{statusInfo, name, fmt.Sprintf("only the relay's account and group %d may connect to %s (mode %04o)",
			f.GID, e.layout.Socket, f.Mode.Perm())}, true
	}
	return check{statusInfo, name, fmt.Sprintf("every local account may connect to %s (mode %04o): a server under peer_auth judges "+
		"each uid this relay reports (where peer_auth.relays maps it), and one without peer_auth takes every caller for the owner -- "+
		"on a computer with other accounts, restrict the socket: seamlessd relay serve --socket-mode 0660 --socket-group <a group "+
		"of the owner's alone>", e.layout.Socket, f.Mode.Perm())}, true
}

// relayServerRow sends /healthz to the server as the relay does: its
// certificate presented, the server's pinned.
func relayServerRow(ctx context.Context, e relayDoctorEnv, st relay.State) check {
	const name = "relay.server"
	rt, err := st.Transport(e.layout.DataDir)
	if err != nil {
		return check{statusFail, name, err.Error()}
	}
	defer rt.CloseIdleConnections()
	ctx, cancel := context.WithTimeout(ctx, e.timeout)
	defer cancel()
	status, line, err := relayHealthz(ctx, &http.Client{Transport: rt, CheckRedirect: config.RefuseRedirect}, st.ServerURL+"/healthz")
	accepted, why := relayProbeVerdict(status, err)
	switch {
	case errors.Is(err, devices.ErrServerFingerprint):
		return check{statusFail, name, fmt.Sprintf("%s presents a certificate other than the one pinned at pairing (SHA-256 %s), so the "+
			"relay refuses it: if the server's certificate was renewed, pair the relay again (seamlessd relay pair --replace)",
			st.ServerURL, st.ServerFingerprint)}
	case err != nil:
		return check{statusFail, name, fmt.Sprintf("%s does not answer (%v): every agent call through the relay fails until it does", st.ServerURL, err)}
	case accepted:
		return check{statusOK, name, st.ServerURL + " answers with the pinned certificate and accepts this relay"}
	case status == http.StatusUnauthorized:
		return check{statusFail, name, fmt.Sprintf("%s refuses this relay (%s): pair it again", st.ServerURL, line)}
	}
	return check{statusWarn, name, fmt.Sprintf("%s: %s (/healthz answered %d: %s)", st.ServerURL, why, status, line)}
}

// relayProbeVerdict reads the server's answer to a probe the relay sent as
// itself (no caller, so no relay headers): any answer but 401 or a 5xx is the
// server's edge accepting the relay's certificate. A server that judges
// relayed callers refuses the probe itself, which names none (400, 403); that
// is the pairing working.
func relayProbeVerdict(status int, err error) (accepted bool, why string) {
	switch {
	case err != nil:
		return false, ""
	case status == http.StatusUnauthorized:
		return false, "the server refuses this relay's certificate: pair it again"
	case status >= 500:
		return false, "the server answers, but not well"
	}
	return true, ""
}

// relayErrorText is the error a JSON refusal carries, else line itself.
func relayErrorText(line string) string {
	var e struct {
		Error string `json:"error"`
	}
	if json.Unmarshal([]byte(line), &e) == nil && e.Error != "" {
		return e.Error
	}
	return line
}

// relayHealthz GETs target through client and returns the status and the
// first line of the answer (for a refusal's words).
func relayHealthz(ctx context.Context, client *http.Client, target string) (int, string, error) {
	req, err := http.NewRequestWithContext(ctx, http.MethodGet, target, nil)
	if err != nil {
		return 0, "", err
	}
	resp, err := client.Do(req)
	if err != nil {
		return 0, "", err
	}
	defer func() { _ = resp.Body.Close() }()
	body, err := io.ReadAll(io.LimitReader(resp.Body, 4<<10))
	if err != nil {
		// The status is the answer; a body cut short only loses its words.
		return resp.StatusCode, "", nil
	}
	line, _, _ := strings.Cut(strings.TrimSpace(string(body)), "\n")
	return resp.StatusCode, line, nil
}
