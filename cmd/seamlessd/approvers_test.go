package main

import (
	"bytes"
	"context"
	"encoding/json"
	"path/filepath"
	"regexp"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/config"
	"github.com/arctop/seamless/internal/store"
	"github.com/arctop/seamless/internal/store/storetest"
)

var codeRe = regexp.MustCompile(`[0-9A-HJKMNP-TV-Z]{4}(-[0-9A-HJKMNP-TV-Z]{4}){3}`)

func TestApprovers_Verbs(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	now := time.Date(2026, 11, 6, 12, 0, 0, 0, time.UTC)
	cfg := config.Defaults()
	cfg.Addr = "127.0.0.1:9123"
	cfg.PeerAuth.Locked = []string{"example-finance"}
	run := func(args ...string) (string, error) {
		var out bytes.Buffer
		err := approvers(ctx, db, cfg, &out, args, now)
		return out.String(), err
	}

	out, err := run("list")
	require.NoError(t, err)
	require.Contains(t, out, "No approver credentials are enrolled")

	out, err = run("enroll-code", "passkey")
	require.NoError(t, err)
	code := codeRe.FindString(out)
	require.NotEmpty(t, code, out)
	require.Contains(t, out, "http://localhost:9123/console/approvers", "the passkey page on the relying party's origin, never 127.0.0.1")
	require.NoError(t, store.CheckEnrolCode(ctx, db, store.ApproverPasskey, code, now))

	out, err = run("enroll-code", "touchid")
	require.NoError(t, err)
	require.Contains(t, out, "seam touchid enroll")

	for _, bad := range [][]string{{"enroll-code"}, {"enroll-code", "password"}, {"enroll-code", "passkey", "x"},
		{"remove"}, {"reset"}, {"reset", "--force"}, {"list", "x"}, {"frobnicate"}} {
		_, err := run(bad...)
		require.Error(t, err, "%v", bad)
	}

	cred, err := store.EnrolApprover(ctx, db, store.ApproverCredential{
		Kind: store.ApproverPasskey, CredentialID: []byte{1, 2, 3}, PublicKey: []byte{9}, Alg: -7, Name: "laptop",
	}, code, "", now)
	require.NoError(t, err)

	out, err = run("list")
	require.NoError(t, err)
	require.Contains(t, out, cred.ID)
	require.Contains(t, out, `"laptop"`)
	require.Contains(t, out, "an enrolment code")

	out, err = run("list", "--json")
	require.NoError(t, err)
	var got struct {
		Approvers []approverJSON `json:"approvers"`
	}
	require.NoError(t, json.Unmarshal([]byte(out), &got))
	require.Len(t, got.Approvers, 1)
	require.Equal(t, "010203", got.Approvers[0].CredentialID)

	out, err = run("remove", cred.ID)
	require.NoError(t, err)
	require.Contains(t, out, "Only the terminal approval path")
	_, err = run("remove", cred.ID)
	require.ErrorIs(t, err, store.ErrApproverNotFound)

	out, err = run("reset", "--yes")
	require.NoError(t, err)
	require.Contains(t, out, "Removed 0 credential(s)")
}

// doctor names the enrolled approvers while a project is locked, and warns
// when there are none.
func TestApproverCheck(t *testing.T) {
	ctx := context.Background()
	db, err := storetest.OpenFresh(t, filepath.Join(t.TempDir(), "seam.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	c := approverCheck(ctx, db)
	require.Equal(t, statusWarn, c.status)
	require.Contains(t, c.detail, "only the terminal path")

	now := time.Now()
	code, _, err := store.MintEnrolCode(ctx, db, store.ApproverPasskey, now)
	require.NoError(t, err)
	_, err = store.EnrolApprover(ctx, db, store.ApproverCredential{
		Kind: store.ApproverPasskey, CredentialID: []byte{1}, PublicKey: []byte{9}, Alg: -7, Name: "key",
	}, code, "", now)
	require.NoError(t, err)
	c = approverCheck(ctx, db)
	require.Equal(t, statusOK, c.status)
	require.Contains(t, c.detail, "passkeys 1")
	require.Equal(t, statusInfo, approverCheck(ctx, nil).status)
}

// console-open signs in on the relying party's origin while a project is
// locked, and on the server URL exactly as before otherwise.
func TestConsoleBase(t *testing.T) {
	cfg := config.Defaults()
	cfg.Addr = "127.0.0.1:9123"
	require.Equal(t, cfg.ServerURL(), consoleBase(cfg), "peer_auth off: unchanged")
	cfg.PeerAuth.Locked = []string{"example-finance"}
	require.Equal(t, cfg.ServerURL(), consoleBase(cfg), "a locked list while disabled changes nothing")
	cfg.PeerAuth.Enabled = true
	require.Equal(t, "http://localhost:9123", consoleBase(cfg))
}
