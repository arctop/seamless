package main

// seam sessions -- session list/detail via the console JSON endpoint, the
// flagged sessions (--flagged), a flagged session's transcript copy
// (--transcript), streamed from the console's route: under peer_auth the
// owner cannot read the daemon's data dir, so the daemon's API is the one way
// to it -- and keeping one by hand (--keep-transcript) when its end signal
// never sent it.

import (
	"cmp"
	"context"
	"errors"
	"flag"
	"fmt"
	"io"
	"net/http"
	"net/url"
	"os"
	"strings"
	"time"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/core"
	"github.com/arctop/seamless/internal/harvest"
)

// The list and the detail view are one command, which is what `between` exists
// for: `seam sessions` lists, `seam sessions <id>` shows one.
var sessionsCmd = spec("sessions", groupObservability, "list sessions, or show one by id",
	between(0, 1, "id"), bindSessions, runSessions).
	withLong(`--flagged lists the flagged sessions of all time (--verdict, one verdict's):
each with its verdict and reason, the transcript file the agent client wrote and
whether it is still there, and whether Seamless keeps a copy of it.

--transcript with an id writes that flagged session's transcript copy to stdout,
decompressed: the JSONL the agent client wrote. It is read through the daemon,
by the owner only (never through a relay or a paired device).

--keep-transcript with an id keeps that flagged session's transcript now, by
hand, when its end signal never sent it (a crash, a closed terminal): the daemon
copies the file itself where it may read it; under peer_auth, with server_url
naming the socket, this account reads the path the session's hooks recorded --
only a regular *.jsonl file inside the agent client's own transcript folder, on
this computer -- and sends it. The owner's alone, like --transcript.`)

type sessionsOpts struct {
	status     *string
	flagged    *bool
	verdict    *string
	transcript *bool
	keep       *bool
	asJSON     *bool
}

func bindSessions(fs *flag.FlagSet) *sessionsOpts {
	return &sessionsOpts{
		// Derived from core.SessionStatuses, and the derivation is load-bearing: the
		// string this replaced advertised "active|completed", omitting the "expired"
		// the console has always accepted. Transcribing the help text would have
		// shipped that omission as an enforced rule.
		status:  enumFlag(fs, "status", "", "filter by `STATUS`", enumOf(core.SessionStatuses)),
		flagged: fs.Bool("flagged", false, "list only flagged sessions, of all time, with their transcript copies"),
		verdict: enumFlag(fs, "verdict", "", "list only the sessions flagged with `VERDICT` (implies --flagged)",
			enumOf(core.FlagVerdicts)),
		transcript: fs.Bool("transcript", false, "with an id: write the flagged session's transcript copy (JSONL) to stdout"),
		keep:       fs.Bool("keep-transcript", false, "with an id: keep the flagged session's transcript now, by hand"),
		asJSON:     jsonFlag(fs, jsonUsage),
	}
}

// sessionRow is one row of the console's session list, as this command reads it.
type sessionRow struct {
	ID             string    `json:"id"`
	Name           string    `json:"name"`
	Project        string    `json:"project"`
	Status         string    `json:"status"`
	Ambient        bool      `json:"ambient"`
	Host           string    `json:"host"`
	Findings       string    `json:"findings"`
	Updated        time.Time `json:"updated"`
	Verdict        string    `json:"verdict"`
	Reason         string    `json:"reason"`
	Copy           string    `json:"transcriptCopy"`
	Kept           bool      `json:"transcriptKept"`
	TranscriptPath string    `json:"transcriptPath"`
}

