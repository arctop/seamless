package main

// seamlessd peer-auth setup|status|undo (task 01M73MX602TYCQTJTBJYSRWKCV):
// the peer_auth guide's "Move an existing install over" as one guided,
// resumable command. Run as the owner, it reads the install, shows the whole
// move, and after one confirmation runs it: each root step is a block of
// commands for the owner's shell, every root command through sudo by absolute
// path (rootstep_commands.go's rules), and the owner-side steps (the client
// config, the hooks) in this process. Every finished step is recorded in a
// root-owned state file (pasStatePath), so a run that stopped -- a refused
// password, a failed check, a crash -- continues where it stopped, `status`
// says where it stands, and `undo` follows the guide's "Roll back".
//
// The binaries the daemon runs are the trust anchor: nothing an agent of the
// owner's could have replaced may run as root or as the daemon's account. A
// release's seamlessd and seam are downloaded for the exact tag and checked
// against its Sigstore-signed checksums.txt in this process, hashed, and
// copied by root into a staging directory only root can write, where the
// copy is checked against that hash -- and on macOS against Apple's
// notarization and the release's Developer ID team -- before it goes into
// place (pasBinaryCommands). A source build has no signature to check:
// --source-build copies the binaries beside this one with the hash check
// alone and says what is lost.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"io"
	"os"
	"path/filepath"
	"regexp"
	"slices"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/relay"
	"github.com/arctop/seamless/internal/shellcmd"
	"github.com/arctop/seamless/internal/update"
)

// The layout the move produces (docs-src/guides/peer-auth.md, "Deploy it").
const (
	pasRoot    = "/opt/seamless"
	pasEtc     = "/opt/seamless/etc"
	pasBin     = "/opt/seamless/bin"
	pasData    = "/opt/seamless/data"
	pasRunDir  = "/opt/seamless/run"
	pasHome    = "/opt/seamless/home"
	pasTLSDir  = setupPeerAuthTLSDir
	pasConfig  = setupPeerAuthConfig
	pasSocket  = relay.DefaultSocketPath
	pasAccount = relay.DefaultAccount
	// pasStage is where root's copies of the binaries are checked before they
	// go into place: root's own directory (0700), in which no other account --
	// the daemon's included, which holds the bin directory -- can make a file.
	pasStage = "/opt/seamless/stage"
	// pasStatePath is the state file: root's, readable by every account
	// (0644), so status and doctor read it as anyone while only root writes it.
	pasStatePath = "/opt/seamless/etc/peer-auth-setup.state"
	// pasReleaseSigners is the daemon's update.release_signers: root's copy
	// (0644) of the owner's pin, under directories only root can write, as
	// serve requires of a pin under peer_auth.
	pasReleaseSigners = "/opt/seamless/etc/release_signers"
	// pasBackupPath is the export taken before anything moves: root's alone
	// (0600), with every project in it.
	pasBackupPath = "/opt/seamless/before-peer-auth.tar.gz"
	// pasKeptSuffix marks the owner's files the move set aside (the server
	// config, the LaunchAgent's plist), which undo puts back.
	pasKeptSuffix = ".pre-peer-auth"
	// pasUID is where the search for a free macOS role id starts, going down:
	// the guide's example. It collides on some machines, so the search skips
	// every id in use as a user id or as a group id.
	pasUID = 480
)

// pasPlanVersion is the state file's plan format.
const pasPlanVersion = 1

// pasPlan is what a move was planned with, recorded as the state file's
// first line: a run that continues it reads it back rather than reading a
// world the finished steps already changed (the old data directory is gone
// after the move, the owner's config is a client's after the switch).
type pasPlan struct {
	V    int    `json:"v"`
	GOOS string `json:"goos"`
	// Owner is the account the move is for: its name, uid, primary group's
	// name (what undo hands the data back to) and home.
	Owner      string `json:"owner"`
	OwnerUID   int    `json:"owner_uid"`
	OwnerGroup string `json:"owner_group"`
	Home       string `json:"home"`
	// Account is the daemon's account; UID the macOS id it and its group are
	// made with, 0 when it exists already (Linux picks its own).
	Account string `json:"account"`
	UID     int    `json:"uid,omitempty"`
	// OldConfig and OldDataDir are the per-user install's.
	OldConfig  string `json:"old_config"`
	OldDataDir string `json:"old_data_dir"`
	// ServerURL is the TCP address the daemon keeps serving (the console's).
	ServerURL string `json:"server_url"`
	// TLSCert and TLSKey are the owner's files the daemon's copies come from,
	// "" without TLS.
	TLSCert string `json:"tls_cert,omitempty"`
	TLSKey  string `json:"tls_key,omitempty"`
	// Services are the per-user services the move retires: launchd labels
	// on macOS, the systemd user unit on Linux.
	Services []string `json:"services,omitempty"`
	// Release is the X.Y.Z whose binaries are installed; Source, for a
	// source build (--source-build), the directory they are copied from.
	Release string `json:"release,omitempty"`
	Source  string `json:"source,omitempty"`
	// Team is the Developer ID team a macOS release's binaries must carry:
	// this seamlessd's own, or, when it has none (a source build installing a
	// release), "" here and the team that signed the release's seamlessd,
	// which the release's Sigstore signature vouches for.
	Team string `json:"team,omitempty"`
	// RootOwnedBinaries keeps /opt/seamless/bin root's: the daemon then only
	// tells the owner about a new release (--root-owned-binaries).
	RootOwnedBinaries bool `json:"root_owned_binaries,omitempty"`
	// OldSeam is the seam the owner's Claude Code hooks ran before the move,
	// which undo puts them back on; "" when none was recorded.
	OldSeam string `json:"old_seam,omitempty"`
	// ReleaseSigners is the owner's update.release_signers, the file the
	// daemon's pin (pasReleaseSigners) is copied from, and SignersSHA256 the
	// hash of what it held when the move was planned: the release is held to
	// those keys before it is installed, and root's copy to that hash, so the
	// keys the owner confirmed are the keys pinned. "" without a pin.
	ReleaseSigners string `json:"release_signers,omitempty"`
	SignersSHA256  string `json:"signers_sha256,omitempty"`
}

