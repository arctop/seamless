package core

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
)

const (
	stampLegal = "example-legal"
	stampFin   = "example-finance"
	stampOps   = "example-ops"
)

func TestMergeStamps_KeepsTheFirstAndNeverRemoves(t *testing.T) {
	have := []Stamp{{Project: stampLegal, Via: StampViaGrant, Ref: "01GRANTAAAA"}, {Project: stampOps, Via: StampViaMove, Ref: "01MOVEBBBB"}}
	add := []Stamp{
		{Project: stampLegal, Via: StampViaMove, Ref: "01OTHER"}, // already named: the first stays
		{Project: stampFin, Via: StampViaGrant, Ref: "01GRANTCCCC"},
		{Project: "example-desk", Via: StampViaGrant, Ref: ""}, // no reference
		{Project: "", Via: StampViaGrant, Ref: "01X"},          // no project
		{Project: "example-data", Via: StampViaGrant, Ref: "01OWN"},
		{Project: "example-audit", Via: StampViaSession, Ref: "01SESSION"},
	}
	got, added := MergeStamps(have, add, "example-data")
	require.True(t, added)
	require.Equal(t, []Stamp{
		have[0], have[1],
		{Project: "example-audit", Via: StampViaSession, Ref: "01SESSION"},
		{Project: stampFin, Via: StampViaGrant, Ref: "01GRANTCCCC"},
	}, got, "have keeps its order and entries (its own project's included); the new ones follow, sorted")

	same, added := MergeStamps(got, add, "example-data")
	require.False(t, added, "merging again adds nothing")
	require.Equal(t, got, same)

	none, added := MergeStamps(nil, nil, stampOps)
	require.False(t, added)
	require.Nil(t, none, "no stamps, no key: a write with isolation.requests off")
}

func TestStampsShown_DropsTheItemsOwnProject(t *testing.T) {
	stamps := []Stamp{{Project: stampLegal, Via: StampViaGrant, Ref: "01A"}, {Project: stampFin, Via: StampViaMove, Ref: "01B"}, {Ref: "01C"}}
	require.Equal(t, []Stamp{stamps[1]}, StampsShown(stamps, stampLegal),
		"an item moved into a project it was stamped for needs no warning about it there")
	require.Nil(t, StampsShown(nil, stampLegal))
}

func TestStamp_TextRenderings(t *testing.T) {
	grant := Stamp{Project: stampLegal, Via: StampViaGrant, Ref: "01M7999M7MBPEKDYS4P1XBJC4D"}
	move := Stamp{Project: stampFin, Via: StampViaMove, Ref: "01M79FJBCPRF2WWPHWE592Y3C1"}
	session := Stamp{Project: "example-vault", Via: StampViaSession, Ref: "01M7C6HQJW9BGFW332C57F3W2M"}
	unknown := Stamp{Project: "example-desk", Ref: "01M799APF1K5Z35H2K5Z0SAVJ8"}
	bare := Stamp{Project: "example-hr"}
	require.Equal(t, "R-JC4D", grant.ShortRef(), "a request's short form, as the Access requests page names it")
	require.Equal(t, "example-legal (grant R-JC4D)", grant.Label())
	require.Equal(t, "example-finance (move R-Y3C1)", move.Label())
	require.Equal(t, "C57F3W2M", session.ShortRef(), "a session by its id's last eight symbols, as the console names it")
	require.Equal(t, "example-vault (session C57F3W2M)", session.Label())
	require.Equal(t, "example-desk (approval R-AVJ8)", unknown.Label(), "a kind this release does not know")
	require.Equal(t, "example-hr", bare.Label(), "no reference: the project alone")
	require.Equal(t, "may contain content from example-legal (grant R-JC4D), example-finance (move R-Y3C1)",
		MayContainLine([]Stamp{grant, move}))
	require.Empty(t, MayContainLine(nil))
}

func TestStamp_JSONIsTheFrontmatterShape(t *testing.T) {
	stamps := []Stamp{
		{Project: stampLegal, Via: StampViaGrant, Ref: "01GRANT"},
		{Project: stampFin, Via: StampViaMove, Ref: "01MOVE"},
		{Project: "example-vault", Via: StampViaSession, Ref: "01SESSION"},
		{Project: "example-desk"},
	}
	b, err := json.Marshal(stamps)
	require.NoError(t, err)
	require.JSONEq(t, `[{"project":"example-legal","grant":"01GRANT"},{"project":"example-finance","move":"01MOVE"},`+
		`{"project":"example-vault","session":"01SESSION"},{"project":"example-desk"}]`, string(b))
	var back []Stamp
	require.NoError(t, json.Unmarshal(b, &back))
	require.Equal(t, stamps, back)

	// A memory with no stamp encodes exactly as before the field existed.
	plain, err := json.Marshal(Memory{ID: "01M", Name: "n"})
	require.NoError(t, err)
	require.NotContains(t, string(plain), "mayContain")
}
