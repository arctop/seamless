package main

// seam proposals, seam promote, seam proposals discard -- the owner's terminal
// path for the restricted accounts' proposals.
//
// A restricted local account (peer_auth.restricted) never lands a change to a
// constraint, convention, protocol or stage. The write waits in the console's
// held queue as that account's proposal, outside every briefing, recall and
// gardener pass, until the owner promotes it (the queue's release: it lands
// exactly as written) or discards it (nothing lands). The console's Held screen
// does both; these commands do the same over its JSON routes:
//
//	GET  /console/held?format=json          the queue (proposals are the rows
//	                                        whose reasons carry account.restricted)
//	POST /console/held/<id>/release         promote
//	POST /console/held/<id>/discard         discard
//
// A write that write screening held is not a proposal, and these commands
// refuse it: screening holds are decided on the console, where the screens'
// findings sit beside the text. The terminal check is the courtesy seam approve
// extends, not the boundary (the console's owner-only auth is): it keeps an
// agent's shell from wandering into a prompt it cannot answer.

import (
	"bufio"
	"context"
	"encoding/json"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/config"
)

// reasonRestrictedAccount is the hold reason that makes a held write a
// restricted account's proposal (screening.ReasonRestrictedAccount; the
// predicate is screening.IsProposal). Transcribed rather than imported for the
// reason planWindows is (plan.go): internal/screening imports internal/store,
// which would pull the SQLite driver into a CLI that has none.
// TestProposals_TranscriptionsMatchTheDaemon pins it to the daemon's constant.
const reasonRestrictedAccount = "account.restricted"

// screeningOffError is the console's answer for the held queue while neither
// write screening is on nor restricted accounts are configured (the console's
// featureOffMessage for the screening feature), transcribed for the same
// reason and pinned by the same test.
const screeningOffError = "the screening feature is disabled"

// heldQueuePath is the console's held queue.
const heldQueuePath = "/console/held?format=json"

// heldActionPath is a held write's release or discard route.
func heldActionPath(id, action string) string {
	return "/console/held/" + url.PathEscape(id) + "/" + action + "?format=json"
}

// heldRow is one held write as the console's JSON names it. The CLI reads only
// these fields; --json prints the console's row objects untouched.
type heldRow struct {
	ID        string    `json:"id"`
	CreatedAt time.Time `json:"createdAt"`
	Status    string    `json:"status"`
	Project   string    `json:"project"`
	Name      string    `json:"name"`
	What      string    `json:"what"`
	Reasons   []struct {
		Code  string `json:"code"`
		Words string `json:"words"`
	} `json:"reasons"`
	Preview string `json:"preview"`
	Note    string `json:"note"`
	Stale   bool   `json:"stale"`
	Account string `json:"account"`
}

// isProposal reports whether the row's reasons make it a restricted account's
// proposal: screening.IsProposal on the console's reason codes.
func (h heldRow) isProposal() bool {
	for _, r := range h.Reasons {
		if r.Code == reasonRestrictedAccount {
			return true
		}
	}
	return false
}

// ref names the held item as project/name ("global/name" for the global scope).
func (h heldRow) ref() string {
	project := h.Project
	if project == "" {
		project = "global"
	}
	return printable(project) + "/" + printable(h.Name)
}

// proposer names who proposed it: "account <name>".
func (h heldRow) proposer() string {
	if h.Account == "" {
		return "a restricted account"
	}
	return "account " + printable(h.Account)
}

// heldQueue is the console's held queue: its pending writes and the recently
// decided ones, each kept both decoded and as the console sent it.
type heldQueue struct {
	Pending []json.RawMessage `json:"pending"`
	Recent  []json.RawMessage `json:"recent"`
}

// fetchHeld reads the held queue. A daemon that keeps no queue (screening off,
// no restricted accounts) is told apart from an unreachable one.
func fetchHeld(cfg config.Config) (heldQueue, error) {
	var q heldQueue
	if err := consoleJSON(cfg, heldQueuePath, &q); err != nil {
		if err.Error() == screeningOffError {
			return heldQueue{}, errors.New("this daemon keeps no proposals: it names no restricted accounts " +
				"(peer_auth.restricted), and write screening is off")
		}
		return heldQueue{}, err
	}
	return q, nil
}

// decodeRows decodes raw console rows, keeping each row's bytes beside it.
func decodeRows(raws []json.RawMessage) ([]heldRow, error) {
	rows := make([]heldRow, 0, len(raws))
	for _, raw := range raws {
		var h heldRow
		if err := json.Unmarshal(raw, &h); err != nil {
			return nil, fmt.Errorf("unreadable console response from %s: %w", heldQueuePath, err)
		}
		rows = append(rows, h)
	}
	return rows, nil
}

// --- proposals ---

