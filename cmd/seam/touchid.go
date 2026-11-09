package main

// seam touchid -- the macOS Touch ID approver (plan step T), installed and
// enrolled only when the owner asks: a default install neither contains nor
// launches it.
//
// `seam touchid enroll`, in the owner's own terminal on the Mac:
//
//  1. gets the Seamless Approve app: the release asset of this seam's own
//     version (seamless-approve_<v>_darwin.zip), checked against the
//     release's checksums.txt, which must carry the release workflow's
//     Sigstore signature for that tag, or a local build with --app;
//  2. checks its code signature: intact (codesign --verify --strict --deep),
//     the helper's bundle identifier, and the same Developer ID team as this
//     seam binary (the keychain access group that holds the key is bound to
//     that team too), plus Gatekeeper's notarization check for a release
//     asset -- a tampered, re-signed or foreign app is refused;
//  3. installs it atomically at ~/Applications/Seamless Approve.app,
//     replacing an existing install only after a y/N (a local build is
//     checked before that question, so a bad one is refused at once);
//  4. opens the app through LaunchServices at once, its dialog waiting for
//     the one-time enrollment code, and tells the owner how to mint that
//     code (as the daemon's account: only that account can) in another
//     terminal. The owner types the code into the app's own dialog, never
//     through seam; the app creates the Secure Enclave key, enrolls it, and
//     retires this Mac's earlier key with it. seam then confirms on the
//     daemon that a new Touch ID key exists and names the one it replaced.
//
// Nothing here is the proof of anything. The enrollment code is (the daemon
// checks it), and every later approval is the key's signature (the daemon's
// approve core checks it). The terminal check is a courtesy, as for seam
// approve: it keeps an agent's shell out of prompts it cannot answer.

import (
	"bufio"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"path/filepath"
	"slices"
	"strings"
	"time"
	"unicode"
	"unicode/utf8"

	"github.com/arctop/seamless/internal/config"
)

// approverBundleID is the helper app's bundle identifier, the Identifier its
// code signature must carry.
const approverBundleID = "com.arctop.seamless.approve"

// approversJSONPath is the Approvers page as JSON.
const approversJSONPath = "/console/approvers?format=json"

// approverKindTouchID is a Touch ID key's kind (store.ApproverTouchID).
const approverKindTouchID = "touchid"

// maxApproverNameRunes is the daemon's limit on a credential's name
// (store.MaxApproverNameRunes, pinned by a test).
const maxApproverNameRunes = 64

var (
	// errTouchIDNotMac is seam touchid enroll anywhere but macOS.
	errTouchIDNotMac = errors.New("the Touch ID approver exists only on macOS")
	// errTouchIDNotTerminal is seam touchid enroll without the owner's terminal.
	errTouchIDNotTerminal = errors.New("seam touchid enroll is the owner's own terminal path: it needs a terminal on stdin and stdout, " +
		"and an agent cannot use it")
)

var touchIDEnrollCmd = spec("touchid enroll", groupAccess,
	"install the macOS Touch ID approver and enroll its key (the owner's terminal)",
	noArgs(), bindTouchIDEnroll, runTouchIDEnroll).withLong(
	`Installs Seamless Approve, the small signed macOS app whose Secure Enclave
key approves an agent's access to a locked project with Touch ID (or the
Apple Watch, or the login password), and enrolls that key with the daemon.
Nothing installs it but this command.

The app is this seam's own release (seamless-approve_<version>_darwin.zip),
checked against the release's checksums.txt, which must carry the release
workflow's Sigstore signature for that version, as an update's does; a seam
built from source has no release to fetch, and installs a local build with
--app <path to Seamless Approve.app or its zip> instead. Before installing,
its code signature must verify, name the helper
(` + approverBundleID + `), and chain to Apple
for the same Developer ID team as this seam binary; a release asset must also
pass Gatekeeper's notarization check. When this seam is not signed with a
Developer ID, a release asset is held to the team that signed the release,
and for a local build the app's team is shown and you are asked. It installs at
~/Applications/Seamless Approve.app, replacing an installed one only after
you confirm; keeping it enrolls with the installed app (a local build named
with --app is checked first, and is then not used).

Then it opens the app, whose dialog waits for the one-time enrollment code,
and prints the command that mints the code, to run as the daemon's account
in another terminal: type the code into the app's dialog, and touch the
sensor. Afterwards it confirms on the daemon that a new Touch ID key is
enrolled. Re-enrolling replaces this Mac's earlier key: the app deletes it,
and the daemon retires its credential with the new enrollment once that
earlier key has signed for it, so no dead key is left listed. A key another
Mac holds, or a passkey, is never retired.

It needs macOS, a terminal on stdin and stdout (an agent cannot use it), and
server_url naming the daemon's peer_auth socket (unix:///path), which is how
the app reaches the daemon.`)

