package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"fmt"
	"strings"
	"time"
)

type SessionAutoLockSettings struct {
	Enabled   bool  `json:"enabled"`
	Threshold int   `json:"threshold"`
	Revision  int64 `json:"-"`
}

func (db *DB) ensureSessionAutoLockSchema(ctx context.Context) error {
	if db.isSQLite() {
		if err := db.ensureSQLiteColumn(ctx, "session_blacklist", "lock_source", "TEXT NOT NULL DEFAULT 'manual'"); err != nil {
			return err
		}
	} else if _, err := db.conn.ExecContext(ctx, `ALTER TABLE session_blacklist ADD COLUMN IF NOT EXISTS lock_source TEXT NOT NULL DEFAULT 'manual'`); err != nil {
		return err
	}
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS session_auto_lock_settings (id INTEGER PRIMARY KEY, enabled INTEGER NOT NULL, threshold INTEGER NOT NULL, revision BIGINT NOT NULL)`,
		`INSERT INTO session_auto_lock_settings(id,enabled,threshold,revision) VALUES(1,0,3,1) ON CONFLICT(id) DO NOTHING`,
		`CREATE TABLE IF NOT EXISTS session_500_streaks (session_key TEXT PRIMARY KEY, failures INTEGER NOT NULL, revision BIGINT NOT NULL, updated_at BIGINT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_session_500_streaks_updated ON session_500_streaks(updated_at)`,
	} {
		if _, err := db.conn.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	var enabled int
	if err := db.conn.QueryRowContext(ctx, `SELECT enabled,threshold,revision FROM session_auto_lock_settings WHERE id=1`).Scan(&enabled, &db.sessionAutoLock.Threshold, &db.sessionAutoLock.Revision); err != nil {
		return err
	}
	db.sessionAutoLock.Enabled = enabled == 1
	return nil
}

func (db *DB) GetSessionAutoLockSettings() SessionAutoLockSettings {
	db.sessionAutoLockMu.RLock()
	defer db.sessionAutoLockMu.RUnlock()
	return db.sessionAutoLock
}

func (db *DB) SetSessionAutoLockSettings(ctx context.Context, next SessionAutoLockSettings) error {
	if next.Threshold < 1 || next.Threshold > 10000 {
		return fmt.Errorf("threshold must be between 1 and 10000")
	}
	db.sessionAutoLockMu.Lock()
	defer db.sessionAutoLockMu.Unlock()
	enabled := 0
	if next.Enabled {
		enabled = 1
	}
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		if err := tx.QueryRowContext(ctx, `UPDATE session_auto_lock_settings SET enabled=$1,threshold=$2,revision=revision+1 WHERE id=1 RETURNING revision`, enabled, next.Threshold).Scan(&next.Revision); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `DELETE FROM session_500_streaks`)
		return err
	})
	if err == nil {
		db.sessionAutoLock = next
	}
	return err
}

// Observe one final request result, never an internal retry. Completion order
// defines consecutive results. Counters and locks persist across restarts.
func (db *DB) ObserveSessionFinalStatus(ctx context.Context, identity SessionErrorIdentity, status int, started time.Time, settings SessionAutoLockSettings) (bool, error) {
	if !settings.Enabled || !ValidSessionOperationKey(identity.Key) || identity.UserID == "" || status < 100 {
		return false, nil
	}
	db.sessionAutoLockMu.RLock()
	defer db.sessionAutoLockMu.RUnlock()
	if !db.sessionAutoLock.Enabled || settings.Revision != db.sessionAutoLock.Revision {
		return false, nil
	}
	locked := false
	err := db.withWriteTx(ctx, func(tx *sql.Tx) error {
		// A stale worker cannot keep locking after persisted policy changes.
		var enabled int
		var revision int64
		if err := tx.QueryRowContext(ctx, `SELECT enabled,revision FROM session_auto_lock_settings WHERE id=1`).Scan(&enabled, &revision); err != nil {
			return err
		}
		if enabled != 1 || revision != settings.Revision {
			return nil
		}
		var existing int
		var updated int64
		query := `SELECT locked,updated_at FROM session_blacklist WHERE session_key=$1`
		if !db.isSQLite() {
			query += ` FOR UPDATE`
		}
		err := tx.QueryRowContext(ctx, query, identity.Key).Scan(&existing, &updated)
		if err != nil && err != sql.ErrNoRows {
			return err
		}
		// An old in-flight request must not re-lock a manually unlocked session.
		if err == nil && (existing == 1 || started.UnixMilli() <= updated) {
			return nil
		}
		if status != 500 {
			_, err = tx.ExecContext(ctx, `DELETE FROM session_500_streaks WHERE session_key=$1`, identity.Key)
			return err
		}
		var failures int
		err = tx.QueryRowContext(ctx, `INSERT INTO session_500_streaks(session_key,failures,revision,updated_at) VALUES($1,1,$2,$3) ON CONFLICT(session_key) DO UPDATE SET failures=CASE WHEN session_500_streaks.revision=excluded.revision THEN session_500_streaks.failures+1 ELSE 1 END,revision=excluded.revision,updated_at=excluded.updated_at RETURNING failures`, identity.Key, revision, time.Now().UnixMilli()).Scan(&failures)
		if err != nil || failures < settings.Threshold {
			return err
		}
		payload, err := json.Marshal(identity)
		if err != nil {
			return err
		}
		result, err := tx.ExecContext(ctx, `INSERT INTO session_blacklist(session_key,user_id,session_id,identity_data,locked,updated_at,lock_source) VALUES($1,$2,$3,$4,1,$5,'automatic') ON CONFLICT(session_key) DO UPDATE SET locked=1,updated_at=excluded.updated_at,lock_source='automatic' WHERE session_blacklist.locked=0 AND session_blacklist.updated_at<$6`, identity.Key, identity.UserID, identity.SessionID, string(payload), time.Now().UnixMilli(), started.UnixMilli())
		if err != nil {
			return err
		}
		count, err := result.RowsAffected()
		locked = count > 0
		return err
	})
	if err == nil && locked {
		db.invalidateSessionBlacklistCache()
	}
	return locked, err
}

func (db *DB) invalidateSessionBlacklistCache() {
	db.sessionBlacklist.mu.Lock()
	db.sessionBlacklist.entries = nil
	db.sessionBlacklist.version++
	db.sessionBlacklist.mu.Unlock()
}

func (db *DB) populateSessionLockSources(ctx context.Context, items []SessionErrorRow) error {
	args := []any{}
	for _, row := range items {
		if row.LockedBy == row.Identity.Key {
			args = append(args, row.Identity.Key)
		}
	}
	if len(args) == 0 {
		return nil
	}
	rows, err := db.conn.QueryContext(ctx, `SELECT session_key,lock_source FROM session_blacklist WHERE locked=1 AND session_key IN (`+strings.Join(dbPlaceholders(db.isSQLite(), 1, len(args)), ",")+`)`, args...)
	if err != nil {
		return err
	}
	defer rows.Close()
	sources := map[string]string{}
	for rows.Next() {
		var key, source string
		if err := rows.Scan(&key, &source); err != nil {
			return err
		}
		sources[key] = source
	}
	for i := range items {
		items[i].LockSource = sources[items[i].Identity.Key]
	}
	return rows.Err()
}