// pasTeam is a Developer ID team identifier as codesign reports it.
var pasTeam = regexp.MustCompile(`^[A-Z0-9]{10}$`)

// pasSHA256 is a hex SHA-256, as the plan records one.
var pasSHA256 = regexp.MustCompile(`^[0-9a-f]{64}$`)

// pasPath reports whether p is an absolute path a root command may carry.
func pasPath(p string) bool {
	return filepath.IsAbs(p) && filepath.Clean(p) == p && !strings.ContainsFunc(p, setupControl)
}

// problem says why p cannot be run, "" when it can: every value the commands
// carry has its format, whatever wrote the state file. The commands quote
// each value as well; this keeps a wrong one from reaching them at all.
func (p pasPlan) problem() string {
	switch {
	case p.V != pasPlanVersion:
		return fmt.Sprintf("the plan's format is %d, and this seamlessd reads %d", p.V, pasPlanVersion)
	case p.GOOS != "darwin" && p.GOOS != "linux":
		return "peer_auth runs on macOS and Linux only"
	case !pasAccountName(p.Owner):
		return fmt.Sprintf("%q is not an account name", p.Owner)
	case p.OwnerUID <= 0 || uint64(p.OwnerUID) > uint64(^uint32(0)):
		return fmt.Sprintf("uid %d is not the owner's", p.OwnerUID)
	case !pasAccountName(p.OwnerGroup):
		return fmt.Sprintf("%q is not a group name", p.OwnerGroup)
	case p.Account != pasAccount:
		return fmt.Sprintf("the daemon's account is %s, not %q", pasAccount, p.Account)
	case p.GOOS == "darwin" && p.UID != 0 && (p.UID <= 200 || p.UID >= 500):
		return fmt.Sprintf("%d is not a free macOS role id (201-499)", p.UID)
	case p.GOOS == "linux" && p.UID != 0:
		return "a Linux account's id is useradd's to pick"
	case !pasPath(p.Home), !pasPath(p.OldConfig), !pasPath(p.OldDataDir):
		return "a path in the plan is not absolute"
	case p.OldDataDir == pasData || strings.HasPrefix(p.OldDataDir, pasRoot+"/"):
		return "the data directory is under " + pasRoot + " already"
	case (p.TLSCert == "") != (p.TLSKey == ""):
		return "the TLS certificate and key come as a pair"
	case p.TLSCert != "" && (!pasPath(p.TLSCert) || !pasPath(p.TLSKey)):
		return "a TLS path in the plan is not absolute"
	case p.TLSCert != "" && filepath.Base(p.TLSCert) == filepath.Base(p.TLSKey):
		return "the TLS certificate and key have the same file name"
	case p.TLSCert != "" && (!pasFileName(filepath.Base(p.TLSCert)) || !pasFileName(filepath.Base(p.TLSKey))):
		return "a TLS file name starts with a dot or holds a character a file name here may not"
	case (p.Release == "") == (p.Source == ""):
		return "the plan names neither a release nor a source build"
	case p.Source != "" && !pasPath(p.Source):
		return "the source build's directory is not absolute"
	case (p.ReleaseSigners == "") != (p.SignersSHA256 == ""):
		return "the release signers file and its hash come as a pair"
	case p.ReleaseSigners != "" && (!pasPath(p.ReleaseSigners) || !pasSHA256.MatchString(p.SignersSHA256)):
		return "the release signers file in the plan is not an absolute path with its sha256"
	case p.ReleaseSigners != "" && p.Release == "":
		return "update.release_signers installs only a release the owner signed, and the plan installs a source build"
	case p.OldSeam != "" && (!pasPath(p.OldSeam) || strings.HasPrefix(p.OldSeam, pasRoot+"/")):
		return "the hooks' seam before the move is not an absolute path outside " + pasRoot
	case p.Team != "" && (p.GOOS != "darwin" || p.Release == "" || !pasTeam.MatchString(p.Team)):
		return fmt.Sprintf("%q is not a Developer ID team", p.Team)
	}
	if p.Release != "" {
		if _, ok := update.Parse(p.Release); !ok {
			return fmt.Sprintf("%q is not a release version", p.Release)
		}
	}
	if _, err := pasServerURL(p.ServerURL); err != nil {
		return err.Error()
	}
	for _, s := range p.Services {
		if !slices.Contains(pasServiceNames(p.GOOS), s) {
			return fmt.Sprintf("%q is not a per-user service this move retires", s)
		}
	}
	return ""
}

// pasServiceNames are the per-user services a move may retire on goos.
func pasServiceNames(goos string) []string {
	if goos == "darwin" {
		return []string{launchdLabel, legacyLaunchdLabel}
	}
	return []string{systemdUnit}
}

// pasAccountName is an account or group name a command may carry: no
// leading dash, no space or control character.
func pasAccountName(s string) bool {
	return s != "" && len(s) <= 64 && !strings.HasPrefix(s, "-") && !strings.ContainsFunc(s, setupControl) &&
		!strings.ContainsAny(s, " \t:/")
}

