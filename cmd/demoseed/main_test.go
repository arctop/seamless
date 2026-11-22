package main

import (
	"io"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
)

// TestFxScaleNeedsReview pins the flag rules: -scale grows the review fixture
// and is refused on any other mode, and the rules from before -scale still
// hold word for word.
func TestFxScaleNeedsReview(t *testing.T) {
	for _, tc := range []struct {
		args    string
		refusal string // "" when the flags are accepted
	}{
		{"-review -scale -data d", ""},
		{"-review -data d", ""},
		{"-data d", ""},
		{"-diary -data d", ""},
		{"-scenes -repo r -race -data d", ""},
		{"-scale -data d", "-scale is only valid with -review"},
		{"-scale -diary -data d", "-scale is only valid with -review"},
		{"-scale -scenes -data d", "-scale is only valid with -review"},
		{"-review -scale -diary -data d", "-review takes none of -diary, -scenes, -repo and -race"},
		{"-review -scale", "-data is required"},
		{"-diary -race -data d", "-diary takes none of -scenes, -repo and -race"},
	} {
		t.Run(tc.args, func(t *testing.T) {
			o, err := parseFlags(strings.Fields(tc.args), io.Discard)
			require.NoError(t, err)
			err = o.check()
			if tc.refusal == "" {
				require.NoError(t, err)
				return
			}
			require.EqualError(t, err, tc.refusal)
		})
	}
}

// TestFxScaleFlagUsage pins that -scale is a flag of its own, said to need
// -review, and that an unknown flag is a parse error.
func TestFxScaleFlagUsage(t *testing.T) {
	var out strings.Builder
	_, err := parseFlags([]string{"-h"}, &out)
	require.Error(t, err)
	require.Contains(t, out.String(), "-scale")
	require.Contains(t, out.String(), "with -review: grow the review fixture to the owner's scale")
	_, err = parseFlags([]string{"-nope"}, io.Discard)
	require.Error(t, err)
}
