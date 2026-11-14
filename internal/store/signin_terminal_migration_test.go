package store

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"

	"github.com/arctop/seamless/internal/core"
)

// Migration 044 is additive: two columns on console_signins with a default.
// The previous release names its columns in every statement over the table,
// so its inserts still work and take the default, and its reads -- 041's
// columns -- read a migrated database as before.
func TestMigration044_PreviousReleaseStillWorks(t *testing.T) {
	ctx := context.Background()
	db := leaseDB(t)
	defaults := map[string]string{}
	rows, err := db.QueryContext(ctx, `SELECT name, "notnull", COALESCE(dflt_value, 'NULL') FROM pragma_table_info('console_signins')`)
	require.NoError(t, err)
	defer func() { _ = rows.Close() }()
	for rows.Next() {
		var name, dflt string
		var notNull int
		require.NoError(t, rows.Scan(&name, &notNull, &dflt))
		if notNull == 1 {
			defaults[name] = dflt
		}
	}
	require.NoError(t, rows.Err())
	require.Equal(t, "''", defaults["remote_addr"], "remote_addr is NOT NULL DEFAULT ''")
	require.Equal(t, "''", defaults["user_agent"], "user_agent is NOT NULL DEFAULT ''")

	at, until := core.FormatTime(apNow), core.FormatTime(apNow.Add(time.Hour))
	_, err = db.ExecContext(ctx, `INSERT INTO console_signins (id, token_hash, cookie_hash, credential, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?)`, "01OLDSIGNIN", HashConsoleSignIn("cookie-old", "key-old"), hashSignInCookie("cookie-old"),
		"01CRED", at, until)
	require.NoError(t, err, "the previous release's passkey sign-in")
	_, err = db.ExecContext(ctx, `INSERT INTO console_signins (id, token_hash, cookie_hash, credential, created_at, expires_at)
		VALUES (?, ?, ?, ?, ?, ?)`, "01OLDWAITING", HashConsoleSignIn("cookie-wait", "key-wait"), hashSignInCookie("cookie-wait"),
		terminalSignInWaiting+terminalSignInCodeHash("ABCD-EFGH"), at, at)
	require.NoError(t, err, "the previous release's terminal sign-in")
	for _, id := range []string{"01OLDSIGNIN", "01OLDWAITING"} {
		var addr, agent string
		require.NoError(t, db.QueryRowContext(ctx, `SELECT remote_addr, user_agent FROM console_signins WHERE id = ?`, id).Scan(&addr, &agent))
		require.Empty(t, addr, id)
		require.Empty(t, agent, id)
	}

	s, err := LiveConsoleSignIn(ctx, db, HashConsoleSignIn("cookie-old", "key-old"), apNow)
	require.NoError(t, err, "041's columns read a migrated database as before")
	require.Equal(t, "01OLDSIGNIN", s.ID)
	_, state, err := TerminalSignInFor(ctx, db, "01OLDWAITING", HashConsoleSignIn("cookie-wait", "key-wait"), apNow)
	require.NoError(t, err)
	require.Equal(t, TerminalSignInWaiting, state)
	n, err := EndConsoleSignInByCookie(ctx, db, "cookie-wait", apNow)
	require.NoError(t, err)
	require.Equal(t, int64(1), n, "a sign-out ends the previous release's waiting row as before")
}