// pasFileName is a file name the layout keeps (a TLS pair's): no leading dot
// (a staged copy's), no separator or control character.
func pasFileName(s string) bool {
	return s != "" && !strings.HasPrefix(s, ".") && !strings.ContainsFunc(s, setupControl) && !strings.Contains(s, "/")
}

// pasServerURL checks the TCP address the plan keeps: an http(s) URL with a
// host and a port, as ServerURL derives it.
func pasServerURL(raw string) (string, error) {
	if !strings.HasPrefix(raw, "http://") && !strings.HasPrefix(raw, "https://") {
		return "", fmt.Errorf("%q is not the daemon's http(s) address", raw)
	}
	if strings.ContainsFunc(raw, setupControl) || strings.ContainsAny(raw, " '\"\\`$") {
		return "", fmt.Errorf("%q is not the daemon's http(s) address", raw)
	}
	return raw, nil
}

// The state file's lines: the plan, then one line for each step finished or
// rolled back, in order. A new plan line starts a new move: a re-run after a
// finished undo plans again from the world as it then is.
const (
	pasLinePlan   = "plan"
	pasLineDone   = "done"
	pasLineUndone = "undone"
)

// pasState is the state file as read: the current plan (nil when there is
// none) and each of its steps' last record.
type pasState struct {
	plan   *pasPlan
	begun  time.Time
	status map[string]string
	at     map[string]time.Time
}

// pasParseState reads the state file. A line it cannot read refuses the file
// whole: root wrote every line, so one that does not parse is a file
// something else changed, and the moves it would guide are root's.
func pasParseState(data []byte) (pasState, error) {
	st := pasState{status: map[string]string{}, at: map[string]time.Time{}}
	for n, line := range strings.Split(strings.TrimRight(string(data), "\n"), "\n") {
		if line == "" {
			continue
		}
		bad := fmt.Errorf("%s line %d does not read as the state setup writes", pasStatePath, n+1)
		verb, rest, _ := strings.Cut(line, " ")
		switch verb {
		case pasLinePlan:
			ts, js, ok := strings.Cut(rest, " ")
			at, err := time.Parse(time.RFC3339, ts)
			if !ok || err != nil {
				return pasState{}, bad
			}
			var p pasPlan
			dec := json.NewDecoder(strings.NewReader(js))
			dec.DisallowUnknownFields()
			if err := dec.Decode(&p); err != nil {
				return pasState{}, bad
			}
			if why := p.problem(); why != "" {
				return pasState{}, fmt.Errorf("%s line %d: %s", pasStatePath, n+1, why)
			}
			st = pasState{plan: &p, begun: at, status: map[string]string{}, at: map[string]time.Time{}}
		case pasLineDone, pasLineUndone:
			id, ts, ok := strings.Cut(rest, " ")
			at, err := time.Parse(time.RFC3339, ts)
			if !ok || err != nil || st.plan == nil || pasStepByID(id) == nil {
				return pasState{}, bad
			}
			st.status[id], st.at[id] = verb, at
		default:
			return pasState{}, bad
		}
	}
	return st, nil
}

// pasPhase is where a move stands.
type pasPhase int

const (
	pasNone       pasPhase = iota // no move planned (or none since the last finished undo)
	pasInProgress                 // steps left to run
	pasDone                       // every step ran
	pasUndoing                    // an undo began and did not finish
	pasRolledBack                 // every step that ran was undone
)

// phase says where st stands.
func (st pasState) phase() pasPhase {
	if st.plan == nil {
		return pasNone
	}
	done, undone := 0, 0
	for _, s := range pasSteps {
		switch st.status[s.id] {
		case pasLineDone:
			done++
		case pasLineUndone:
			undone++
		}
	}
	switch {
	case undone > 0 && done > 0:
		return pasUndoing
	case undone > 0:
		return pasRolledBack
	case done == len(pasSteps):
		return pasDone
	}
	return pasInProgress
}

// next is the first step not yet done.
func (st pasState) next() *pasStepDef {
	for i := range pasSteps {
		if st.status[pasSteps[i].id] != pasLineDone {
			return &pasSteps[i]
		}
	}
	return nil
}

// pasRun is one run of setup or undo: the machine, the plan, and the
// directory its owner-side files are prepared in.
type pasRun struct {
	ctx  context.Context
	h    *pasHost
	plan pasPlan
	t    shellcmd.Tools
	// tmp is this run's own directory (0700) for the files root copies from
	// (the daemon's config, the downloaded binaries); removed at the end.
	tmp string
	// dryRun builds every step's commands and runs none.
	dryRun bool
}

// wheel is root's group on the plan's system.
func (x *pasRun) wheel() string {
	if x.plan.GOOS == "linux" {
		return "root"
	}
	return "wheel"
}

// runPeerAuth dispatches `seamlessd peer-auth <setup|status|undo>`.
func runPeerAuth(args []string) error {
	if len(args) == 0 {
		return errors.New("usage: seamlessd peer-auth setup|status|undo (see seamlessd peer-auth <verb> -h)")
	}
	h := realPasHost()
	switch args[0] {
	case "setup":
		return pasSetupCmd(h, args[1:])
	case "status":
		return pasStatusCmd(h, args[1:])
	case "undo":
		return pasUndoCmd(h, args[1:])
	}
	return fmt.Errorf("unknown peer-auth verb %q: setup, status or undo", args[0])
}

// pasOptions are setup's flags.
type pasOptions struct {
	yes, dryRun, sourceBuild, rootOwned bool
	release                             string
}