func runSessions(ctx context.Context, e *env, o *sessionsOpts, pos []string) error {
	flagged := *o.flagged || *o.verdict != ""
	switch {
	case *o.transcript && len(pos) == 0:
		return errors.New("--transcript writes one session's copy: name the session by its id")
	case *o.keep && len(pos) == 0:
		return errors.New("--keep-transcript keeps one session's transcript: name the session by its id")
	case *o.transcript && *o.keep:
		return errors.New("--transcript and --keep-transcript are two steps: keep the transcript first, then read it")
	case *o.transcript && (*o.asJSON || flagged || *o.status != ""):
		return errors.New("--transcript writes the JSONL itself: it takes an id and no other flag")
	case *o.keep && (*o.asJSON || flagged || *o.status != ""):
		return errors.New("--keep-transcript takes an id and no other flag")
	case len(pos) > 0 && (flagged || *o.status != ""):
		return errors.New("--status, --flagged and --verdict filter the list: drop them to show one session")
	}
	cfg, err := e.loadConfig()
	if err != nil {
		return err
	}
	if len(pos) > 0 {
		if *o.transcript {
			return sessionTranscript(e.stdout, cfg, pos[0])
		}
		if *o.keep {
			return keepTranscript(ctx, e.stdout, cfg, pos[0], nil)
		}
		if *o.asJSON {
			return printConsoleJSON(e, cfg, sessionDetailPath(pos[0]))
		}
		return sessionDetail(e.stdout, cfg, pos[0])
	}

	var data struct {
		Total    int          `json:"total"`
		Active   int          `json:"active"`
		Sessions []sessionRow `json:"sessions"`
		// Locked names the locked projects whose sessions the console left
		// out (peer_auth; absent otherwise).
		Locked []string `json:"locked"`
	}
	path := "/console/sessions?format=json"
	if *o.status != "" {
		path += "&status=" + url.QueryEscape(*o.status)
	}
	if flagged {
		// Flagged sessions of all time: a flag is for coming back to.
		path += "&w=all&verdict=" + url.QueryEscape(cmp.Or(*o.verdict, "flagged"))
	}
	if *o.asJSON {
		return printConsoleJSON(e, cfg, path)
	}
	if err := consoleJSON(cfg, path, &data); err != nil {
		return err
	}
	if flagged {
		printFlagged(e.stdout, data.Sessions)
	} else {
		fmt.Fprintf(e.stdout, "%d sessions (%d active)\n", data.Total, data.Active)
		for _, s := range data.Sessions {
			amb := ""
			if s.Ambient {
				amb = " (ambient)"
			}
			if s.Verdict != "" {
				amb += " [flagged " + s.Verdict + "]"
			}
			fmt.Fprintf(e.stdout, "  %-20s %-10s %-9s %s%s\n", sessionName(s.Name, s.ID), orDash(s.Project), s.Status, agoShort(s.Updated), amb)
		}
	}
	if line := lockedSkipped(data.Locked, ""); line != "" {
		fmt.Fprintln(e.stdout, line)
	}
	return nil
}

// sessionName is a session's name, or its short id when it has none.
func sessionName(name, id string) string {
	if name == "" {
		return shortID(id)
	}
	return name
}

// printFlagged prints the flagged sessions, one block each.
func printFlagged(out io.Writer, rows []sessionRow) {
	noun := "sessions"
	if len(rows) == 1 {
		noun = "session"
	}
	fmt.Fprintf(out, "%d flagged %s\n", len(rows), noun)
	for _, s := range rows {
		fmt.Fprintf(out, "\n%s  [%s]  %s  %s  %s\n", sessionName(s.Name, s.ID), s.Verdict, orDash(s.Project), s.Status, agoShort(s.Updated))
		fmt.Fprintf(out, "  id: %s\n", s.ID)
		if s.Reason != "" {
			fmt.Fprintf(out, "  reason: %s\n", s.Reason)
		}
		fmt.Fprintf(out, "  transcript: %s\n", transcriptFileLine(s.TranscriptPath, s.Host))
		fmt.Fprintf(out, "  copy: %s\n", copyLine(s.Copy, s.Kept, s.ID))
	}
}

// transcriptFileLine is the transcript file the agent client wrote and whether
// it is still there -- looked for on this computer, as the owner, which the
// daemon under peer_auth never does. A session recorded on another computer is
// not looked for here.
func transcriptFileLine(path, host string) string {
	if path == "" {
		return "none recorded (no hook named one)"
	}
	if host = strings.ToLower(strings.TrimSpace(host)); host != "" && host != config.Hostname() {
		return path + " (on " + host + "; not looked for from here)"
	}
	switch _, err := os.Stat(path); {
	case err == nil:
		return path + " (still there)"
	case errors.Is(err, os.ErrNotExist):
		return path + " (gone)"
	default:
		return path + " (not readable from here: " + err.Error() + ")"
	}
}

