package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"errors"
)

func (db *DB) ResolveSessionOutboundWindows(ctx context.Context, rootKey string, accountID int64, generation uint64, windows map[string]uint64) (map[string]uint64, error) {
	var record SessionContinuityRecord
	err := db.withWriteTx(ctx, func(transaction *sql.Tx) error {
		if _, err := transaction.ExecContext(ctx, `UPDATE codex_session_continuity SET state=state WHERE root_key=$1`, rootKey); err != nil {
			return err
		}
		var raw string
		if err := transaction.QueryRowContext(ctx, `SELECT state FROM codex_session_continuity WHERE root_key=$1`, rootKey).Scan(&raw); err != nil {
			return err
		}
		if err := json.Unmarshal([]byte(raw), &record); err != nil {
			return err
		}
		if !record.OutboundWindowReset || record.AccountID != accountID || record.FailoverCount != generation {
			return ErrSessionOwnerConflict
		}
		if record.OutboundWindowBases == nil {
			record.OutboundWindowBases = make(map[string]uint64)
		}
		changed := false
		for thread, number := range windows {
			if thread == "" {
				return errors.New("missing outbound window thread")
			}
			if base, found := record.OutboundWindowBases[thread]; found {
				if number < base {
					return errors.New("outbound window predates current account segment")
				}
			} else {
				if len(record.OutboundWindowBases) >= 1024 {
					return errors.New("outbound window identity limit exceeded")
				}
				record.OutboundWindowBases[thread] = number
				changed = true
			}
		}
		if !changed {
			return nil
		}
		payload, err := json.Marshal(record)
		if err != nil {
			return err
		}
		_, err = transaction.ExecContext(ctx, `UPDATE codex_session_continuity SET state=$2 WHERE root_key=$1`, rootKey, string(payload))
		return err
	})
	return record.OutboundWindowBases, err
}