func pasSetupCmd(h *pasHost, args []string) error {
	fs := flag.NewFlagSet("peer-auth setup", flag.ContinueOnError)
	var o pasOptions
	fs.BoolVar(&o.yes, "yes", false, "go ahead without asking (the steps still ask for your password through sudo)")
	fs.BoolVar(&o.dryRun, "dry-run", false, "show the plan and every command it would run, and change nothing")
	fs.StringVar(&o.release, "release", "", "install release `X.Y.Z`'s binaries (default: this seamlessd's own release)")
	fs.BoolVar(&o.sourceBuild, "source-build", false, "install the binaries beside this seamlessd, a source build: no signature checks them, and the daemon never updates itself")
	fs.BoolVar(&o.rootOwned, "root-owned-binaries", false, "keep /opt/seamless/bin root's: the daemon then only tells you about a new release")
	if err := fs.Parse(args); err != nil {
		return err
	}
	if fs.NArg() > 0 {
		return fmt.Errorf("peer-auth setup takes no arguments (got %q)", fs.Arg(0))
	}
	return pasSetup(context.Background(), h, o)
}

// errPasRefused is a move setup will not start or continue; its message says
// why and what to do.
var errPasRefused = errors.New("peer-auth setup")

// pasSetup plans the move (or reads the plan a stopped run recorded), shows
// it, asks once, and runs every step not yet done.
func pasSetup(ctx context.Context, h *pasHost, o pasOptions) error {
	if why := pasHostProblem(h); why != "" {
		return fmt.Errorf("%w: %s", errPasRefused, why)
	}
	st, err := h.state()
	if err != nil {
		return err
	}
	var plan pasPlan
	var warnings []string
	resume := false
	switch st.phase() {
	case pasDone:
		fmt.Fprintf(h.out, "Locked projects are on: every step of the move ran (planned %s). seamlessd peer-auth status shows it.\n",
			st.begun.Format(time.RFC3339))
		return nil
	case pasUndoing:
		return fmt.Errorf("%w: an undo began and did not finish: run seamlessd peer-auth undo to finish it", errPasRefused)
	case pasInProgress:
		plan, resume = *st.plan, true
		if why := pasResumeProblem(h, plan); why != "" {
			return fmt.Errorf("%w: %s", errPasRefused, why)
		}
		if o.release != "" || o.sourceBuild || o.rootOwned {
			return fmt.Errorf("%w: a move is under way, and it continues with the plan it began with: run seamlessd peer-auth setup "+
				"without --release, --source-build or --root-owned-binaries, or seamlessd peer-auth undo to plan afresh", errPasRefused)
		}
	default:
		if o.release != "" && o.sourceBuild {
			return fmt.Errorf("%w: --release and --source-build name two different binaries: pass one", errPasRefused)
		}
		if plan, warnings, err = pasPreflight(ctx, h, o); err != nil {
			return err
		}
	}

	x := &pasRun{ctx: ctx, h: h, plan: plan, t: shellcmd.ToolsFor(plan.GOOS), dryRun: o.dryRun}
	if !o.dryRun {
		if x.tmp, err = h.tempDir(); err != nil {
			return err
		}
		defer func() { _ = os.RemoveAll(x.tmp) }()
	}
	pasShowPlan(h.out, plan, st, resume, warnings)
	if o.dryRun {
		return pasShowCommands(x, st)
	}
	if !o.yes {
		if !h.interactive {
			return fmt.Errorf("%w: nothing asked on a terminal: pass --yes to go ahead, or --dry-run to see every command", errPasRefused)
		}
		if !pasConfirm(h, "Go ahead? [y/N] ") {
			return fmt.Errorf("%w: not confirmed; nothing changed", errPasRefused)
		}
	}
	if !resume {
		if err := h.shell(ctx, rootstepChain(pasBeginCommands(x))); err != nil {
			return fmt.Errorf("%w: the state file could not be started (%w); nothing else changed", errPasRefused, err)
		}
	}
	for i := range pasSteps {
		s := &pasSteps[i]
		if st.status[s.id] == pasLineDone {
			continue
		}
		fmt.Fprintf(h.out, "\n%d/%d %s: %s\n", i+1, len(pasSteps), s.id, s.title(plan))
		if err := pasRunStep(x, s); err != nil {
			return fmt.Errorf("%w: step %d (%s) stopped: %w\n\nFix that and run seamlessd peer-auth setup again: it continues from this step. "+
				"seamlessd peer-auth undo rolls back what is done", errPasRefused, i+1, s.id, err)
		}
		if err := h.record(ctx, x.t, pasLineDone, s.id); err != nil {
			return fmt.Errorf("%w: step %d (%s) ran, but recording it failed (%w): a run again repeats it", errPasRefused, i+1, s.id, err)
		}
	}
	pasShowFinish(h.out, plan)
	return nil
}

// pasRunStep runs one step: its commands, built from the world as it is now,
// then its owner-side half.
func pasRunStep(x *pasRun, s *pasStepDef) error {
	if s.before != nil {
		if err := s.before(x); err != nil {
			return err
		}
	}
	if s.build != nil {
		cmds, err := s.build(x)
		if err != nil {
			return err
		}
		if len(cmds) > 0 {
			if err := x.h.shell(x.ctx, rootstepChain(cmds)); err != nil {
				return err
			}
		}
	}
	if s.after != nil {
		return s.after(x)
	}
	return nil
}

// pasConfirm asks question and reads y or yes.
func pasConfirm(h *pasHost, question string) bool {
	fmt.Fprint(h.out, question)
	line, err := h.in.ReadString('\n')
	if err != nil && line == "" {
		return false
	}
	a := strings.ToLower(strings.TrimSpace(line))
	return a == "y" || a == "yes"
}