// copyLine is the copy's state, and how to read it when one is kept.
func copyLine(state string, kept bool, id string) string {
	if kept {
		return "kept; read it with `seam sessions " + id + " --transcript`"
	}
	if state == string(core.TranscriptCopied) {
		return "none on this server (the record names one made elsewhere, or before a restore)"
	}
	return core.TranscriptCopy(state).Explain()
}

// sessionDetailPath is the console route for one session. The id is spliced in
// as typed, exactly as the detail view always has: --json reads the same route.
func sessionDetailPath(id string) string {
	return "/console/sessions/" + id + "?format=json"
}

func sessionDetail(out io.Writer, cfg config.Config, id string) error {
	var d struct {
		Session struct {
			ID          string `json:"id"`
			Name        string `json:"name"`
			ProjectSlug string `json:"projectSlug"`
			Status      string `json:"status"`
			Model       string `json:"model"`
			Account     string `json:"account"`
			Host        string `json:"host"`
		} `json:"session"`
		Findings  string `json:"findings"`
		ToolCalls int    `json:"toolCalls"`
		Reads     int    `json:"memoryReads"`
		Writes    int    `json:"memoryWrites"`
		Injected  int    `json:"injectedItems"`
		ReadBack  int    `json:"readAfterInject"`
		Flag      *struct {
			Verdict        string `json:"verdict"`
			Reason         string `json:"reason"`
			TranscriptPath string `json:"transcriptPath"`
			Copy           string `json:"copy"`
			Kept           bool   `json:"kept"`
		} `json:"flag"`
	}
	if err := consoleJSON(cfg, sessionDetailPath(id), &d); err != nil {
		return err
	}
	fmt.Fprintf(out, "%s  [%s]  %s\n", sessionName(d.Session.Name, d.Session.ID), d.Session.Status, orDash(d.Session.ProjectSlug))
	if d.Session.Model != "" {
		fmt.Fprintf(out, "model: %s\n", d.Session.Model)
	}
	if by := writtenBy(d.Session.Account); by != "" {
		fmt.Fprintln(out, by)
	}
	if f := d.Flag; f != nil {
		line := "flagged: " + f.Verdict
		if f.Reason != "" {
			line += " -- " + f.Reason
		}
		fmt.Fprintln(out, line)
		fmt.Fprintf(out, "transcript: %s\n", transcriptFileLine(f.TranscriptPath, d.Session.Host))
		fmt.Fprintf(out, "copy: %s\n", copyLine(f.Copy, f.Kept, d.Session.ID))
	}
	fmt.Fprintf(out, "tool calls: %d  writes: %d  reads: %d  read-after-inject: %d/%d\n",
		d.ToolCalls, d.Writes, d.Reads, d.ReadBack, d.Injected)
	if strings.TrimSpace(d.Findings) != "" {
		fmt.Fprintf(out, "\nfindings:\n%s\n", d.Findings)
	}
	return nil
}

// transcriptErrorTrailer is the console's TranscriptErrorTrailer (this binary
// does not import the console): the trailer a copy's stream sets when the copy
// could not be read whole.
const transcriptErrorTrailer = "X-Seamless-Transcript-Error"

