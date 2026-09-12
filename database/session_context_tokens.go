package database

import (
	"context"
	"database/sql"
	"errors"
	"fmt"
	"sort"
	"strings"
	"time"
)

func (db *DB) RecordSessionContextTokens(ctx context.Context, tokens map[string]time.Time) error {
	if len(tokens) == 0 {
		return nil
	}
	if len(tokens) > 4096 {
		return errors.New("too many session context tokens")
	}
	keys := make([]string, 0, len(tokens))
	for key, expiry := range tokens {
		if !ValidSessionOperationKey(key) || expiry.IsZero() {
			return errors.New("invalid session context token")
		}
		keys = append(keys, key)
	}
	sort.Strings(keys)
	values, arguments := make([]string, 0, len(keys)), make([]any, 0, 2*len(keys))
	for index, key := range keys {
		values = append(values, fmt.Sprintf("($%d,$%d)", index*2+1, index*2+2))
		arguments = append(arguments, key, tokens[key].Unix())
	}
	return db.withWriteTx(ctx, func(transaction *sql.Tx) error {
		if _, err := transaction.ExecContext(ctx, `DELETE FROM codex_session_context_tokens WHERE expires_at <= $1`, time.Now().Unix()); err != nil {
			return err
		}
		_, err := transaction.ExecContext(ctx, `INSERT INTO codex_session_context_tokens(token_key,expires_at) VALUES `+strings.Join(values, ",")+` ON CONFLICT(token_key) DO UPDATE SET expires_at=CASE WHEN EXCLUDED.expires_at > codex_session_context_tokens.expires_at THEN EXCLUDED.expires_at ELSE codex_session_context_tokens.expires_at END`, arguments...)
		return err
	})
}

func (db *DB) HasSessionContextToken(ctx context.Context, key string) (bool, error) {
	if !ValidSessionOperationKey(key) {
		return false, errors.New("invalid session context token")
	}
	var found bool
	err := db.conn.QueryRowContext(ctx, `SELECT EXISTS(SELECT 1 FROM codex_session_context_tokens WHERE token_key=$1 AND expires_at>$2)`, key, time.Now().Unix()).Scan(&found)
	return found, err
}
