package database

import (
	"context"
	"database/sql"
	"errors"
	"time"
)

// Tracking starts at installation, not process startup: an in-flight turn from
// before this migration must never become a new turn after an upgrade/restart.
func (db *DB) ensureUsageTurnStarts(ctx context.Context) error {
	for _, ddl := range []string{
		`CREATE TABLE IF NOT EXISTS usage_turn_tracking (id INTEGER PRIMARY KEY, enabled_at_ms BIGINT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS usage_turn_starts (scope TEXT PRIMARY KEY, first_request TEXT NOT NULL, observed_at_ms BIGINT NOT NULL)`,
	} {
		if _, err := db.conn.ExecContext(ctx, ddl); err != nil {
			return err
		}
	}
	_, err := db.conn.ExecContext(ctx, `INSERT INTO usage_turn_tracking(id, enabled_at_ms) VALUES (1, $1) ON CONFLICT(id) DO NOTHING`, time.Now().UnixMilli())
	return err
}

// ObserveUsageTurnStart elects one logical ingress request, independently of
// completion order, account selection, retries and usage-log batch flushing.
// Empty first_request is a tombstone: we first saw this turn in progress.
func (db *DB) ObserveUsageTurnStart(ctx context.Context, scope, requestID string, startedAt time.Time, directUser bool) (*bool, error) {
	if scope == "" || requestID == "" || startedAt.IsZero() {
		return nil, nil
	}
	var enabledAt int64
	if err := db.conn.QueryRowContext(ctx, `SELECT enabled_at_ms FROM usage_turn_tracking WHERE id=1`).Scan(&enabledAt); err != nil {
		return nil, err
	}
	if startedAt.UnixMilli() < enabledAt {
		return nil, nil
	}
	var first string
	err := db.conn.QueryRowContext(ctx, `SELECT first_request FROM usage_turn_starts WHERE scope=$1`, scope).Scan(&first)
	if errors.Is(err, sql.ErrNoRows) {
		candidate := ""
		if directUser {
			candidate = requestID
		}
		err = db.withSQLiteWriteLock(ctx, func() error {
			_, err := db.conn.ExecContext(ctx, `INSERT INTO usage_turn_starts(scope, first_request, observed_at_ms) VALUES ($1,$2,$3) ON CONFLICT(scope) DO NOTHING`, scope, candidate, time.Now().UnixMilli())
			return err
		})
		if err == nil {
			err = db.conn.QueryRowContext(ctx, `SELECT first_request FROM usage_turn_starts WHERE scope=$1`, scope).Scan(&first)
		}
	}
	if err != nil || first == "" {
		return nil, err
	}
	value := first == requestID
	return &value, nil
}

func cloneUsageBool(value *bool) *bool {
	if value == nil {
		return nil
	}
	copy := *value
	return &copy
}