// sessionTranscript writes session id's transcript copy to out as the daemon
// streams it. A copy the daemon could not read whole is an error, after the
// part that arrived: never a cut-off conversation passed off as the whole one.
func sessionTranscript(out io.Writer, cfg config.Config, id string) error {
	req, err := http.NewRequest(http.MethodGet, cfg.ServerURL()+"/console/sessions/"+url.PathEscape(id)+"/transcript", nil)
	if err != nil {
		return err
	}
	req.Header.Set("Authorization", "Bearer "+cfg.MCP.APIKey)
	req.Header.Set("Accept", "application/json")
	// No timeout: a copy runs to hundreds of megabytes.
	client, err := cfg.HTTPClient(0)
	if err != nil {
		return err
	}
	resp, err := client.Do(req)
	if err != nil {
		return fmt.Errorf("console unreachable at %s: %w", cfg.DaemonAddress(), err)
	}
	defer func() { _ = resp.Body.Close() }()
	if resp.StatusCode != http.StatusOK {
		return errors.New(consoleError(resp))
	}
	if _, err := io.Copy(out, resp.Body); err != nil {
		return fmt.Errorf("transcript of %s cut off: %w", id, err)
	}
	if msg := resp.Trailer.Get(transcriptErrorTrailer); msg != "" {
		return fmt.Errorf("transcript of %s: %s", id, msg)
	}
	return nil
}

// keepTranscript keeps flagged session id's transcript by hand, through the
// owner's route (harvest.TranscriptKeepPath). Off the peer_auth socket the
// daemon reads the file itself, where it may. On the socket the daemon names
// the path its hooks recorded, and this account -- the owner's, on this
// computer -- reads it, only inside its agent client's own transcript folder
// (openNamedTranscript), and sends it. newClient is nil for cfg.HTTPClient(0).
func keepTranscript(ctx context.Context, out io.Writer, cfg config.Config, id string, newClient func(config.Config) (*http.Client, error)) error {
	if newClient == nil {
		newClient = func(cfg config.Config) (*http.Client, error) { return cfg.HTTPClient(0) }
	}
	// No timeout of its own beyond the upload's: a copy runs to hundreds of
	// megabytes.
	ctx, cancel := context.WithTimeout(ctx, harvest.TranscriptUploadTimeout)
	defer cancel()
	client, err := newClient(cfg)
	if err != nil {
		return err
	}
	path := harvest.TranscriptKeepPath(id)
	kept := func() error {
		fmt.Fprintf(out, "copy kept; read it with `seam sessions %s --transcript`\n", id)
		return nil
	}
	if !pushesCaptures(cfg) {
		status, answer, err := postTranscript(ctx, client, cfg, path+"?"+harvest.TranscriptFromDaemonParam+"=1", nil, 0)
		if err != nil {
			return fmt.Errorf("daemon unreachable at %s: %w", cfg.DaemonAddress(), err)
		}
		if status != http.StatusOK {
			return errors.New(answer.Error)
		}
		return kept()
	}
	status, answer, err := postTranscript(ctx, client, cfg, path, nil, 0)
	if err != nil {
		return fmt.Errorf("daemon unreachable at %s: %w", cfg.DaemonAddress(), err)
	}
	if status != http.StatusOK || answer.Send == nil {
		return errors.New(cmp.Or(answer.Error, "the daemon named no transcript to send"))
	}
	send := answer.Send
	// An unknown hostname here proves nothing either way; the path check
	// below still holds the file to this account's own transcript folder.
	if host, here := strings.ToLower(strings.TrimSpace(send.Host)), config.Hostname(); host != "" && here != "" && host != here {
		return fmt.Errorf("session %s ran on %s: its transcript is there, not on this computer", id, host)
	}
	roots := transcriptRoots(send.Client)
	f, size, refusal := openNamedTranscript(send.Path, roots)
	switch refusal {
	case harvest.TranscriptRefusedMissing:
		return fmt.Errorf("not sent: %s is not there, or not readable from this account", send.Path)
	case harvest.TranscriptRefusedPath:
		return fmt.Errorf("not sent: %s is not a regular *.jsonl file inside %s (%s's own transcript folder for this account)",
			send.Path, cmp.Or(strings.Join(roots, ", "), "no folder"), cmp.Or(send.Client, "its agent client"))
	}
	defer func() { _ = f.Close() }()
	status, answer, err = postTranscript(ctx, client, cfg, path, f, size)
	if err != nil {
		return fmt.Errorf("transcript of %s not sent: %w", id, err)
	}
	if status != http.StatusOK {
		return fmt.Errorf("transcript of %s not kept: %s", id, answer.Error)
	}
	return kept()
}
