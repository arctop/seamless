package store

// The Tasks list's place query (an item's own page, store/itempage.go).

import (
	"context"
	"database/sql"
	"fmt"
	"time"

	"github.com/arctop/seamless/internal/core"
)

// TaskPlace is where task id sits in the Tasks list under f: its groups in
// the list's order -- in progress under a live claim, in progress with none,
// ready, blocked, the gates, the steps waiting on a gate, then the closed
// tasks newest-closed first -- each open group oldest-created first (newest
// first with newest, but the gates, which go by who must act). now is the
// instant a claim is judged live at. found is false when the list does not
// hold the task.
func TaskPlace(ctx context.Context, db *sql.DB, f TaskFilter, newest bool, now time.Time, id string) (ItemPlace, bool, error) {
	at := core.FormatTime(now.UTC())
	started := "t.status = 'in_progress' AND t.kind <> 'gate'"
	rank := `CASE
		WHEN ` + started + ` AND ` + sqlClaimLive("t") + ` THEN 0
		WHEN ` + started + ` THEN 1
		WHEN ` + sqlReady("t") + ` THEN 2
		WHEN ` + sqlBlocked("t") + ` THEN 3
		WHEN ` + sqlGates("t") + ` THEN 4
		WHEN ` + sqlGated("t") + ` THEN 5
		WHEN t.status IN ('done','dropped') THEN 6
	END`
	where, args := f.where("t")
	inner := `SELECT id, label, rk, created_at, closed, who FROM (
		SELECT t.id AS id, t.title AS label, ` + rank + ` AS rk, t.created_at AS created_at,
		       COALESCE(t.closed_at, t.updated_at) AS closed, lower(t.gate_who) AS who
		  FROM tasks t
		 WHERE 1 = 1` + where + `
	) WHERE rk IS NOT NULL`
	dir := "ASC"
	if newest {
		dir = "DESC"
	}
	order := `rk,
		CASE WHEN rk = 4 THEN who END,
		CASE WHEN rk = 6 THEN closed END DESC,
		CASE WHEN rk = 6 THEN id END DESC,
		CASE WHEN rk = 4 THEN created_at END,
		CASE WHEN rk = 4 THEN id END,
		created_at ` + dir + `, id ` + dir
	all := append([]any{at}, args...)
	place, found, err := itemPlace(ctx, db, placeSQL(inner, order), append(all, id), id)
	if err != nil {
		return ItemPlace{}, false, fmt.Errorf("store.TaskPlace: %w", err)
	}
	return place, found, nil
}
