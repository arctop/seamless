package main

import (
	"context"
	"net/http"
	"reflect"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/approval"
)

// Moves and read-and-write grants (isolation.requests) at the terminal: seam
// access request --write, and seam approve showing the kind, the carry and the
// access choice, and committing the owner's choice.

func TestAccessShapes_TheCarryMatchesTheDaemon(t *testing.T) {
	require.Equal(t, jsonKeys(reflect.TypeFor[approval.CarryFacts]()), jsonKeys(reflect.TypeFor[accessCarry]()))
}

func TestAccessRequest_WriteAsksForAMove(t *testing.T) {
	ac := newAccessConsole(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"01KACCESSREQUEST0000007F3K","short_id":"R-7F3K","project":"example-hr",` +
			`"kind":"move","from":"example-ops","page":"","filed":true,"reason":"none"}`))
	})
	e, out, errb := ac.env()
	require.Equal(t, 0, dispatch(context.Background(), e, []string{"access", "request", "--write", "example-hr"}), errb.String())
	require.Equal(t, "Requested a move into example-hr from example-ops (R-7F3K). The owner approves it with: seam approve R-7F3K\n"+
		"Record what belongs in example-ops now: once the move is approved, this agent cannot write there.\n", out.String())
	seen := ac.requests()
	require.Len(t, seen, 1)
	require.Equal(t, []string{"true"}, seen[0].form["write"])
}

func TestAccessRequest_WriteAsksForAReadAndWriteGrant(t *testing.T) {
	ac := newAccessConsole(t, func(w http.ResponseWriter, r *http.Request) {
		_, _ = w.Write([]byte(`{"id":"01KACCESSREQUEST0000007F3K","short_id":"R-7F3K","project":"example-payroll",` +
			`"kind":"grant","from":"example-hr","access":"write","page":"","filed":true,"reason":"none"}`))
	})
	e, out, errb := ac.env()
	require.Equal(t, 0, dispatch(context.Background(), e, []string{"access", "request", "--write", "example-payroll"}), errb.String())
	require.Equal(t, "Requested read and write access to example-payroll for example-hr (R-7F3K; the owner may approve it read-only). "+
		"The owner approves it with: seam approve R-7F3K\n", out.String())

	// Without --write the form carries no write field: the request is what it
	// always was.
	e, _, errb = ac.env()
	require.Equal(t, 0, dispatch(context.Background(), e, []string{"access", "request", "example-payroll"}), errb.String())
	seen := ac.requests()
	_, has := seen[len(seen)-1].form["write"]
	require.False(t, has)
}

// moveFacts is a move request as a daemon under peer_auth shows it.
func moveFacts() accessFacts {
	f := factsOf(reqA, "R-7F3K", "example-hr", accessWarning{Code: "move", Text: "This agent has worked in example-ops. " +
		"If you approve, it works in example-hr from now on, and what it saw may be written into example-hr. " +
		"It can no longer write to example-ops or the global scope until the agent process exits."})
	f.Kind, f.From = "move", "example-ops"
	f.Carry = &accessCarry{Home: "example-ops", Holds: []string{"example-hr"}, WriteTo: []string{"example-hr"}}
	return f
}

// A move no lock covers asks no duration: it is for good, and the commit
// names the default duration, which the proof signs.
func TestApprove_AMoveAsksNoDuration(t *testing.T) {
	f := moveFacts()
	rig := newApproveRig(t, routes(t, byRef(t, f)), "y\n")
	require.Equal(t, 0, rig.run("R-7F3K"), rig.errb.String())
	out := rig.out.String()
	require.Contains(t, out, "\nMove request R-7F3K\n\n")
	require.Contains(t, out, "  ! This agent has worked in example-ops. If you approve, it works in example-hr from now on")
	require.Contains(t, out, "  Leaves          example-ops\n")
	require.Contains(t, out, "  Holds           example-hr (once approved)\n")
	require.Contains(t, out, "  May write to    example-hr (once approved)\n")
	require.Contains(t, out, "  Kind            a move into a fenced project, for good\n")
	require.NotContains(t, out, "For how long?")
	require.Contains(t, out, "If approved: Claude Code 2.1.319 (pid 4242) moves from example-ops into example-hr for good.\n")
	require.Equal(t, approveArgvFor(reqA, "session", f.Digest), rig.runner.recorded()[len(rig.runner.recorded())-1])
}

// A move into a locked project asks the duration of its lock.
func TestApprove_ALockedMoveAsksItsLocksDuration(t *testing.T) {
	f := moveFacts()
	f.Locked = true
	rig := newApproveRig(t, routes(t, byRef(t, f)), "1h\ny\n")
	require.Equal(t, 0, rig.run("R-7F3K"), rig.errb.String())
	out := rig.out.String()
	require.Contains(t, out, "  Project         example-hr (confidential, locked, read-write)\n")
	require.Contains(t, out, "  Lock            approving also opens the lock to this agent's process tree, for the duration chosen\n")
	require.Contains(t, out, "For how long?")
	require.Contains(t, out, "If approved: Claude Code 2.1.319 (pid 4242) moves from example-ops into example-hr for good, "+
		"and its lock opens to it for 1 hour, until "+clockTime(fixedNow.Add(time.Hour))+".\n")
	require.Equal(t, approveArgvFor(reqA, "1h", f.Digest), rig.runner.recorded()[len(rig.runner.recorded())-1])
}

// writeGrantFacts is a grant between confidential projects that asked for
// read and write, as a daemon under peer_auth shows it.
func writeGrantFacts() accessFacts {
	f := factsOf(reqA, "R-7F3K", "example-payroll",
		accessWarning{Code: "copy_into", Text: "content from example-payroll may be copied into example-hr"})
	f.Kind, f.Access, f.From = "grant", "read-only", "example-hr"
	f.Carry = &accessCarry{Home: "example-hr", Holds: []string{"example-hr", "example-payroll"}, WriteTo: []string{"example-hr"}}
	f.AccessChoices, f.AskedAccess = []string{"read-only", "read-write"}, "read-write"
	f.ReadWrite = "content from example-payroll may be copied into example-hr, and it may change example-payroll's tasks, " +
		"memories and notes; content from example-hr may be copied into example-payroll"
	return f
}

func TestApprove_AGrantAsksTheAccess(t *testing.T) {
	f := writeGrantFacts()
	rig := newApproveRig(t, routes(t, byRef(t, f)), "read-w\n4h\ny\n")
	require.Equal(t, 0, rig.run("R-7F3K"), rig.errb.String())
	out := rig.out.String()
	require.Contains(t, out, "  Holds           example-hr and example-payroll (once approved)\n")
	require.Contains(t, out, "  May write to    example-hr (once approved, read-only)\n")
	require.Contains(t, out, "  It asks for     read and write\n")
	require.Contains(t, out, "  Kind            a grant of a confidential project: read-only, or read and write as you choose\n")
	require.Contains(t, out, "Which access?\n"+
		"  read-only   content from example-payroll may be copied into example-hr (the default)\n"+
		"  read-write  "+f.ReadWrite+"\n"+
		"Access [read-only, read-write; Enter for read-only; deny to deny it]: ")
	require.Contains(t, out, "If approved: Claude Code 2.1.319 (pid 4242), working in example-hr, may read and write example-payroll for 4 hours")
	want := append(approveArgvFor(reqA, "4h", f.Digest), "--access", "read-write")
	require.Equal(t, want, rig.runner.recorded()[len(rig.runner.recorded())-1])
}

// Enter takes read-only, the default, and the commit names no access: every
// seamlessd takes it.
func TestApprove_AGrantDefaultsToReadOnly(t *testing.T) {
	f := writeGrantFacts()
	rig := newApproveRig(t, routes(t, byRef(t, f)), "\n1h\ny\n")
	require.Equal(t, 0, rig.run("R-7F3K"), rig.errb.String())
	require.Contains(t, rig.out.String(), "may read example-payroll (read-only) for 1 hour")
	require.Equal(t, approveArgvFor(reqA, "1h", f.Digest), rig.runner.recorded()[len(rig.runner.recorded())-1])
}

func TestApprove_AccessFlag(t *testing.T) {
	f := writeGrantFacts()
	rig := newApproveRig(t, routes(t, byRef(t, f)), "y\n")
	require.Equal(t, 0, rig.run("--access", "read-write", "--for", "1h", "R-7F3K"), rig.errb.String())
	require.NotContains(t, rig.out.String(), "Which access?")
	require.Equal(t, append(approveArgvFor(reqA, "1h", f.Digest), "--access", "read-write"), rig.runner.recorded()[len(rig.runner.recorded())-1])

	rig = newApproveRig(t, routes(t, byRef(t, f)), "")
	require.Equal(t, 1, rig.run("--access", "write", "R-7F3K"))
	require.Contains(t, rig.errb.String(), `invalid value "write" for --access: valid values for R-7F3K are read-only, read-write`)

	lock := factsOf(reqA, "R-7F3K", "example-finance")
	rig = newApproveRig(t, routes(t, byRef(t, lock)), "")
	require.Equal(t, 1, rig.run("--access", "read-only", "R-7F3K"))
	require.Contains(t, rig.errb.String(), `valid values for R-7F3K are read-write`, "a lock's request offers its own access alone")
}

// Without peer_auth the owner's access rides the console's form.
func TestApprove_AdvisoryReadAndWriteGrant(t *testing.T) {
	f := writeGrantFacts()
	f.Program, f.Agent, f.PID, f.UID = "", "", 0, 0
	table := byRef(t, f, advisoryOne)
	table["POST /console/access/"+reqA+"/approve"] = `{"approved":"R-7F3K","project":"example-payroll","from":"example-hr",` +
		`"grant":"01KGRANT","access":"write","expires_at":"` + fixedNow.Add(time.Hour).Format(time.RFC3339) + `"}`
	rig := newApproveRig(t, routes(t, table), "read-write\n1h\ny\n")
	require.Equal(t, 0, rig.run("R-7F3K"), rig.errb.String())
	require.Contains(t, rig.out.String(), "Approved R-7F3K: sessions working in example-hr may read and write example-payroll until "+
		clockTime(fixedNow.Add(time.Hour))+".\n")
	var form map[string][]string
	for _, r := range rig.ac.requests() {
		if r.method == http.MethodPost {
			form = r.form
		}
	}
	require.Equal(t, []string{"read-write"}, form["access"])
}

func TestApprove_AdvisoryMove(t *testing.T) {
	f := moveFacts()
	f.Program, f.Agent, f.PID, f.UID = "", "", 0, 0
	table := byRef(t, f, advisoryOne)
	table["POST /console/access/"+reqA+"/approve"] = `{"approved":"R-7F3K","kind":"move","project":"example-hr",` +
		`"from":"example-ops","session":"cc/quiet-otter@example-hr"}`
	rig := newApproveRig(t, routes(t, table), "y\n")
	require.Equal(t, 0, rig.run("R-7F3K"), rig.errb.String())
	require.Contains(t, rig.out.String(), "Approved R-7F3K: the agent works in example-hr from now on, as session cc/quiet-otter@example-hr.\n")
	var form map[string][]string
	for _, r := range rig.ac.requests() {
		if r.method == http.MethodPost {
			form = r.form
		}
	}
	require.Equal(t, []string{"session"}, form["duration"])
	_, has := form["access"]
	require.False(t, has)
}

// --passkey opens the page with the access chosen, where it is not the
// default.
func TestApprove_PasskeyPageCarriesTheAccess(t *testing.T) {
	f := writeGrantFacts()
	rig := newApproveRig(t, routes(t, byRef(t, f)), "")
	require.Equal(t, 0, rig.run("--passkey", "--for", "1h", "--access", "read-write", "R-7F3K"), rig.errb.String())
	calls := rig.runner.recorded()
	require.Equal(t, []string{"open", "https://seamless.example.org/console/access/" + reqA + "?access=read-write&for=1h"}, calls[len(calls)-1])
}

func TestDecidable_ADeniedMove(t *testing.T) {
	f := moveFacts()
	rig := newApproveRig(t, routes(t, byRef(t, f, func(o *accessOne) { o.Status = statusDenied })), "")
	require.Equal(t, 1, rig.run("R-7F3K"))
	require.Contains(t, rig.errb.String(), "R-7F3K was denied: nothing to decide (the agent stays in example-ops until it asks again)")
}
