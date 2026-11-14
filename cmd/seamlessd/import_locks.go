package main

import (
	"bufio"
	"context"
	"errors"
	"fmt"
	"io"
	"os"
	"strconv"
	"strings"

	"github.com/arctop/seamless/internal/archive"
	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/store"
)

// lockGate decides what an import does with the locked projects in its archive.
// A locked project opens with an approval, and a console lock comes off only
// with a passkey, so an archive that brings locks to an instance with no passkey
// able to open them is the one case that asks.
//
// It judges what a row can show: that a passkey is enrolled for this relying
// party and can reach this machine. Whether the owner still holds it takes a
// ceremony in a browser against a running daemon, which a restore has not got
// yet, so the import asks the owner and its closing advice has them use it.
type lockGate struct {
	cfg         config.Config
	flags       *importFlags
	interactive bool
	in          *bufio.Reader
	out         io.Writer
	host        string
	daemonUp    func() bool
	// local reads the passkeys enrolled in the destination, for a merge.
	local func(context.Context) ([]store.PasskeyFact, error)
	// answers names the two answers the refusal offers when the gate cannot
	// ask; the zero value is seamlessd import's flags (importLockAnswers).
	answers lockAnswers

	// What the decision was, for advise.
	asked   bool
	keeping bool
	usable  []string
	first   string // a locked project, for the advice's link
}

// lockAnswers is how a caller of the gate takes its two answers without a
// terminal: keep and remove, each the clause the refusal ends with.
type lockAnswers struct{ keep, remove string }

// importLockAnswers are seamlessd import's: its --keep-locks and
// --remove-locks flags.
var importLockAnswers = lockAnswers{
	keep:   "Pass --keep-locks to import them locked and add a passkey right after",
	remove: "--remove-locks to import them without their locks",
}

// lockAct is one answer to the gate's question.
type lockAct int

const (
	lockActKeep lockAct = iota
	lockActRemove
	lockActStop
)

type lockOption struct {
	label string
	act   lockAct
}

// errImportStopped is an import the owner stopped at the gate. Nothing has been
// written by then.
var errImportStopped = errors.New("nothing was changed")

// decide is archive.ImportOptions.OnLocks.
func (g *lockGate) decide(ctx context.Context, mode archive.Mode, m archive.Manifest) (archive.LockChoice, error) {
	slugs := m.Locks.Slugs()
	g.asked, g.first = true, slugs[0]
	usable, reason, err := g.passkeys(ctx, mode, m)
	if err != nil {
		return archive.LocksKeep, err
	}
	g.usable = usable

	fmt.Fprintf(g.out, "\nThis backup holds %s: %s.\n", projectCount(len(slugs)), strings.Join(slugs, ", "))
	if len(usable) > 0 {
		fmt.Fprintf(g.out, "Passkeys that can open them here: %s.\n", quotedList(usable))
	} else {
		fmt.Fprintf(g.out, "No passkey can open them here: %s.\n", reason)
	}

	act, err := g.answer(mode, slugs, reason)
	if err != nil {
		return archive.LocksKeep, err
	}
	switch act {
	case lockActRemove:
		return archive.LocksRemove, nil
	case lockActStop:
		return archive.LocksKeep, g.stopped(mode)
	}
	g.keeping = true
	// A merge does not need the daemon down, except for this: the daemon reads
	// the locked projects when it starts, so until it restarts these projects
	// would be open, with their content already in.
	if mode == archive.ModeMerge && !g.flags.dryRun && !g.flags.force && g.daemonUp() {
		return archive.LocksKeep, fmt.Errorf("%w: a daemon reads the locked projects when it starts, so these would be open until it restarted. "+
			"Nothing was changed: stop the daemon (seamlessd stop) and import again, or pass --force and restart it right after", archive.ErrDaemonRunning)
	}
	return archive.LocksKeep, nil
}

