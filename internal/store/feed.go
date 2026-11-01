package store

import (
	"context"
	"database/sql"
	"fmt"
)

// The session facts write screening's metadata feed (internal/feed) puts on
// its frames: a session's display name and the agent process stamped on it.

// SessionProcess is a session's display name and the agent process stamped on
// it ("" when none was).
type SessionProcess struct {
	Name         string
	AgentProcess string
}

// SessionAgentProcesses maps session ids to their display names and agent
// processes. Unknown ids are absent.
func SessionAgentProcesses(ctx context.Context, db *sql.DB, ids []string) (map[string]SessionProcess, error) {
	out := make(map[string]SessionProcess)
	for chunk := range chunked(ids, sqlInChunk) {
		err := eachRow(ctx, db, `SELECT id, name, agent_process FROM sessions
			WHERE id IN (`+placeholders(len(chunk))+`)`, anySlice(chunk), func(rows *sql.Rows) error {
			var id string
			var p SessionProcess
			if err := rows.Scan(&id, &p.Name, &p.AgentProcess); err != nil {
				return err
			}
			out[id] = p
			return nil
		})
		if err != nil {
			return nil, fmt.Errorf("store.SessionAgentProcesses: %w", err)
		}
	}
	return out, nil
}
