package store

import (
	"context"
	"database/sql"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// enrollcapAdd stores a fresh passkey named name as added from addr, one
// second after the one before it (*at), so the rows' order is their adding's.
// A passkey never waits for room: every add succeeds.
func enrollcapAdd(t *testing.T, db *sql.DB, at *time.Time, name, addr string) (PasskeyEnrollment, string) {
	t.Helper()
	*at = at.Add(time.Second)
	e, code, err := BeginPasskeyEnrollment(context.Background(), db, signinCred(name), addr, PasskeyEnrollmentTTL, *at)
	require.NoError(t, err, "%s from %q never waits for room", name, addr)
	return e, code
}

// enrollcapWaiting counts the passkeys waiting, from source (peerSource) or,
// with source "", from every address but loopback.
func enrollcapWaiting(t *testing.T, db *sql.DB, source string) int {
	t.Helper()
	rows, err := db.Query(`SELECT remote_addr FROM passkey_enrollments WHERE status = ?`, EnrollmentPending)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	n := 0
	for rows.Next() {
		var addr string
		require.NoError(t, rows.Scan(&addr))
		s := peerSource(addr)
		if s == source || (source == "" && s != peerSourceLoopback) {
			n++
		}
	}
	require.NoError(t, rows.Err())
	return n
}

// enrollcapWaits reports whether e still waits.
func enrollcapWaits(t *testing.T, db *sql.DB, e PasskeyEnrollment) bool {
	t.Helper()
	got, err := PasskeyEnrollmentByID(context.Background(), db, e.ID)
	if err != nil {
		require.ErrorIs(t, err, ErrEnrollmentNotFound)
		return false
	}
	return got.Status == EnrollmentPending
}

// One address holds at most maxPendingEnrollmentsFrom, and a passkey added
// past that displaces the address's oldest, whose code then names nothing.
// Someone looping on the page from one host, however long, never displaces
// the owner's passkey added from another address, which the owner's code
// still approves. The control is a loop from the owner's own address -- a
// proxy in front of the console makes every request one -- which does.
func TestPasskeyEnrollment_PerAddressDisplacesItsOwn(t *testing.T) {
	db := leaseDB(t)
	ctx := context.Background()
	at := apNow
	owners, ownersCode := enrollcapAdd(t, db, &at, "owner", "198.51.100.7")
	first, firstCode := enrollcapAdd(t, db, &at, "loop-first", "192.0.2.1")
	for i := range maxPendingEnrollmentsFrom {
		enrollcapAdd(t, db, &at, fmt.Sprintf("loop-%d", i), "192.0.2.1")
	}
	require.False(t, enrollcapWaits(t, db, first), "the address's oldest made room")
	_, _, err := ApprovePasskeyEnrollment(ctx, db, firstCode, at)
	require.ErrorIs(t, err, ErrEnrollmentNotFound, "a displaced passkey's code names nothing")
	require.Equal(t, maxPendingEnrollmentsFrom, enrollcapWaiting(t, db, "192.0.2.1"))

	for i := range 3 * maxPendingEnrollments {
		enrollcapAdd(t, db, &at, fmt.Sprintf("loop-more-%d", i), "192.0.2.1")
	}
	require.True(t, enrollcapWaits(t, db, owners), "a loop from another address never displaces the owner's passkey")
	require.Equal(t, maxPendingEnrollmentsFrom, enrollcapWaiting(t, db, "192.0.2.1"))
	cred, _, err := ApprovePasskeyEnrollment(ctx, db, ownersCode, at)
	require.NoError(t, err)
	require.Equal(t, owners.Credential.Name, cred.Name)

	// The control: from the owner's own address the loop does displace it.
	owners, _ = enrollcapAdd(t, db, &at, "owner-again", "198.51.100.7")
	for i := range maxPendingEnrollmentsFrom {
		enrollcapAdd(t, db, &at, fmt.Sprintf("same-address-%d", i), "198.51.100.7")
	}
	require.False(t, enrollcapWaits(t, db, owners), "a loop from the same address crowds it out")
}

// Past maxPendingEnrollments from every address but loopback, a passkey from
// another host displaces the oldest of the address holding the most, never
// one from an address holding fewer, and never one from loopback, which
// keeps its own share however many hosts loop: eight IPv4 addresses, or eight
// /64s of one /56, at their share each, fill the cap.
func TestPasskeyEnrollment_HostsThatFillTheCapNeverReachLoopback(t *testing.T) {
	for _, hosts := range signcapOtherHosts {
		t.Run(hosts.name, func(t *testing.T) {
			db := leaseDB(t)
			at := apNow
			var server []PasskeyEnrollment
			for i, addr := range []string{"127.0.0.1", "::1", "127.0.0.2", "::ffff:127.0.0.1"} {
				e, _ := enrollcapAdd(t, db, &at, fmt.Sprintf("server-%d", i), addr)
				server = append(server, e)
			}
			owners, _ := enrollcapAdd(t, db, &at, "owner's phone", "203.0.113.9")
			var oldest PasskeyEnrollment
			for i := range maxPendingEnrollments - 1 {
				e, _ := enrollcapAdd(t, db, &at, fmt.Sprintf("host-%d", i), hosts.addr(i/maxPendingEnrollmentsFrom))
				if i == 0 {
					oldest = e
				}
			}
			require.Equal(t, maxPendingEnrollments, enrollcapWaiting(t, db, ""))

			// One more from a host of its own: the fullest host with the
			// oldest passkey gives one up.
			enrollcapAdd(t, db, &at, "one-more", "198.51.100.200")
			require.False(t, enrollcapWaits(t, db, oldest))
			require.Equal(t, maxPendingEnrollments, enrollcapWaiting(t, db, ""), "the cap holds")

			// The loop goes on across its hosts.
			for round := range 3 {
				for h := range maxPendingEnrollments / maxPendingEnrollmentsFrom {
					enrollcapAdd(t, db, &at, fmt.Sprintf("round-%d-%d", round, h), hosts.addr(h))
				}
			}
			require.True(t, enrollcapWaits(t, db, owners), "hosts looping at their share never reach a host holding fewer")
			for _, e := range server {
				require.True(t, enrollcapWaits(t, db, e), "hosts never reach loopback's share: %s", e.Credential.Name)
			}
			require.Equal(t, maxPendingEnrollments, enrollcapWaiting(t, db, ""))
			require.Equal(t, maxPendingEnrollmentsFrom, enrollcapWaiting(t, db, peerSourceLoopback))

			// Loopback is held to its own share: a fifth from there displaces
			// loopback's oldest, not another host's.
			enrollcapAdd(t, db, &at, "server-more", "::1")
			require.False(t, enrollcapWaits(t, db, server[0]))
			require.Equal(t, maxPendingEnrollmentsFrom, enrollcapWaiting(t, db, peerSourceLoopback))
			require.Equal(t, maxPendingEnrollments, enrollcapWaiting(t, db, ""))
		})
	}
}

// The address is kept as printable ASCII and read back with the row; no
// address recorded, one that does not parse, and the text of loopback's own
// key are each held to the other hosts' cap, as one address of their own.
func TestPasskeyEnrollment_AddressKeptAndCounted(t *testing.T) {
	db := leaseDB(t)
	at := apNow
	e, _ := enrollcapAdd(t, db, &at, "phone", "  192.0.2.44\x1b]0;owned\x07 ")
	require.Equal(t, "192.0.2.44?]0;owned?", e.From)
	got, err := PasskeyEnrollmentByID(context.Background(), db, e.ID)
	require.NoError(t, err)
	require.Equal(t, e.From, got.From)

	for _, addr := range []string{"", "loopback", "localhost"} {
		var first PasskeyEnrollment
		for i := range maxPendingEnrollmentsFrom + 1 {
			e, _ := enrollcapAdd(t, db, &at, fmt.Sprintf("%q-%d", addr, i), addr)
			if i == 0 {
				first = e
			}
		}
		require.False(t, enrollcapWaits(t, db, first), "%q is one address, held to its share", addr)
		require.Equal(t, maxPendingEnrollmentsFrom, enrollcapWaiting(t, db, peerSource(addr)), "%q", addr)
	}
	require.Zero(t, enrollcapWaiting(t, db, peerSourceLoopback), "none of them is loopback")
}

// Migration 049 is additive: one column on passkey_enrollments with a
// default. The previous release names its columns in every statement over the
// table, so its insert still works and takes the default, and the row it adds
// reads back with no address, counted as one address of its own.
func TestMigration049_PreviousReleaseStillWorks(t *testing.T) {
	ctx := context.Background()
	db := leaseDB(t)
	var dflt string
	var notNull int
	require.NoError(t, db.QueryRowContext(ctx, `SELECT "notnull", COALESCE(dflt_value, 'NULL') FROM pragma_table_info('passkey_enrollments')
		WHERE name = 'remote_addr'`).Scan(&notNull, &dflt))
	require.Equal(t, 1, notNull)
	require.Equal(t, "''", dflt, "remote_addr is NOT NULL DEFAULT ''")

	c := signinCred("previous")
	at := core.FormatTime(apNow)
	_, err := db.ExecContext(ctx, `INSERT INTO passkey_enrollments (id, status, code_hash, name, credential_id, public_key,
		alg, sign_count, backup_eligible, backup_state, aaguid, transports, attachment, attestation_format, user_handle, rp_id,
		created_at, pending_until) VALUES (?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?, ?)`,
		"01OLDENROLL", EnrollmentPending, enrollmentCodeHash("ABCD-EFGH"), c.Name, c.CredentialID, c.PublicKey, c.Alg, 0, 1, 1,
		c.AAGUID, "internal", c.Attachment, c.AttestationFormat, c.UserHandle, c.RPID, at,
		core.FormatTime(apNow.Add(PasskeyEnrollmentTTL)))
	require.NoError(t, err, "the previous release's insert")
	e, err := PasskeyEnrollmentByCode(ctx, db, "ABCD-EFGH", apNow)
	require.NoError(t, err)
	require.Equal(t, "01OLDENROLL", e.ID)
	require.Empty(t, e.From)
	require.Equal(t, 1, enrollcapWaiting(t, db, peerSource("")))
	cred, _, err := ApprovePasskeyEnrollment(ctx, db, "ABCD-EFGH", apNow)
	require.NoError(t, err, "the previous release's row is approved as before")
	require.Equal(t, c.Name, cred.Name)
}