type touchIDEnrollOpts struct {
	app  *presentString
	name *presentString
}

func bindTouchIDEnroll(fs *flag.FlagSet) *touchIDEnrollOpts {
	o := &touchIDEnrollOpts{app: &presentString{}, name: &presentString{}}
	fs.Var(o.app, "app", "install from a local build at `PATH` (Seamless Approve.app, or a zip of it) instead of the release asset")
	fs.Var(o.name, "name", "list the key under `NAME` (default: Touch ID on <this host>)")
	return o
}

// approverEnrollArgs is the helper's command line for an enrollment: the
// socket to enroll over and the name to list the key under. The code is not
// among them: the owner types it into the helper's own dialog.
func approverEnrollArgs(socket, name string) []string {
	return []string{"enroll", "--socket", socket, "--name", name}
}

func runTouchIDEnroll(ctx context.Context, e *env, o *touchIDEnrollOpts, _ []string) error {
	if e.platform() != "darwin" {
		return errTouchIDNotMac
	}
	if !e.isTerminal() {
		return errTouchIDNotTerminal
	}
	if o.app.set && strings.TrimSpace(o.app.val) == "" {
		return errors.New("--app needs the path of a Seamless Approve.app, or of a zip holding one")
	}
	name := defaultKeyName(config.Hostname())
	if o.name.set {
		name = strings.TrimSpace(o.name.val)
		if err := validKeyName(name); err != nil {
			return err
		}
	}
	cfg, err := e.loadConfig()
	if err != nil {
		return err
	}
	sock := cfg.SocketPath()
	switch {
	case sock == "":
		return errNoApproverSocket
	case !filepath.IsAbs(sock):
		return fmt.Errorf("the peer_auth socket %q is not an absolute path", sock)
	}

	// The daemon first: how the owner mints the code, and the keys enrolled so
	// far. A daemon that locks nothing answers here, before anything installs.
	t, err := enrollCodeApprover(ctx, e, cfg)
	if err != nil {
		return err
	}
	before, err := touchIDKeys(cfg)
	if err != nil {
		return err
	}

	w := newTouchIDWorld()
	home, err := w.home()
	if err != nil {
		return fmt.Errorf("find the home directory: %w", err)
	}
	target := approverAppPath(home)
	in := bufio.NewReader(e.stdin)
	installed, err := installApprover(ctx, e, w, in, o.app, target)
	if err != nil || !installed {
		return err
	}

	// Checked again just before it opens, here as in the bridge's launch: the
	// app is on disk under ~/Applications, where the owner's agents can write.
	if err := verifyApproverForLaunch(ctx, e.run, w.self, target); err != nil {
		return err
	}

	// The app opens at once and its dialog waits for the code, so nothing
	// here waits on a key the owner might miss after switching terminals.
	fmt.Fprintf(e.stdout, "\nOpening Seamless Approve. Its window waits for a one-time enrollment code.\n"+
		"Mint the code as the daemon's account: in another terminal on this Mac, run\n\n  %s\n\n",
		strings.Join(shellWords(enrollCodeArgv(t)), " "))
	fmt.Fprintln(e.stdout, "It asks for your password or Touch ID and prints the code, which works once, for 10 minutes. "+
		"Type the code into the Seamless Approve window, then touch the sensor. This terminal waits until Seamless Approve closes.")
	argv := append([]string{"open", "-W", "-n", "-a", target, "--args"}, approverEnrollArgs(sock, name)...)
	code, err := e.run(ctx, argv, nil, io.Discard, e.stderr)
	if err != nil {
		return fmt.Errorf("open Seamless Approve: %w", err)
	}
	if code != 0 {
		return fmt.Errorf("open could not start Seamless Approve at %s (exit status %d)", target, code)
	}
	after, err := touchIDKeys(cfg)
	if err != nil {
		return fmt.Errorf("the daemon's approvers could not be read to confirm the new key (Seamless Approve has finished): %w", err)
	}
	added := newKeys(before, after)
	if len(added) == 0 {
		return errors.New("no new Touch ID key is enrolled: Seamless Approve was cancelled, or the daemon refused the code " +
			"(it may be mistyped, expired or already used) -- mint a fresh code and run seam touchid enroll again")
	}
	for _, k := range added {
		fmt.Fprintf(e.stdout, "Enrolled the Touch ID key %q (%s). An agent's access dialog can now be approved with it.\n",
			printable(k.Name), printable(k.ID))
	}
	reportReplaced(e.stdout, t, before, after, added)
	return nil
}