var proposalsCmd = spec("proposals", groupRestricted,
	"list the restricted accounts' proposals waiting for you",
	noArgs().withHint("to decide one: seam promote <hold>, or seam proposals discard <hold>"),
	bindProposals, runProposals).withLong(
	`Lists the proposals waiting for the owner: a restricted local account's
(peer_auth.restricted) change to a constraint, convention, protocol or stage.
None of them is in any briefing, recall or gardener pass until promoted.

Each line shows the hold's id, what it changes, where, which account proposed
it and how long it has waited. Promote one with seam promote <hold>: it lands
exactly as the account wrote it. Discard one with seam proposals discard
<hold>: nothing lands, and its text is erased. The console's Held screen does
the same.

--json prints the console's own row for each proposal, as the daemon sent it,
in {"proposals": [...]}.`)

type proposalsOpts struct {
	project *string
	asJSON  *bool
}

func bindProposals(fs *flag.FlagSet) *proposalsOpts {
	return &proposalsOpts{
		project: fs.String("project", "", "only the proposals for project `SLUG`"),
		asJSON:  jsonFlag(fs, "print the console's row for each proposal as-is (for jq) instead of the text rendering"),
	}
}

func runProposals(_ context.Context, e *env, o *proposalsOpts, _ []string) error {
	cfg, err := e.loadConfig()
	if err != nil {
		return err
	}
	q, err := fetchHeld(cfg)
	if err != nil {
		return err
	}
	rows, err := decodeRows(q.Pending)
	if err != nil {
		return err
	}
	var shown []heldRow
	raws := []json.RawMessage{}
	for i, h := range rows {
		if !h.isProposal() || (*o.project != "" && h.Project != *o.project) {
			continue
		}
		shown = append(shown, h)
		raws = append(raws, q.Pending[i])
	}
	if *o.asJSON {
		b, err := json.MarshalIndent(struct {
			Proposals []json.RawMessage `json:"proposals"`
		}{raws}, "", "  ")
		if err != nil {
			return err
		}
		return writeRaw(e.stdout, b)
	}
	if len(shown) == 0 {
		fmt.Fprintln(e.stdout, "(no proposals waiting)")
		return nil
	}
	for _, h := range shown {
		line := fmt.Sprintf("  %s  %s %s  proposed by %s  %s", printable(h.ID), printable(h.What), h.ref(),
			h.proposer(), agoShort(h.CreatedAt))
		if h.Stale {
			line += "  (changed since: discard only)"
		}
		fmt.Fprintln(e.stdout, line)
	}
	return nil
}

// --- promote and discard ---

var promoteCmd = spec("promote", groupRestricted,
	"promote a proposal: it lands exactly as written (owner's terminal)",
	exactly(1, "hold"), bindNoOpts, runPromote).withLong(
	`Promotes a restricted account's proposal: it lands exactly as the account
wrote it -- a constraint, convention, protocol or stage that briefings then
inject -- and stays labeled as written by that account. The hold is its id
from seam proposals (or a unique prefix of it).

It shows the proposal, the text as an agent would read it, and asks to
confirm: y promotes, anything else changes nothing. A write held by write
screening is not a proposal and is refused here: decide it on the console's
Held screen. A proposal whose target changed after it was proposed can only be
discarded, since promoting it would undo that change.

It needs a terminal on stdin and stdout: an agent cannot use it.`)

var proposalsDiscardCmd = spec("proposals discard", groupRestricted,
	"discard a proposal: nothing lands (owner's terminal)",
	exactly(1, "hold"), bindNoOpts, runProposalsDiscard).withLong(
	`Discards a restricted account's proposal: nothing lands, and its text is
erased. The hold is its id from seam proposals (or a unique prefix of it). It
shows the proposal and asks to confirm: y discards, anything else changes
nothing. A write held by write screening is refused here: decide it on the
console's Held screen.

It needs a terminal on stdin and stdout: an agent cannot use it.`)

// ownerTerminalOnly is a proposal command run without the owner's terminal:
// seam approve's refusal, for this command.
func ownerTerminalOnly(name string) error {
	return fmt.Errorf("seam %s is the owner's own terminal path: it needs a terminal on stdin and stdout, "+
		"and an agent cannot use it\n%s(the owner decides proposals here, or on the console's Held screen)", name, hintIndent)
}

func runPromote(_ context.Context, e *env, _ *noOpts, pos []string) error {
	return decideProposal(e, "promote", pos[0])
}

func runProposalsDiscard(_ context.Context, e *env, _ *noOpts, pos []string) error {
	return decideProposal(e, "proposals discard", pos[0])
}