// pasHostProblem is why this machine cannot run a move at all.
func pasHostProblem(h *pasHost) string {
	switch {
	case h.goos == "windows":
		return "Windows has no Unix-socket peer credentials, which peer_auth names every caller by, so locked projects run on macOS and Linux only"
	case h.goos != "darwin" && h.goos != "linux":
		return "peer_auth runs on macOS and Linux only"
	case h.euid == 0:
		return "run it as yourself, not as root: it asks for your password through sudo for the steps that need root"
	}
	return ""
}

// pasResumeProblem is why a recorded plan cannot be continued by this run.
func pasResumeProblem(h *pasHost, p pasPlan) string {
	switch {
	case p.GOOS != h.goos:
		return fmt.Sprintf("the move was planned on %s", p.GOOS)
	case p.Owner != h.owner.name || p.OwnerUID != h.owner.uid:
		return fmt.Sprintf("the move is %s's (uid %d): run it as that account", p.Owner, p.OwnerUID)
	}
	return ""
}

// pasStatusCmd prints where a move stands.
func pasStatusCmd(h *pasHost, args []string) error {
	fs := flag.NewFlagSet("peer-auth status", flag.ContinueOnError)
	if err := fs.Parse(args); err != nil {
		return err
	}
	st, err := h.state()
	if err != nil {
		return err
	}
	fmt.Fprintln(h.out, pasStatusLine(st))
	if st.plan == nil {
		return nil
	}
	for i, s := range pasSteps {
		mark := "pending"
		switch st.status[s.id] {
		case pasLineDone:
			mark = "done " + st.at[s.id].Format(time.RFC3339)
		case pasLineUndone:
			mark = "undone " + st.at[s.id].Format(time.RFC3339)
		}
		fmt.Fprintf(h.out, "  %2d. %-11s %-28s %s\n", i+1, s.id, mark, s.title(*st.plan))
	}
	return nil
}

// pasStatusLine is where st stands, in one line: status's first and
// doctor's.
func pasStatusLine(st pasState) string {
	switch st.phase() {
	case pasNone:
		return "no move to peer_auth recorded on this computer (" + pasStatePath + " holds none): seamlessd peer-auth setup starts one"
	case pasDone:
		return fmt.Sprintf("on: the move to peer_auth ran every step (planned %s)", st.begun.Format(time.RFC3339))
	case pasUndoing:
		return "an undo began and did not finish: run seamlessd peer-auth undo to finish it"
	case pasRolledBack:
		return fmt.Sprintf("rolled back: the move planned %s was undone; seamlessd peer-auth setup plans a new one", st.begun.Format(time.RFC3339))
	}
	n := 0
	for _, s := range pasSteps {
		if st.status[s.id] == pasLineDone {
			n++
		}
	}
	next := st.next()
	return fmt.Sprintf("in progress: %d of %d steps done, next %s (%s): run seamlessd peer-auth setup to continue, or seamlessd peer-auth undo",
		n, len(pasSteps), next.id, next.title(*st.plan))
}

// pasUndoCmd rolls back what a move did, per the guide's "Roll back".
func pasUndoCmd(h *pasHost, args []string) error {
	fs := flag.NewFlagSet("peer-auth undo", flag.ContinueOnError)
	yes := fs.Bool("yes", false, "go ahead without asking (the steps still ask for your password through sudo)")
	dryRun := fs.Bool("dry-run", false, "show every command the undo would run, and change nothing")
	if err := fs.Parse(args); err != nil {
		return err
	}
	return pasUndo(context.Background(), h, *yes, *dryRun)
}

func pasUndo(ctx context.Context, h *pasHost, yes, dryRun bool) error {
	if why := pasHostProblem(h); why != "" {
		return fmt.Errorf("%w: %s", errPasRefused, why)
	}
	st, err := h.state()
	if err != nil {
		return err
	}
	switch st.phase() {
	case pasNone:
		return fmt.Errorf("%w: no move to undo (%s holds none)", errPasRefused, pasStatePath)
	case pasRolledBack:
		fmt.Fprintln(h.out, "Nothing to undo: the move was rolled back already.")
		return nil
	}
	plan := *st.plan
	if why := pasResumeProblem(h, plan); why != "" {
		return fmt.Errorf("%w: %s", errPasRefused, why)
	}
	x := &pasRun{ctx: ctx, h: h, plan: plan, t: shellcmd.ToolsFor(plan.GOOS), dryRun: dryRun}
	todo := pasUndoPlan(st)
	fmt.Fprintln(h.out, "Roll back the move to peer_auth:")
	for _, u := range todo {
		fmt.Fprintf(h.out, "  - %s\n", u.title(plan))
	}
	fmt.Fprintf(h.out, "Kept: the account %s, %s (the binaries, the daemon's config) and the backup %s; delete them by hand once you no longer need them.\n",
		plan.Account, pasRoot, pasBackupPath)
	if dryRun {
		for _, u := range todo {
			if u.build == nil {
				continue
			}
			cmds, err := u.build(x)
			if err != nil {
				return err
			}
			if len(cmds) > 0 {
				fmt.Fprintf(h.out, "\n# %s\n%s\n", u.title(plan), rootstepChain(cmds))
			}
		}
		return nil
	}
	if !yes {
		if !h.interactive {
			return fmt.Errorf("%w: nothing asked on a terminal: pass --yes to go ahead, or --dry-run to see every command", errPasRefused)
		}
		if !pasConfirm(h, "Roll back? [y/N] ") {
			return fmt.Errorf("%w: not confirmed; nothing changed", errPasRefused)
		}
	}
	for _, u := range todo {
		fmt.Fprintf(h.out, "\n%s\n", u.title(plan))
		if u.before != nil {
			if err := u.before(x); err != nil {
				return fmt.Errorf("%w: undo %s stopped: %w; run seamlessd peer-auth undo again once it is fixed", errPasRefused, u.id, err)
			}
		}
		if u.build != nil {
			cmds, err := u.build(x)
			if err != nil {
				return fmt.Errorf("%w: undo %s stopped: %w; run seamlessd peer-auth undo again once it is fixed", errPasRefused, u.id, err)
			}
			if len(cmds) > 0 {
				if err := h.shell(ctx, rootstepChain(cmds)); err != nil {
					return fmt.Errorf("%w: undo %s stopped: %w; run seamlessd peer-auth undo again once it is fixed", errPasRefused, u.id, err)
				}
			}
		}
		if u.after != nil {
			if err := u.after(x); err != nil {
				return fmt.Errorf("%w: undo %s stopped: %w; run seamlessd peer-auth undo again once it is fixed", errPasRefused, u.id, err)
			}
		}
		if err := h.record(ctx, x.t, pasLineUndone, u.id); err != nil {
			return fmt.Errorf("%w: undo %s ran, but recording it failed: %w", errPasRefused, u.id, err)
		}
	}
	// The steps undo keeps are recorded too, so the state reads rolled back
	// and the next setup plans afresh, reusing what is there.
	for _, s := range pasSteps {
		if st.status[s.id] == pasLineDone && pasUndoByID(s.id) == nil {
			if err := h.record(ctx, x.t, pasLineUndone, s.id); err != nil {
				return fmt.Errorf("%w: recording %s as rolled back failed: %w", errPasRefused, s.id, err)
			}
		}
	}
	fmt.Fprintf(h.out, "\nRolled back. Your per-user daemon serves %s again from %s.\n", plan.OldDataDir, plan.OldConfig)
	return nil
}

