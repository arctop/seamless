package main

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPctOf(t *testing.T) {
	require.Equal(t, 0, pctOf(0, 0))
	require.Equal(t, 0, pctOf(1, 0))
	require.Equal(t, 50, pctOf(1, 2))
	require.Equal(t, 100, pctOf(3, 3))
	require.Equal(t, 33, pctOf(1, 3))
}

func TestShortIDAndDash(t *testing.T) {
	require.Equal(t, "01KX7R4H", shortID("01KX7R4H6HZEAK1MX7ARTQVFNJ"))
	require.Equal(t, "short", shortID("short"))
	require.Equal(t, "-", orDash(""))
	require.Equal(t, "seamless", orDash("seamless"))
}

func TestAgoShort(t *testing.T) {
	require.Equal(t, "-", agoShort(time.Time{}))
	require.Equal(t, "now", agoShort(time.Now()))
	require.Equal(t, "2h", agoShort(time.Now().Add(-2*time.Hour)))
	require.Equal(t, "3d", agoShort(time.Now().Add(-72*time.Hour)))
}

// A read answer's provenance stamps render as the console words them, and an
// item with none renders nothing (printField skips it).
func TestMayContain_RendersTheStamps(t *testing.T) {
	require.Equal(t, "may contain content from example-legal (grant R-JC4D), example-finance (move R-Y3C1)",
		mayContain(map[string]any{"may_contain": []any{
			map[string]any{"project": "example-legal", "grant": "01M7999M7MBPEKDYS4P1XBJC4D"},
			map[string]any{"project": "example-finance", "move": "01M79FJBCPRF2WWPHWE592Y3C1"},
		}}))
	require.Equal(t, "may contain content from example-legal (grant R-JC4D), 1 project(s) this connection may not name",
		mayContain(map[string]any{"may_contain": []any{map[string]any{"project": "example-legal", "grant": "01M7999M7MBPEKDYS4P1XBJC4D"}},
			"may_contain_withheld": float64(1)}))
	require.Empty(t, mayContain(map[string]any{"id": "01X"}))
}