// reportReplaced says what became of the Touch ID keys enrolled before: the
// ones the daemon no longer lists (the app's enrollment retired them, each
// having signed as this Mac's earlier key), and the ones still listed under a
// new key's name, which only their dates now tell apart.
func reportReplaced(w io.Writer, t terminalApprover, before, after, added []approverKey) {
	for _, k := range newKeys(after, before) {
		fmt.Fprintf(w, "Retired the Touch ID key %q (%s, added %s): it was this Mac's earlier key, and the daemon no longer lists it.\n",
			printable(k.Name), printable(k.ID), k.AddedAt.Local().Format("2006-01-02 15:04"))
	}
	for _, k := range after {
		if slices.ContainsFunc(added, func(a approverKey) bool { return a.ID == k.ID }) ||
			!slices.ContainsFunc(added, func(a approverKey) bool { return a.Name == k.Name }) {
			continue
		}
		fmt.Fprintf(w, "The daemon still lists an older Touch ID key named %q (%s, added %s). ",
			printable(k.Name), printable(k.ID), k.AddedAt.Local().Format("2006-01-02 15:04"))
		if !approverRequestID.MatchString(k.ID) { // an approver id is a ULID, as a request id is
			fmt.Fprintln(w, "If it was this Mac's, remove it with `seamlessd approvers remove`, run as the daemon's account; if it is another Mac's, keep it.")
			continue
		}
		fmt.Fprintf(w, "If it was this Mac's, remove it as the daemon's account:\n\n  %s\n\nIf it is another Mac's, keep it.\n",
			strings.Join(shellWords(removeApproverArgv(t, k.ID)), " "))
	}
}

// removeApproverArgv removes one credential as the daemon's account, as
// enrollCodeArgv mints a code.
func removeApproverArgv(t terminalApprover, id string) []string {
	return []string{"sudo", "-k", "-u", t.Account, "env", "SEAMLESS_CONFIG=" + t.Config, t.Seamlessd, "approvers", "remove", id}
}

// defaultKeyName is the name a key is listed under when --name is absent.
func defaultKeyName(host string) string {
	name := "Touch ID"
	if h := strings.TrimSpace(host); h != "" && validKeyName(name+" on "+h) == nil {
		name += " on " + h
	}
	return name
}