// answer settles the question: from the flags, from the terminal, or by
// refusing when neither can.
func (g *lockGate) answer(mode archive.Mode, slugs []string, reason string) (lockAct, error) {
	switch {
	case g.flags.removeLocks:
		return lockActRemove, nil
	case g.flags.keepLocks:
		return lockActKeep, nil
	case g.flags.dryRun:
		if len(g.usable) == 0 {
			fmt.Fprintln(g.out, "A real import will ask what to do, or stop without a terminal until you pass --keep-locks or --remove-locks.")
		}
		return lockActKeep, nil
	}
	if len(g.usable) > 0 {
		if !g.interactive {
			return lockActKeep, nil // the locks stay on, which is the safe way to be wrong
		}
		if g.confirmHeld() {
			return lockActKeep, nil
		}
		g.usable = nil // the closing advice must not send them to a passkey they lack
	}
	if !g.interactive {
		answers := g.answers
		if answers == (lockAnswers{}) {
			answers = importLockAnswers
		}
		return lockActStop, fmt.Errorf("%s (%s) and no passkey can open them here: %s. Nothing was changed. %s, or %s",
			"this backup holds "+projectCount(len(slugs)), strings.Join(slugs, ", "), reason, answers.keep, answers.remove)
	}
	return g.choose(g.options(mode))
}

// confirmHeld asks whether the owner still holds a passkey the row says exists.
func (g *lockGate) confirmHeld() bool {
	fmt.Fprint(g.out, "Do you still have one of them? [y/N]: ")
	line, _ := g.in.ReadString('\n') //nolint:errcheck // EOF or a short read still yields the text read; an empty line declines, which leads to the menu
	switch strings.ToLower(strings.TrimSpace(line)) {
	case "y", "yes":
		return true
	}
	return false
}

// options are the answers on offer, the recommended one first. A fresh restore
// has no "add a passkey first": a daemon started on this empty data dir to add
// one would create a database, and this import would then be a merge.
func (g *lockGate) options(mode archive.Mode) []lockOption {
	remove := lockOption{"Import them and remove their locks. Agents in those projects then read them without asking.", lockActRemove}
	if !g.cfg.PeerAuth.Enabled {
		return []lockOption{
			{"Stop, so I can turn on peer_auth first. Nothing has changed.", lockActStop},
			{"Import them and keep the locks. Locks do nothing until peer_auth is on, so turn it on before starting the daemon.", lockActKeep},
			remove,
		}
	}
	keep := lockOption{"Import them and keep the locks, and add a passkey right after.", lockActKeep}
	if mode == archive.ModeFresh {
		return []lockOption{keep, remove, {"Cancel. Nothing has changed.", lockActStop}}
	}
	return []lockOption{{"Stop, so I can add a passkey first. Nothing has changed.", lockActStop}, keep, remove}
}

// choose runs the menu. End of input is a stop: nothing has been written.
func (g *lockGate) choose(opts []lockOption) (lockAct, error) {
	fmt.Fprintln(g.out, "\nWhat do you want to do?")
	for i, o := range opts {
		fmt.Fprintf(g.out, "  %d. %s\n", i+1, o.label)
	}
	for {
		fmt.Fprintf(g.out, "Choose 1-%d [1]: ", len(opts))
		line, err := g.in.ReadString('\n')
		text := strings.TrimSpace(line)
		if text == "" && err != nil {
			fmt.Fprintln(g.out)
			return lockActStop, nil
		}
		if text == "" {
			return opts[0].act, nil
		}
		if n, convErr := strconv.Atoi(text); convErr == nil && n >= 1 && n <= len(opts) {
			return opts[n-1].act, nil
		}
		fmt.Fprintf(g.out, "%q is not one of the choices.\n", text)
	}
}

// stopped is the error for an owner's stop, naming the next step.
func (g *lockGate) stopped(mode archive.Mode) error {
	_, origins := g.cfg.RelyingParty()
	switch {
	case !g.cfg.PeerAuth.Enabled:
		return fmt.Errorf("%w: turn on peer_auth in this config, then run this import again", errImportStopped)
	case mode == archive.ModeMerge:
		return fmt.Errorf("%w: with the daemon running, add a passkey on %s/console/approvers, then stop the daemon and run this import again", errImportStopped, origins[0])
	}
	return errImportStopped
}

