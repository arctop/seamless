package main

// seam project isolation -- the owner's CLI half of the per-project isolation
// fence, backed by the same console route the Overview control posts to.
//
// The asymmetry is the console's, mirrored rather than reinvented: loosening is
// immediate (nothing leaked while the fence was up, so the change is fully
// reversible), while tightening changes what every OTHER agent can see and
// detaches the project from its families and parent, so it prints exactly what it
// would change and applies nothing without --yes. --yes is the console confirm
// panel's confirm=1, so there is one round trip either way and the SERVER -- not
// this client -- decides which direction the request was.

import (
	"context"
	"errors"
	"flag"
	"fmt"
	"net/url"
	"slices"
	"strings"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
)

var projectIsolationCmd = spec("project isolation", groupObservability,
	"show or set a project's isolation fence (open|confidential|sealed)",
	between(1, 2, "slug", "state").
		withHint("state is one of open, confidential, sealed; omit it to show the current state"),
	bindProjectIsolation, runProjectIsolation).
	withLong(`Isolation fences agent-to-agent leakage, not the owner -- the console still
sees everything.

  open          shares normally (the default)
  confidential  nothing leaves: agents in other projects never read this one,
                and agents bound here cannot write outside it
  sealed        nothing in or out: agents here see only this project

Isolation requires a standalone project, so tightening detaches this one from
its families and parent, and is refused while it still has children. It prints
those consequences and applies nothing until you pass --yes. Loosening is
immediate.`)

type projectIsolationOpts struct {
	yes    *bool
	asJSON *bool
}

func bindProjectIsolation(fs *flag.FlagSet) *projectIsolationOpts {
	return &projectIsolationOpts{
		yes:    fs.Bool("yes", false, "apply a tighten without the confirmation step (a loosen needs none)"),
		asJSON: jsonFlag(fs, jsonUsage+"; the exit status is unchanged"),
	}
}

// isolationStates is the state vocabulary, derived from core.Isolations rather
// than transcribed. The console validates the same argument through
// validate.Isolation, which reads the same canonical slice, so the client-side
// check cannot reject a state the route would have accepted.
var isolationStates = enumOf(core.Isolations)

// parseIsolation validates the optional state positional. The spec DSL constrains
// positional COUNT only, so the enum is checked here -- and before any config load
// or request, so a typo costs no round trip and never reaches the route as a
// parameter. The wording matches validate.Isolation's, which stays the authority.
func parseIsolation(s string) (string, error) {
	if !slices.Contains(isolationStates, s) {
		return "", fmt.Errorf("invalid isolation %q: valid values are %s", s, strings.Join(isolationStates, ", "))
	}
	return s, nil
}

func runProjectIsolation(_ context.Context, e *env, o *projectIsolationOpts, pos []string) error {
	slug := pos[0]
	var state string
	if len(pos) == 2 {
		s, err := parseIsolation(pos[1])
		if err != nil {
			return err
		}
		state = s
	}
	cfg, err := e.loadConfig()
	if err != nil {
		return err
	}
	if state == "" {
		return showProjectIsolation(e, cfg, slug, *o.asJSON)
	}
	return setProjectIsolation(e, cfg, slug, state, *o.yes, *o.asJSON)
}

// cliProjectSummary is the slice of the console's project summary JSON this
// command reads: the thin GET already carries the fence and its promise, so
// showing the state costs no POST and cannot change anything.
type cliProjectSummary struct {
	Isolation        string `json:"isolation"`
	IsolationPromise string `json:"isolationPromise"`
}

// showProjectIsolation prints the state a project holds now, and its promise.
// With asJSON it prints the route's summary as-is, after the same check: an
// answer with no isolation state is not seamlessd's, in either rendering.
func showProjectIsolation(e *env, cfg config.Config, slug string, asJSON bool) error {
	path := "/console/projects/" + url.PathEscape(slug) + "?format=json"
	var p cliProjectSummary
	var raw []byte
	if asJSON {
		body, err := consoleRaw(cfg, path)
		if err != nil {
			return err
		}
		if err := decodeConsole(path, body, &p); err != nil {
			return err
		}
		raw = body
	} else if err := consoleJSON(cfg, path, &p); err != nil {
		return err
	}
	if p.Isolation == "" {
		// The console fills this in for every project ("" reads as open on its side,
		// never on the wire), so an empty state means something other than seamlessd
		// answered. Printing "isolation: " would be a confident blank.
		return fmt.Errorf("unreadable project summary for %s: no isolation state", slug)
	}
	if raw != nil {
		return writeRaw(e.stdout, raw)
	}
	fmt.Fprintf(e.stdout, "%s  isolation: %s\n", slug, p.Isolation)
	printIsolationPromise(e, p.IsolationPromise)
	return nil
}

// cliIsolationEffects mirrors the store's tighten report as it arrives on the
// wire. Only the topology a tighten gives up is rendered here: from/to are
// already the response's state/requested, and children arrive exclusively on the
// 409 that consolePOST turns into this command's error.
type cliIsolationEffects struct {
	Families []string `json:"families"`
	Parent   string   `json:"parent"`
}

// cliIsolationResult mirrors the console's isolationResponse. Applied and
// ConfirmRequired -- not the status code -- say which outcome happened; State is
// always what the project holds NOW, so it doubles as the read path afterwards.
type cliIsolationResult struct {
	Project          string               `json:"project"`
	Requested        string               `json:"requested"`
	State            string               `json:"state"`
	Promise          string               `json:"promise"`
	RequestedPromise string               `json:"requestedPromise"`
	Applied          bool                 `json:"applied"`
	ConfirmRequired  bool                 `json:"confirmRequired"`
	GlobalMemories   int                  `json:"globalMemories"`
	Filed            int                  `json:"filed"`
	Effects          *cliIsolationEffects `json:"effects"`
}