// validKeyName holds a key's name to what the daemon accepts, and to what can
// be the helper's argument: a name that began with "-" would read as a flag.
func validKeyName(name string) error {
	n := utf8.RuneCountInString(name)
	switch {
	case n == 0 || n > maxApproverNameRunes || !utf8.ValidString(name) ||
		strings.ContainsFunc(name, func(r rune) bool { return !unicode.IsPrint(r) }):
		return fmt.Errorf("invalid value %q for --name: a name is 1 to %d printable characters on one line", name, maxApproverNameRunes)
	case strings.HasPrefix(name, "-"):
		return fmt.Errorf("invalid value %q for --name: a name may not begin with -", name)
	}
	return nil
}

// enrollCodeApprover reads, from the daemon's access JSON, the account and
// paths the enrollment code is minted with, held to the shapes seam approve
// holds them to before they become a command line. The daemon is verified
// first, as seam approve verifies it: they become that command line either way.
func enrollCodeApprover(ctx context.Context, e *env, cfg config.Config) (terminalApprover, error) {
	daemon, err := verifyDaemon(ctx, e, cfg)
	if err != nil {
		return terminalApprover{}, err
	}
	var list accessList
	if err := accessGetVia(daemon.client, cfg, accessListPath, &list); err != nil {
		return terminalApprover{}, err
	}
	t := list.Terminal
	if t == nil || t.Account == "" || t.Seamlessd == "" || t.Config == "" {
		return terminalApprover{}, errors.New("the daemon did not say how to reach its own account: mint the code with " +
			"`seamlessd approvers enroll-code touchid`, run as the daemon's account, then run seam touchid enroll again")
	}
	switch {
	case !accountName.MatchString(t.Account):
		return terminalApprover{}, fmt.Errorf("the daemon named %q as its account, which is not an account name", t.Account)
	case isRootAccount(t.Account):
		return terminalApprover{}, errRootAccount(t.Account)
	case !absPath(t.Seamlessd):
		return terminalApprover{}, fmt.Errorf("the daemon named %q as its seamlessd, which is not an absolute path", t.Seamlessd)
	case !absPath(t.Config):
		return terminalApprover{}, fmt.Errorf("the daemon named %q as its config, which is not an absolute path", t.Config)
	}
	if err := daemon.accountIsPeer(e, t.Account); err != nil {
		return terminalApprover{}, err
	}
	return *t, nil
}

// enrollCodeArgv mints a Touch ID enrollment code as the daemon's account. -k
// makes sudo ask every time and keep no ticket an agent could reuse.
func enrollCodeArgv(t terminalApprover) []string {
	return []string{"sudo", "-k", "-u", t.Account, "env", "SEAMLESS_CONFIG=" + t.Config, t.Seamlessd, "approvers", "enroll-code", "touchid"}
}

// shellWords quotes each argument for a POSIX shell, so a printed command
// line pastes as the arguments it names.
func shellWords(argv []string) []string {
	out := make([]string, len(argv))
	for i, a := range argv {
		if a != "" && !strings.ContainsFunc(a, func(r rune) bool {
			return !(r < utf8.RuneSelf && (unicode.IsLetter(r) || unicode.IsDigit(r) || strings.ContainsRune("_./=:,+@%-", r)))
		}) {
			out[i] = a
			continue
		}
		out[i] = "'" + strings.ReplaceAll(a, "'", `'\''`) + "'"
	}
	return out
}

// approverKey is one enrolled credential in the Approvers JSON.
type approverKey struct {
	ID         string     `json:"id"`
	Kind       string     `json:"kind"`
	Name       string     `json:"name"`
	Synced     bool       `json:"synced"`
	AddedAt    time.Time  `json:"added_at"`
	AddedBy    string     `json:"added_by"`
	LastUsedAt *time.Time `json:"last_used_at,omitempty"`
}

// approversPage is GET /console/approvers?format=json.
type approversPage struct {
	Credentials []approverKey `json:"credentials"`
}