// passkeys returns the labels of the passkeys that can open these projects here,
// or why none can.
func (g *lockGate) passkeys(ctx context.Context, mode archive.Mode, m archive.Manifest) ([]string, string, error) {
	if !g.cfg.PeerAuth.Enabled {
		return nil, "peer_auth is off in this config, so locks are not enforced and no passkey can be used", nil
	}
	if mode == archive.ModeMerge {
		facts, err := g.local(ctx)
		if err != nil {
			return nil, "", err
		}
		var names []string
		for _, p := range facts {
			names = append(names, p.Name)
		}
		return names, "no passkey is enrolled on this instance", nil
	}
	// A fresh restore brings the backup's passkeys with its database.
	rpID, _ := g.cfg.RelyingParty()
	names, reason := restoredPasskeys(*m.Locks, m.SourceHost, g.host, rpID)
	return names, reason, nil
}

// restoredPasskeys returns which of a backup's passkeys could work after a
// restore on host under relying party rpID, or why none could. A passkey signs
// for the relying party it was enrolled for and no other, and one tied to the
// machine that enrolled it does not come along to a new one. An empty recorded
// relying party is unknown, and does not count against the passkey.
func restoredPasskeys(l archive.Locks, sourceHost, host, rpID string) ([]string, string) {
	if len(l.Passkeys) == 0 {
		return nil, "the backup carries no passkey"
	}
	if l.PasskeyRPID != "" && l.PasskeyRPID != rpID {
		return nil, fmt.Sprintf("the backup's passkeys belong to %q and this instance uses %q", l.PasskeyRPID, rpID)
	}
	var names []string
	for _, p := range l.Passkeys {
		if p.Portable || sourceHost == host {
			names = append(names, p.Name)
		}
	}
	if len(names) == 0 {
		return nil, fmt.Sprintf("the backup's passkeys are tied to %q and this is %q", sourceHost, host)
	}
	return names, ""
}

// advise says what to do once the import has finished, so that the owner ends up
// able to open what was just locked. The check it names is a real one: unlocking
// a locked project takes the passkey itself.
func (g *lockGate) advise(w io.Writer, rep *archive.ImportReport) {
	if !g.asked || !g.keeping || rep == nil || rep.DryRun || rep.Locks == nil || len(rep.Locks.Locked) == 0 {
		return
	}
	fmt.Fprintln(w)
	if !g.cfg.PeerAuth.Enabled {
		fmt.Fprintln(w, "peer_auth is off in this config, so these locks are not enforced yet. Turn it on before you start the daemon.")
		return
	}
	_, origins := g.cfg.RelyingParty()
	origin := origins[0]
	start := "Start the daemon (seamlessd start)"
	if g.flags.force && rep.Mode == archive.ModeMerge {
		start = "Restart the daemon now: until it restarts, these projects are not locked"
	}
	if len(g.usable) > 0 {
		fmt.Fprintf(w, "%s, then check that your passkey still opens them: %s/console/unlock/%s\n", start, origin, g.first)
		fmt.Fprintf(w, "If it does not, add another on %s/console/approvers with a one-time code from your terminal.\n", origin)
		return
	}
	fmt.Fprintln(w, "Add a passkey now, so you can open these projects:")
	fmt.Fprintf(w, "  1. %s.\n", start)
	fmt.Fprintf(w, "  2. Open %s/console/approvers and create one. The page shows the command that makes its one-time code.\n", origin)
	fmt.Fprintf(w, "  3. Check it by unlocking a project with it: %s/console/unlock/%s\n", origin, g.first)
}

// localPasskeys reads the passkeys enrolled in cfg's database. It never migrates
// it, and no database yet means none enrolled.
func localPasskeys(ctx context.Context, cfg config.Config) ([]store.PasskeyFact, error) {
	db, err := store.OpenExisting(cfg.DBPath())
	if errors.Is(err, os.ErrNotExist) {
		return nil, nil
	}
	if err != nil {
		return nil, err
	}
	defer func() { _ = db.Close() }()
	facts, err := store.ReadLockFacts(ctx, db)
	if err != nil {
		return nil, err
	}
	return facts.Passkeys, nil
}

func projectCount(n int) string {
	if n == 1 {
		return "1 locked project"
	}
	return strconv.Itoa(n) + " locked projects"
}

func quotedList(names []string) string {
	out := make([]string, len(names))
	for i, n := range names {
		out[i] = strconv.Quote(n)
	}
	return strings.Join(out, ", ")
}