// setProjectIsolation posts the requested state and renders whichever outcome
// came back.
//
// Each parameter goes in the query string exactly once. The route reads r.Form,
// which concatenates the query and the body, so a value supplied in both places
// is two values and a deliberate 400 -- which is also why consolePOST's bodyless
// request needs no special-casing here.
//
// With asJSON the response is printed as-is in place of the text, and the
// outcome still decides the exit status: an unconfirmed tighten exits 1 in
// either rendering, so a script reading JSON cannot mistake it for a fence that
// went up either.
func setProjectIsolation(e *env, cfg config.Config, slug, state string, yes, asJSON bool) error {
	q := url.Values{"format": {"json"}, "state": {state}}
	if yes {
		q.Set("confirm", "1")
	}
	path := "/console/projects/" + url.PathEscape(slug) + "/isolation?" + q.Encode()
	var res cliIsolationResult
	var raw []byte
	if asJSON {
		body, err := consolePOSTRaw(cfg, path)
		if err != nil {
			return err
		}
		if err := decodeConsole(path, body, &res); err != nil {
			return err
		}
		raw = body
	} else if err := consolePOST(cfg, path, &res); err != nil {
		// A refusal arrives here as the handler's own message -- the blocking
		// children and the remedy -- because every 409 on this route carries `error`.
		return err
	}
	switch {
	case raw != nil && res.Applied:
		return writeRaw(e.stdout, raw)
	case raw != nil && res.ConfirmRequired:
		if err := writeRaw(e.stdout, raw); err != nil {
			return err
		}
		return errNotApplied
	case res.Applied:
		fmt.Fprintf(e.stdout, "%s  isolation set to %s\n", slug, res.State)
		printIsolationPromise(e, res.Promise)
		printIsolationDetach(e, res.Effects, true)
		printIsolationFiled(e, res.Filed)
		return nil
	case res.ConfirmRequired:
		fmt.Fprintf(e.stdout, "%s  isolation: %s -> %s (not applied)\n", slug, res.State, res.Requested)
		printIsolationPromise(e, res.RequestedPromise)
		printIsolationDetach(e, res.Effects, false)
		printIsolationProvenance(e, res.GlobalMemories)
		// Those lines are the product, not a log of this error: they are what the
		// caller asked to see, and the error is the verdict that nothing happened
		// (the `plan check` / `status` precedent). Exiting 0 here would let a script
		// read an unconfirmed tighten as a fence that went up.
		return errNotApplied
	default:
		// The two outcomes above are exhaustive on a 200, and a refusal is the 409
		// handled above. Anything else is something other than seamlessd answering;
		// rendering it as a success would be the plausible dummy the no-fake-results
		// rule exists to prevent.
		return fmt.Errorf("unreadable isolation response for %s: neither applied nor awaiting confirmation", slug)
	}
}

// errNotApplied is an unconfirmed tighten's result, in both renderings.
var errNotApplied = errors.New("nothing applied -- re-run with --yes to apply this tighten")

// printIsolationPromise echoes the promise sentence verbatim. The console owns
// that copy -- one sentence per state, quoted from one place so the surfaces
// cannot drift -- and transcribing it here is how the CLI would end up promising
// something the fence does not do.
func printIsolationPromise(e *env, promise string) {
	if promise == "" {
		return
	}
	fmt.Fprintf(e.stdout, "  %s\n", promise)
}

// printIsolationDetach renders the topology a tighten gives up, in the tense of
// the outcome: what it WOULD detach while it has not applied, what it DID once it
// has. Effects is absent whenever there is nothing to say (a loosen, or a project
// that was already standalone).
func printIsolationDetach(e *env, eff *cliIsolationEffects, applied bool) {
	if eff == nil {
		return
	}
	leaves, detaches := "leaves family", "detaches from parent"
	if applied {
		leaves, detaches = "left family", "detached from parent"
	}
	if len(eff.Families) > 0 {
		fmt.Fprintf(e.stdout, "  %s %s\n", leaves, strings.Join(eff.Families, ", "))
	}
	if eff.Parent != "" {
		fmt.Fprintf(e.stdout, "  %s %s\n", detaches, eff.Parent)
	}
}

// printIsolationProvenance renders the confirm panel's provenance consequence:
// how many global memories originated in this project's sessions, and therefore
// how many relocation proposals the tighten will file.
//
// -1 means the audit is not built yet; 0 means it found nothing. Both print
// nothing, because silence is the only honest rendering of "not computed" and a
// known zero has nothing to report. What must never happen is -1 reaching a
// count, which would read as a fact about the project.
func printIsolationProvenance(e *env, n int) {
	if n <= 0 {
		return
	}
	fmt.Fprintf(e.stdout, "  %d global memories originated in this project's sessions; "+
		"relocation proposals will be filed in the Gardener inbox\n", n)
}

// printIsolationFiled reports what the apply actually did, as opposed to what
// the confirm step promised it would do. The two are separate lines on purpose:
// printIsolationProvenance is a forecast the owner is being asked to accept,
// this is the receipt. Zero prints nothing -- a tighten that found no leaked
// knowledge has nothing to report, and saying "filed 0" would invite the reader
// to wonder what failed.
func printIsolationFiled(e *env, n int) {
	if n <= 0 {
		return
	}
	noun := "relocation proposals"
	if n == 1 {
		noun = "relocation proposal"
	}
	fmt.Fprintf(e.stdout, "  filed %d %s in the Gardener inbox\n", n, noun)
}
