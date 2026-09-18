package database

import (
	"context"
	"fmt"
	"strings"
	"time"
)

type AccountHealthLatestRequest struct {
	CreatedAt       time.Time `json:"created_at"`
	TurnStateLength *int      `json:"turn_state_length"`
}

// GetAccountHealthLatestRequests returns one latest recorded user request per
// visible account, including an explicit zero/null length. Never search backward
// for an older nonempty value or load request diagnostics. Empty IDs mean no work.
func (db *DB) GetAccountHealthLatestRequests(ctx context.Context, ids []int64, now time.Time) (map[int64]AccountHealthLatestRequest, error) {
	ids = positiveUniqueIDs(ids)
	result := make(map[int64]AccountHealthLatestRequest, len(ids))
	if len(ids) == 0 || len(ids) > accountRequestCountBreakdownMaxIDs {
		return result, nil
	}
	query, args := db.accountHealthLatestQuery(ids, now)
	rows, err := db.conn.QueryContext(ctx, query, args...)
	if err != nil {
		return nil, err
	}
	defer rows.Close()
	for rows.Next() {
		var id int64
		var created any
		var item AccountHealthLatestRequest
		if err := rows.Scan(&id, &created, &item.TurnStateLength); err != nil {
			return nil, err
		}
		item.CreatedAt, err = parseDBTimeValue(created)
		if err != nil {
			return nil, err
		}
		result[id] = item
	}
	return result, rows.Err()
}

func (db *DB) accountHealthLatestQuery(ids []int64, now time.Time) (string, []any) {
	// Each bounded account lookup uses (account_id, created_at), with id breaking
	// ties from batched inserts. LIMIT 1 is inside the lookup, not after a full
	// history aggregation. PostgreSQL uses a single array parameter for page IDs.
	if !db.isSQLite() {
		return `SELECT requested.account_id, latest.created_at, latest.turn_state_length
			FROM unnest($1::bigint[]) AS requested(account_id)
			CROSS JOIN LATERAL (
				SELECT created_at, turn_state_length FROM usage_logs
				WHERE account_id = requested.account_id AND created_at <= $2
				AND ` + db.endUserUsageLogPredicate() + `
				ORDER BY created_at DESC, id DESC LIMIT 1
			) AS latest`, []any{postgresInt8Array(ids), now}
	}
	args := []any{db.timeArg(now)}
	values := make([]string, 0, len(ids))
	for _, id := range ids {
		args = append(args, id)
		values = append(values, fmt.Sprintf("($%d)", len(args)))
	}
	return `WITH requested(account_id) AS (VALUES ` + strings.Join(values, ",") + `)
		SELECT requested.account_id, latest.created_at, latest.turn_state_length
		FROM requested JOIN usage_logs AS latest ON latest.id = (
			SELECT id FROM usage_logs
			WHERE account_id = requested.account_id AND created_at <= $1
			AND ` + db.endUserUsageLogPredicate() + `
			ORDER BY created_at DESC, id DESC LIMIT 1
		)`, args
}
