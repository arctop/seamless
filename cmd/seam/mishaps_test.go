package main

import (
	"testing"

	"github.com/stretchr/testify/require"
)

// fxMishaps is the console's mishap list: two typed reports (one naming a
// memory), an old untyped one, and a memory gone from the index.
const fxMishaps = `{"window":"all","windowLabel":"all time","total":3,"shown":3,"types":[` +
	`{"type":"shell-trap","meaning":"m","count":2},{"type":"wrong-target","meaning":"m","count":0},` +
	`{"type":"live-state","meaning":"m","count":0},{"type":"ignored-warning","meaning":"m","count":0},` +
	`{"type":"fabricated","meaning":"m","count":0},{"type":"other","meaning":"m","count":0},` +
	`{"type":"untyped","meaning":"m","count":1}],"mishaps":[` +
	`{"id":"01KMMMMMMMMMMMMMMMMMMMMMM1","ts":"2999-01-01T00:00:00Z","type":"shell-trap","text":"a bare == in zsh died",` +
	`"description":"shell-trap: a bare == in zsh died","project":"example-project","sessionId":"01KHHHHHHHHHHHHHHHHHHHHHHH","session":"cc/quiet-otter"},` +
	`{"id":"01KMMMMMMMMMMMMMMMMMMMMMM2","ts":"2999-01-01T00:00:00Z","type":"shell-trap","text":"zsh-equals-expansion bit again",` +
	`"description":"shell-trap: zsh-equals-expansion bit again","memories":[{"id":"01KAAAAAAAAAAAAAAAAAAAAAAA","name":"zsh-equals-expansion"}]},` +
	`{"id":"01KMMMMMMMMMMMMMMMMMMMMMM3","ts":"0001-01-01T00:00:00Z","text":"pkill matched the live daemon","description":"pkill matched the live daemon","project":"example-project"}` +
	`],"memories":[{"id":"01KAAAAAAAAAAAAAAAAAAAAAAA","name":"zsh-equals-expansion","count":2},` +
	`{"id":"01KBBBBBBBBBBBBBBBBBBBBBBB","missing":true,"count":1}]}`

// fxMishapsTrap is the list filtered to one type, cut at its first report.
const fxMishapsTrap = `{"type":"shell-trap","days":3,"windowLabel":"last 3 days","total":3,"shown":2,"types":[` +
	`{"type":"shell-trap","count":2},{"type":"untyped","count":1}],"mishaps":[` +
	`{"id":"01KMMMMMMMMMMMMMMMMMMMMMM1","ts":"2999-01-01T00:00:00Z","type":"shell-trap","text":"a bare == in zsh died","project":"example-project"}` +
	`],"memories":[],"locked":["acme-finance"]}`

func mishapsFixture(t *testing.T) *daemonFixture {
	return newDaemonFixture(t, nil, map[string]string{
		"GET /console/mishaps?format=json":                                                fxMishaps,
		"GET /console/mishaps?days=3&format=json&project=example-project&type=shell-trap": fxMishapsTrap,
	}, fxHealthz)
}

func TestMishaps_TableByTypeThenTheReports(t *testing.T) {
	fx := mishapsFixture(t)
	out, errb, code := fx.run(t, "mishaps")
	require.Equal(t, 0, code, errb)
	require.Equal(t, "3 mishaps reported (all time)\n"+
		"  shell-trap       2\n"+
		"  wrong-target     0\n"+
		"  live-state       0\n"+
		"  ignored-warning  0\n"+
		"  fabricated       0\n"+
		"  other            0\n"+
		"  untyped          1\n"+
		"\n"+
		"memories named:\n"+
		"  zsh-equals-expansion             2\n"+
		"  01KBBBBB (no longer in the index) 1\n"+
		"\n"+
		"  now   shell-trap       example-project a bare == in zsh died  (cc/quiet-otter)\n"+
		"  now   shell-trap       global         zsh-equals-expansion bit again\n"+
		"  -     untyped          example-project pkill matched the live daemon\n", out)
}

// The flags compose the console's query; a type filter titles the reports,
// a cut list says so, and a locked project the answer left out is named.
func TestMishaps_FiltersReachTheConsole(t *testing.T) {
	fx := mishapsFixture(t)
	out, errb, code := fx.run(t, "mishaps", "--type", "shell-trap", "--project", "example-project", "--days", "3")
	require.Equal(t, 0, code, errb)
	require.Contains(t, out, "3 mishaps reported (last 3 days)\n")
	require.Contains(t, out, "\n2 shell-trap reports:\n")
	require.Contains(t, out, "  ... the newest 1 of 2\n")
	require.NotContains(t, out, "memories named:", "no memory named, no block")
	require.NotContains(t, out, "acme-finance", "the filter's own project is not locked")
}

// --json prints the console's answer byte for byte.
func TestMishaps_JSONIsTheConsoleAnswer(t *testing.T) {
	fx := mishapsFixture(t)
	out, errb, code := fx.run(t, "mishaps", "--json")
	require.Equal(t, 0, code, errb)
	require.Equal(t, fxMishaps+"\n", out)
	require.Contains(t, synopsis(mishapsCmd), "[--json]")
}

// A type the vocabulary lacks and a days count that is not positive are
// refused at parse time, naming what they take.
func TestMishapsParse_RefusesBadFlags(t *testing.T) {
	_, err := parse(commands(), []string{"mishaps", "--type", "shell_trap"})
	require.ErrorContains(t, err, "valid values are shell-trap, wrong-target, live-state, ignored-warning, fabricated, other, untyped")
	_, err = parse(commands(), []string{"mishaps", "--days", "0"})
	require.ErrorContains(t, err, "must be a positive integer")
	_, err = parse(commands(), []string{"mishaps", "extra"})
	require.ErrorContains(t, err, "takes no positional arguments")

	p, err := parse(commands(), []string{"mishaps", "--type", "untyped"})
	require.NoError(t, err)
	require.Equal(t, "untyped", *p.opts.(*mishapsOpts).typ)
}

// seam mishaps sits with the other observability commands.
func TestMishaps_InTheObservabilityGroup(t *testing.T) {
	require.Equal(t, groupObservability, mishapsCmd.group)
	require.Contains(t, helpText(), "seam mishaps [--days N] [--json] [--project SLUG] [--type TYPE]")
}