// pasUndoPlan is the undo steps for what st records as done, in the order
// the guide rolls back: the system service first, the data back, the
// per-user service, then the owner's config and hooks.
func pasUndoPlan(st pasState) []*pasStepDef {
	var out []*pasStepDef
	for i := range pasUndoSteps {
		u := &pasUndoSteps[i]
		if st.status[u.id] == pasLineDone {
			out = append(out, u)
		}
	}
	return out
}

// pasShowPlan prints what the move does.
func pasShowPlan(w io.Writer, p pasPlan, st pasState, resume bool, warnings []string) {
	if resume {
		fmt.Fprintf(w, "Continuing the move to peer_auth planned %s:\n", st.begun.Format(time.RFC3339))
	} else {
		fmt.Fprintln(w, "Turn on locked projects (peer_auth): the daemon moves to an account of its own.")
	}
	row := func(label, text string) { fmt.Fprintf(w, "  %-9s %s\n", label, text) }
	acct := p.Account + ", a system account"
	switch {
	case p.UID != 0:
		acct += fmt.Sprintf(" made now (uid and gid %d)", p.UID)
	case p.GOOS == "linux":
		acct += " made now (useradd --system picks its id)"
	default:
		acct += " that is there already"
	}
	row("account", acct)
	row("binaries", pasBinariesWords(p))
	row("config", pasConfig+": your config with data_dir "+pasData+pasTLSWords(p)+" and a peer_auth block (owner "+p.Owner+", socket "+pasSocket+")")
	row("data", pasHomeWords(p, p.OldDataDir)+" moves to "+pasData)
	row("service", pasServiceWords(p))
	row("you", pasHomeWords(p, p.OldConfig)+" becomes a client's (the socket), the old one kept as "+filepath.Base(p.OldConfig)+pasKeptSuffix+
		"; "+pasHomeWords(p, pasConsoleConfig(p))+" keeps the key, for the console")
	row("hooks", "reinstalled to run "+pasBin+"/seam")
	row("backup", "first, an export of everything into "+pasBackupPath+", root's alone")
	for _, wr := range warnings {
		if rest, ok := strings.CutPrefix(wr, "passkeys: "); ok {
			row("passkeys", rest)
			continue
		}
		if rest, ok := strings.CutPrefix(wr, "signers: "); ok {
			row("signers", rest)
			continue
		}
		fmt.Fprintf(w, "  warning   %s\n", wr)
	}
	fmt.Fprintln(w, "\nSteps (the ones that need root ask for your password through sudo):")
	for i, s := range pasSteps {
		mark := ""
		if st.status[s.id] == pasLineDone {
			mark = " (done)"
		}
		fmt.Fprintf(w, "  %2d. %s%s\n", i+1, s.title(p), mark)
	}
	fmt.Fprintln(w, "\nseamlessd peer-auth setup --dry-run shows every command; a run that stops continues where it stopped.")
}

// pasShowCommands prints every step's commands as they would run now.
func pasShowCommands(x *pasRun, st pasState) error {
	if st.plan == nil {
		fmt.Fprintf(x.h.out, "\n# start the state file\n%s\n", rootstepChain(pasBeginCommands(x)))
	}
	for i := range pasSteps {
		s := &pasSteps[i]
		if st.status[s.id] == pasLineDone {
			continue
		}
		fmt.Fprintf(x.h.out, "\n# %d. %s\n", i+1, s.title(x.plan))
		if s.before != nil {
			fmt.Fprintf(x.h.out, "# first, in this process: %s\n", s.beforeWords(x.plan))
		}
		if s.build != nil {
			cmds, err := s.build(x)
			if err != nil {
				fmt.Fprintf(x.h.out, "# (shown once the steps before it ran: %v)\n", err)
				continue
			}
			if len(cmds) > 0 {
				fmt.Fprintln(x.h.out, rootstepChain(cmds))
			}
		}
		if s.after != nil {
			fmt.Fprintf(x.h.out, "# then, in this process: %s\n", s.afterWords(x.plan))
		}
	}
	return nil
}

