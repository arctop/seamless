-- What survives a daemon restart under a running agent (peer_auth), in two parts.
--
-- 1. mcp_sessions: what an MCP client declared at initialize. A `seam mcp-proxy`
-- bridge outlives a restart (an automatic update performs one) and keeps
-- presenting the Mcp-Session-Id it was given, which the daemon accepts by shape.
-- The initialize that declared the client's elicitation capability is gone with
-- the old process, so without this row a restarted daemon cannot tell that the
-- client shows the in-session approval dialog. Written only while the daemon
-- can run that dialog (Approvals or Grants configured), on the peer_auth socket,
-- for a client that declared elicitation: every other install keeps this table
-- empty. Metadata about a connection, never corpus content.
--
--   id            the Mcp-Session-Id, minted by the daemon's transport
--   capabilities  the capabilities restored on the connection's calls, as JSON
--   created_at    when the client initialized
--   last_seen_at  when a call last used it (refreshed about once a day, so
--                 pruning a long-unused row at the next initialize is cheap)
--
-- Times are core.TimeFormat (fixed width, UTC), so a text comparison is a
-- chronological one.
CREATE TABLE mcp_sessions (
    id           TEXT PRIMARY KEY,
    capabilities TEXT NOT NULL,
    created_at   TEXT NOT NULL,
    last_seen_at TEXT NOT NULL
);

CREATE INDEX idx_mcp_sessions_last_seen ON mcp_sessions (last_seen_at);

-- 2. access_leases.briefed_at: when a tool result first carried the project's
-- briefing after the lease landed; NULL until then. It replaces a set in the
-- daemon's memory, so a restart does not send the one-time briefing again. Claimed
-- atomically (store.ClaimAccessLeaseBriefing): of two concurrent calls, one
-- carries it. Nullable and unread by anything older, so additive: the previous
-- release reads and writes access_leases by column name exactly as before.
ALTER TABLE access_leases ADD COLUMN briefed_at TEXT;