// decideProposal promotes (name "promote") or discards a pending proposal,
// after showing it and asking the owner.
func decideProposal(e *env, name, ref string) error {
	if !e.isTerminal() {
		return ownerTerminalOnly(name)
	}
	promote := name == "promote"
	cfg, err := e.loadConfig()
	if err != nil {
		return err
	}
	q, err := fetchHeld(cfg)
	if err != nil {
		return err
	}
	h, err := pendingProposal(q, ref)
	if err != nil {
		return err
	}
	if promote && h.Stale {
		return fmt.Errorf("%s changed after %s was proposed, so promoting it would undo that change: "+
			"discard it with seam proposals discard %s", h.ref(), printable(h.ID), printable(h.ID))
	}
	renderProposal(e, h)
	question := fmt.Sprintf("Promote %s? It lands exactly as written. [y/N] ", h.ref())
	if !promote {
		question = fmt.Sprintf("Discard %s? Nothing lands, and its text is erased. [y/N] ", h.ref())
	}
	answer, _ := prompt(e, bufio.NewReader(e.stdin), question)
	if a := strings.ToLower(answer); a != "y" && a != "yes" {
		fmt.Fprintln(e.stdout, "Nothing changed.")
		return nil
	}
	if !promote {
		if err := consolePOST(cfg, heldActionPath(h.ID, "discard"), nil); err != nil {
			return fmt.Errorf("the discard of %s did not complete: %w", printable(h.ID), err)
		}
		fmt.Fprintf(e.stdout, "Discarded %s: nothing landed, and its text is gone.\n", h.ref())
		return nil
	}
	if err := consolePOST(cfg, heldActionPath(h.ID, "release"), nil); err != nil {
		return fmt.Errorf("the promotion of %s did not complete: %w", printable(h.ID), err)
	}
	fmt.Fprintf(e.stdout, "Promoted %s: it landed as %s wrote it.\n", h.ref(), h.proposer())
	return nil
}

// pendingProposal finds the pending proposal ref names: its id, or a prefix
// of exactly one pending hold's id. A hold that is pending but not a proposal,
// or already decided, is refused with what to do instead.
func pendingProposal(q heldQueue, ref string) (heldRow, error) {
	pending, err := decodeRows(q.Pending)
	if err != nil {
		return heldRow{}, err
	}
	want := strings.ToUpper(strings.TrimSpace(ref))
	var hits []heldRow
	for _, h := range pending {
		if h.ID == want {
			hits = []heldRow{h}
			break
		}
		if want != "" && strings.HasPrefix(h.ID, want) {
			hits = append(hits, h)
		}
	}
	switch len(hits) {
	case 1:
		if !hits[0].isProposal() {
			return heldRow{}, fmt.Errorf("%s is a write held by write screening, not a restricted account's proposal: "+
				"decide it on the console's Held screen", printable(hits[0].ID))
		}
		return hits[0], nil
	case 0:
	default:
		return heldRow{}, fmt.Errorf("%q names %d held writes: give more of the id (seam proposals lists them)", ref, len(hits))
	}
	recent, err := decodeRows(q.Recent)
	if err != nil {
		return heldRow{}, err
	}
	for _, h := range recent {
		if h.ID == want {
			status := h.Status
			if status == "released" && h.isProposal() {
				status = "promoted"
			}
			return heldRow{}, fmt.Errorf("%s is already %s: nothing to decide", printable(h.ID), printable(status))
		}
	}
	return heldRow{}, fmt.Errorf("no proposal %q is waiting (seam proposals lists them)", ref)
}

// renderProposal prints what the owner decides on. Every value is the
// daemon's, and every value is made printable: the name and the text are the
// proposing account's to choose, and a terminal escape among them could repaint
// the lines above.
func renderProposal(e *env, h heldRow) {
	fmt.Fprintf(e.stdout, "\nProposal %s\n\n", printable(h.ID))
	fact := func(label, value string) {
		fmt.Fprintf(e.stdout, "  %-*s%s\n", factLabelWidth, label, value)
	}
	fact("What", printable(h.What))
	fact("Where", h.ref())
	fact("Proposed by", h.proposer())
	fact("Waiting", agoShort(h.CreatedAt))
	for i, r := range h.Reasons {
		label := ""
		if i == 0 {
			label = "Why it waits"
		}
		line := printable(r.Code)
		if r.Words != "" {
			line += ": " + printable(r.Words)
		}
		fact(label, line)
	}
	if h.Note != "" {
		fmt.Fprintf(e.stdout, "\n  %s\n", printable(h.Note))
	}
	if h.Preview != "" {
		fmt.Fprintln(e.stdout, "\n  The text, as an agent would read it (hidden characters spelled out, secrets redacted):")
		for line := range strings.Lines(h.Preview) {
			fmt.Fprintf(e.stdout, "    | %s\n", printable(strings.TrimRight(line, "\n")))
		}
	}
	fmt.Fprintln(e.stdout)
}
