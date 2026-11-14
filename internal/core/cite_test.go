package core

import (
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	p7CiteA = "01M7998NT13A8CHQSE5XCS05WR"
	p7CiteB = "01M799BPDZVAYJ7690PCRWRQBQ"
)

func TestCitations_TheFixedForm(t *testing.T) {
	cases := []struct {
		name string
		body string
		want []Citation
	}{
		{"one", "see example-legal:" + p7CiteA + " for the terms",
			[]Citation{{"example-legal", p7CiteA}}},
		{"start and end of the body", "example-legal:" + p7CiteA,
			[]Citation{{"example-legal", p7CiteA}}},
		{"punctuation around it", "(example-legal:" + p7CiteA + "), and `example-ops:" + p7CiteB + "`.",
			[]Citation{{"example-legal", p7CiteA}, {"example-ops", p7CiteB}}},
		{"a lower-case id is read canonically", "example-legal:" + strings.ToLower(p7CiteA),
			[]Citation{{"example-legal", p7CiteA}}},
		{"distinct, first appearance first", "example-ops:" + p7CiteB + " example-legal:" + p7CiteA + " example-ops:" + p7CiteB,
			[]Citation{{"example-ops", p7CiteB}, {"example-legal", p7CiteA}}},
		{"one id under two projects is two references", "example-legal:" + p7CiteA + " example-ops:" + p7CiteA,
			[]Citation{{"example-legal", p7CiteA}, {"example-ops", p7CiteA}}},
		{"slug punctuation cannot open a token", "-example-legal:" + p7CiteA,
			[]Citation{{"example-legal", p7CiteA}}},
		{"a slug with dots and underscores", "team_a.example:" + p7CiteA,
			[]Citation{{"team_a.example", p7CiteA}}},
		{"a longer token is not an id", "example-legal:" + p7CiteA + "X", nil},
		{"a shorter one neither", "example-legal:" + p7CiteA[:25] + " ", nil},
		{"not a ULID (I is not Crockford)", "example-legal:01M7998NT13A8CHQSE5XCS05WI", nil},
		{"an overflowing ULID", "example-legal:81M7998NT13A8CHQSE5XCS05WR", nil},
		{"the global scope is no project", "global:" + p7CiteA + " _global:" + p7CiteB, nil},
		{"no slug before the colon", " :" + p7CiteA + " -:" + p7CiteB, nil},
		{"a space between slug and colon", "example-legal :" + p7CiteA, nil},
		{"a URL port is no reference", "https://example.com:8443/x", nil},
		{"a wiki link is another form", "[[example-legal]] " + p7CiteA, nil},
	}
	for _, tc := range cases {
		t.Run(tc.name, func(t *testing.T) {
			require.Equal(t, tc.want, Citations(tc.body))
		})
	}
}

func TestCitations_BoundedPerBody(t *testing.T) {
	var b strings.Builder
	for i := range MaxCitations + 10 {
		b.WriteString("example-p")
		b.WriteString(strings.Repeat("x", i%7))
		b.WriteString(string(rune('a' + i%26)))
		b.WriteString(":" + p7CiteA + "\n")
	}
	require.Len(t, Citations(b.String()), MaxCitations)
}

func TestCitation_StringAndForm(t *testing.T) {
	c := Citation{Project: "example-legal", ID: p7CiteA}
	require.Equal(t, "example-legal:"+p7CiteA, c.String())
	require.Equal(t, []Citation{c}, Citations(c.String()), "String writes what Citations reads")
	require.Equal(t, "example-legal:<id>", CitationForm("example-legal"))
}