// pasShowFinish is what comes after the move.
func pasShowFinish(w io.Writer, p pasPlan) {
	fmt.Fprintf(w, "\nLocked projects are ready to turn on. Next:\n")
	fmt.Fprintf(w, "  1. Enroll an approver on the console's Approvers page, which this opens:\n       SEAMLESS_CONFIG=%s seamlessd console-open --path /console/approvers\n"+
		"     Then lock a project from its Overview (Isolation card); it must be confidential or sealed first.\n",
		shellcmd.PosixArg(pasConsoleConfig(p)))
	fmt.Fprintf(w, "  2. Restart your agents (Claude Code, Codex) so their hooks and MCP bridge use the socket.\n")
	fmt.Fprintf(w, "  3. An outbound firewall (LuLu, Little Snitch) sees %s/seamlessd as a new program: allow it to reach your LLM provider and GitHub.\n", pasBin)
	if p.TLSKey != "" {
		fmt.Fprintf(w, "  4. Once the new daemon has proved itself, delete the TLS key in your home (%s): the daemon reads its own copy in %s, "+
			"and whatever renews the certificate now writes there, as root.\n", p.TLSKey, pasTLSDir)
	}
	fmt.Fprintf(w, "The export %s is the fallback if anything went wrong; delete it once the new install has proved itself.\n", pasBackupPath)
}

// pasBinariesWords says where the binaries come from and who holds them.
func pasBinariesWords(p pasPlan) string {
	var from string
	switch {
	case p.Source != "":
		from = "this source build's seamlessd and seam (from " + p.Source + "): no signature checks them, and the daemon never updates itself"
	case p.GOOS == "darwin":
		team := "the team that signed it"
		if p.Team != "" {
			team = "team " + p.Team + ", this seamlessd's own"
		}
		from = "release v" + p.Release + "'s seamlessd and seam, checked against the release's Sigstore signature, Apple's notarization and its Developer ID (" + team + ")"
	default:
		from = "release v" + p.Release + "'s seamlessd and seam, checked against the release's Sigstore signature"
	}
	if p.RootOwnedBinaries {
		return pasBin + ": " + from + "; root's, so the daemon only tells you about a new release"
	}
	if p.Source != "" {
		return pasBin + ": " + from
	}
	return pasBin + ": " + from + "; the daemon's account holds them, so it updates itself"
}

// pasTLSWords is the config's TLS change, "" without TLS.
func pasTLSWords(p pasPlan) string {
	if p.TLSCert == "" {
		return ""
	}
	return ", its TLS pair copied into " + pasTLSDir + " (the key root's and the daemon's alone)"
}

// pasSignersWords names the daemon's release signers pin, "" without one.
func pasSignersWords(p pasPlan) string {
	if p.ReleaseSigners == "" {
		return ""
	}
	return ", and its release signers pin " + pasReleaseSigners
}

// pasServiceWords says which service replaces which.
func pasServiceWords(p pasPlan) string {
	var old string
	if len(p.Services) == 0 {
		old = "(no per-user service was found: stop a daemon you run by hand first)"
	} else {
		old = "your per-user " + strings.Join(p.Services, " and ")
	}
	if p.GOOS == "darwin" {
		return "the LaunchDaemon " + systemDaemonLabel + " replaces " + old
	}
	return "the system unit " + systemDaemonUnit + " replaces " + old
}

// pasHomeWords shortens a path under the owner's home to ~/...
func pasHomeWords(p pasPlan, path string) string {
	if rest, ok := strings.CutPrefix(path, p.Home+"/"); ok {
		return "~/" + rest
	}
	return path
}

// pasConsoleConfig is the owner's console-only config, beside the client
// config.
func pasConsoleConfig(p pasPlan) string {
	return filepath.Join(filepath.Dir(p.OldConfig), "console.yaml")
}

// pasIDsFree picks a macOS role id free as a user id and as a group id,
// searching down from pasUID; 0 when none is.
func pasIDsFree(taken map[int]bool) int {
	for id := pasUID; id > 200; id-- {
		if !taken[id] {
			return id
		}
	}
	return 0
}

// pasRecordLine is one state line, stamped now.
func pasRecordLine(verb, id string, now time.Time) string {
	return verb + " " + id + " " + now.UTC().Format(time.RFC3339)
}

// pasPlanLine is the plan's state line.
func pasPlanLine(p pasPlan, now time.Time) (string, error) {
	js, err := json.Marshal(p)
	if err != nil {
		return "", err
	}
	return pasLinePlan + " " + now.UTC().Format(time.RFC3339) + " " + string(js), nil
}

// pasAppendCommands appends line to the state file as root.
func pasAppendCommands(t shellcmd.Tools, line string) []string {
	return []string{"printf '%s\\n' " + shellcmd.PosixArg(line) + " | " + t.AsRoot(t.Tee, "-a", pasStatePath, ">/dev/null")}
}

// pasBeginCommands start a move: root's /opt/seamless and its etc, and the
// plan as the state file's line, the file readable by every account.
func pasBeginCommands(x *pasRun) []string {
	line, err := pasPlanLine(x.plan, x.h.now())
	if err != nil {
		line = "" // a plan that does not marshal is never begun: problem() ran
	}
	cmds := []string{x.t.AsRoot(x.t.Install, "-d", "-o", "root", "-g", x.wheel(), "-m", "0755", pasRoot, pasEtc)}
	cmds = append(cmds, pasAppendCommands(x.t, line)...)
	return append(cmds, x.t.AsRoot(x.t.Chmod, "0644", pasStatePath))
}