// touchIDKeys lists the daemon's enrolled Touch ID keys.
func touchIDKeys(cfg config.Config) ([]approverKey, error) {
	var page approversPage
	if err := accessGet(cfg, approversJSONPath, &page); err != nil {
		return nil, err
	}
	var keys []approverKey
	for _, c := range page.Credentials {
		if c.Kind == approverKindTouchID {
			keys = append(keys, c)
		}
	}
	return keys, nil
}

// newKeys is after's keys that before did not list.
func newKeys(before, after []approverKey) []approverKey {
	var out []approverKey
	for _, k := range after {
		if !slices.ContainsFunc(before, func(b approverKey) bool { return b.ID == k.ID }) {
			out = append(out, k)
		}
	}
	return out
}

// --- touchid status ---

var touchIDStatusCmd = spec("touchid status", groupAccess,
	"is the Touch ID approver installed, and which Touch ID keys are enrolled",
	noArgs(), bindNoOpts, runTouchIDStatus).withLong(
	`Says whether Seamless Approve is installed at ~/Applications/Seamless
Approve.app and how it is signed, whether server_url names the peer_auth
socket the app talks to the daemon over, and which Touch ID keys the daemon
has enrolled. It changes nothing; seam touchid enroll installs and enrolls.`)

func runTouchIDStatus(ctx context.Context, e *env, _ *noOpts, _ []string) error {
	if e.platform() != "darwin" {
		return errTouchIDNotMac
	}
	w := newTouchIDWorld()
	home, err := w.home()
	if err != nil {
		return fmt.Errorf("find the home directory: %w", err)
	}
	target := approverAppPath(home)
	if !isDir(target) {
		fmt.Fprintf(e.stdout, "Seamless Approve: not installed at %s (seam touchid enroll installs it)\n", printable(target))
	} else {
		sig, err := describeSignature(ctx, e.run, target)
		intact := false
		if err == nil {
			code, rerr := e.run(ctx, []string{"codesign", "--verify", "--strict", "--deep", target}, nil, io.Discard, io.Discard)
			intact = rerr == nil && code == 0
		}
		switch {
		case err != nil:
			fmt.Fprintf(e.stdout, "Seamless Approve: installed at %s, but its signature cannot be read: %v\n", printable(target), err)
		default:
			state := "signature intact"
			if !intact {
				state = "signature DOES NOT VERIFY"
			}
			fmt.Fprintf(e.stdout, "Seamless Approve: installed at %s (%s, %s, %s)\n",
				printable(target), printable(orDash(sig.identifier)), teamWords(sig.team), state)
		}
	}
	cfg, err := e.loadConfig()
	if err != nil {
		return err
	}
	if sock := cfg.SocketPath(); sock != "" {
		fmt.Fprintf(e.stdout, "server_url: the peer_auth socket %s\n", printable(sock))
	} else {
		fmt.Fprintln(e.stdout, "server_url: not the peer_auth socket, so Seamless Approve cannot reach the daemon from here")
	}
	keys, err := touchIDKeys(cfg)
	if err != nil {
		return fmt.Errorf("the daemon's Touch ID keys: %w", err)
	}
	if len(keys) == 0 {
		fmt.Fprintln(e.stdout, "Touch ID keys: none enrolled")
		return nil
	}
	fmt.Fprintln(e.stdout, "Touch ID keys:")
	for _, k := range keys {
		used := "never used"
		if k.LastUsedAt != nil {
			used = "last used " + k.LastUsedAt.Local().Format("2006-01-02 15:04")
		}
		fmt.Fprintf(e.stdout, "  %s  %q  added %s, %s\n", printable(k.ID), printable(k.Name), k.AddedAt.Local().Format("2006-01-02 15:04"), used)
	}
	return nil
}

// teamWords names a signature's team for the owner.
func teamWords(team string) string {
	if team == "" {
		return "no Developer ID team (ad hoc or unsigned)"
	}
	return "team " + team
}