// pasStatusDoctor is doctor's row for a move, nil when none was ever
// recorded on this computer.
func pasStatusDoctor(read func(string) ([]byte, error)) []check {
	data, err := read(pasStatePath)
	if errors.Is(err, os.ErrNotExist) {
		return nil
	}
	const name = "peer_auth setup"
	if err != nil {
		return []check{{statusWarn, name, "cannot read " + pasStatePath + ": " + err.Error()}}
	}
	st, err := pasParseState(data)
	if err != nil {
		return []check{{statusWarn, name, err.Error()}}
	}
	status := statusInfo
	switch st.phase() {
	case pasDone:
		status = statusOK
	case pasInProgress, pasUndoing:
		status = statusWarn
	}
	return []check{{status, name, pasStatusLine(st)}}
}

// pasStepWords is a title for a step, given the plan.
type pasStepWords func(pasPlan) string

// pasStepDef is one step of the move, or of its undo: build returns its
// commands for the owner's shell, built from the plan and the world as it
// is when the step runs (a re-run of a step half done skips what is there
// already); after runs its owner-side half once the commands succeeded.
type pasStepDef struct {
	id    string
	title pasStepWords
	// before runs in this process ahead of the step's commands.
	before      func(*pasRun) error
	beforeWords pasStepWords
	build       func(*pasRun) ([]string, error)
	after       func(*pasRun) error
	afterWords  pasStepWords
}

// pasStepByID finds a step of the move.
func pasStepByID(id string) *pasStepDef {
	for i := range pasSteps {
		if pasSteps[i].id == id {
			return &pasSteps[i]
		}
	}
	return nil
}

// pasUndoByID finds the undo of a step, nil for a step undo keeps.
func pasUndoByID(id string) *pasStepDef {
	for i := range pasUndoSteps {
		if pasUndoSteps[i].id == id {
			return &pasUndoSteps[i]
		}
	}
	return nil
}

// pasHostOwner is the account a run is for.
type pasHostOwner struct {
	name, group, home string
	uid               int
}

// pasRelease is a release setup downloaded and checked against its Sigstore
// signature: its binaries (name -> contents), the checksums.txt they were
// checked against, and the owner's signature over it (checksums.txt.sshsig),
// fetched only when asked for and nil when the release has none.
type pasRelease struct {
	bins      map[string][]byte
	checksums []byte
	ownerSig  []byte
}

// pasHost is what setup reads and runs on the machine; realPasHost is this
// one, and tests build their own.
type pasHost struct {
	goos, goarch string
	euid         int
	owner        pasHostOwner
	environ      []string
	interactive  bool
	in           *bufio.Reader
	out          io.Writer
	now          func() time.Time
	// exe is this seamlessd, symlinks resolved; version and distribution
	// its build's.
	exe, version, distribution string

	// world reads the install and the machine for the preflight.
	world pasWorld
	// shell runs a block of commands in the owner's shell (/bin/sh -c), on
	// this terminal, so sudo asks for the password there.
	shell func(ctx context.Context, script string) error
	// readFile reads a file as the owner (the state file, the owner's own).
	readFile func(string) ([]byte, error)
	// fetch downloads release v's binaries for this machine and checks them
	// against the release's Sigstore-signed checksums; with ownerSig it also
	// downloads the owner's signature over those checksums, when the release
	// has one.
	fetch func(ctx context.Context, v update.Version, ownerSig bool) (pasRelease, error)
	// readSigners reads the owner's update.release_signers as the updater
	// would trust it on a per-user install (pasReadSigners), returning what it
	// holds.
	readSigners func(path string) ([]byte, error)
	// team is the Developer ID team a binary is signed by ("" when none).
	team func(ctx context.Context, path string) string
	// health asks the daemon on the socket whether it serves.
	health func(ctx context.Context) error
	// reachable reports whether a daemon answers at a TCP host:port.
	reachable func(hostport string) bool
	// runOwner runs a command as the owner (install-hooks), on this terminal.
	runOwner func(ctx context.Context, argv []string, env []string) error
	// hookSeam is the seam the owner's Claude Code hooks run now, "" when
	// there are none (nil in tests that do not set it).
	hookSeam func() string
	// updaterBusy reports whether an updater holds the per-user install's
	// update lock (updaterLockHeld; nil in tests that do not set it).
	updaterBusy func(dataDir string) (bool, error)
	tempDir     func() (string, error)
	// wait is the interval between two polls of a daemon starting or stopping.
	wait time.Duration
}

// state reads the state file; a missing one is no move.
func (h *pasHost) state() (pasState, error) {
	data, err := h.readFile(pasStatePath)
	if errors.Is(err, os.ErrNotExist) {
		return pasState{status: map[string]string{}, at: map[string]time.Time{}}, nil
	}
	if err != nil {
		return pasState{}, fmt.Errorf("%w: cannot read %s: %w", errPasRefused, pasStatePath, err)
	}
	return pasParseState(data)
}

// record appends a step's line to the state file, as root.
func (h *pasHost) record(ctx context.Context, t shellcmd.Tools, verb, id string) error {
	return h.shell(ctx, rootstepChain(pasAppendCommands(t, pasRecordLine(verb, id, h.now()))))
}

// pasExeDir is the directory a source build's binaries are copied from.
func pasExeDir(exe string) string { return filepath.Dir(exe) }
