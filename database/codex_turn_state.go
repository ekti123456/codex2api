package database

import (
	"context"
	"crypto/aes"
	"crypto/cipher"
	"crypto/hmac"
	"crypto/rand"
	"crypto/sha256"
	"database/sql"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"strings"
	"time"
)

const CodexTurnStateAliasPrefix = "c2ts_v1_"
const CodexTurnStateTTL = 7 * 24 * time.Hour

type TurnStateEvent struct {
	Action     string     `json:"action"`
	Carrier    string     `json:"carrier"`
	Alias      string     `json:"alias,omitempty"`
	RealHash   string     `json:"real_hash,omitempty"`
	AccountID  int64      `json:"account_id,omitempty"`
	Generation uint64     `json:"generation"`
	At         time.Time  `json:"at"`
	ExpiresAt  *time.Time `json:"expires_at,omitempty"`
}

type TurnStateDiagnostic struct {
	ScopeHash string           `json:"scope_hash"`
	Events    []TurnStateEvent `json:"events"`
	Omitted   int              `json:"omitted,omitempty"`
}

// No plaintext upstream token is serialized in diagnostics or stored in a column.
type CodexTurnStateBinding struct {
	Scope       string `json:"scope"`
	RootKey     string `json:"root_key"`
	AccountID   int64  `json:"account_id"`
	AccountHash string `json:"account_hash"`
	Generation  uint64 `json:"generation"`
}

type CodexTurnStateRecord struct {
	CodexTurnStateBinding
	Alias     string    `json:"alias"`
	Real      string    `json:"-"`
	CreatedAt time.Time `json:"created_at"`
	ExpiresAt time.Time `json:"expires_at"`
}

func ValidCodexTurnStateAlias(value string) bool {
	if len(value) != len(CodexTurnStateAliasPrefix)+43 || !strings.HasPrefix(value, CodexTurnStateAliasPrefix) {
		return false
	}
	b, err := base64.RawURLEncoding.DecodeString(strings.TrimPrefix(value, CodexTurnStateAliasPrefix))
	return err == nil && len(b) == 32 && base64.RawURLEncoding.EncodeToString(b) == strings.TrimPrefix(value, CodexTurnStateAliasPrefix)
}

func (db *DB) ensureCodexTurnStateTable(ctx context.Context) error {
	for _, statement := range []string{
		`CREATE TABLE IF NOT EXISTS codex_turn_state_secret (id INTEGER PRIMARY KEY, secret TEXT NOT NULL)`,
		`CREATE TABLE IF NOT EXISTS codex_turn_states (alias TEXT PRIMARY KEY, source_key TEXT NOT NULL UNIQUE, binding TEXT NOT NULL, ciphertext TEXT NOT NULL, created_at BIGINT NOT NULL, expires_at BIGINT NOT NULL)`,
		`CREATE INDEX IF NOT EXISTS idx_codex_turn_states_expiry ON codex_turn_states(expires_at)`,
	} {
		if _, err := db.conn.ExecContext(ctx, statement); err != nil {
			return err
		}
	}
	return db.withWriteTx(ctx, func(tx *sql.Tx) error {
		var secret string
		err := tx.QueryRowContext(ctx, `SELECT secret FROM codex_turn_state_secret WHERE id=1`).Scan(&secret)
		if errors.Is(err, sql.ErrNoRows) {
			var count int
			if err := tx.QueryRowContext(ctx, `SELECT COUNT(*) FROM codex_turn_states`).Scan(&count); err != nil {
				return err
			}
			if count != 0 {
				return errors.New("turn-state encryption key missing")
			}
			key := make([]byte, 32)
			if _, err := rand.Read(key); err != nil {
				return err
			}
			if _, err := tx.ExecContext(ctx, `INSERT INTO codex_turn_state_secret(id,secret) VALUES(1,$1) ON CONFLICT(id) DO NOTHING`, hex.EncodeToString(key)); err != nil {
				return err
			}
			err = tx.QueryRowContext(ctx, `SELECT secret FROM codex_turn_state_secret WHERE id=1`).Scan(&secret)
		}
		if err != nil {
			return err
		}
		key, err := hex.DecodeString(secret)
		if err != nil || len(key) != 32 {
			return errors.New("invalid turn-state encryption key")
		}
		block, err := aes.NewCipher(key)
		if err != nil {
			return err
		}
		db.turnStateCipher, err = cipher.NewGCM(block)
		db.turnStateKey = key
		return err
	})
}

func (db *DB) IssueCodexTurnState(ctx context.Context, binding CodexTurnStateBinding, real string) (CodexTurnStateRecord, error) {
	var result CodexTurnStateRecord
	if db.turnStateCipher == nil || binding.Scope == "" || binding.AccountID <= 0 || real == "" || len(real) > 16384 {
		return result, errors.New("invalid turn-state mapping")
	}
	encoded, err := json.Marshal(binding)
	if err != nil {
		return result, err
	}
	mac := hmac.New(sha256.New, db.turnStateKey)
	mac.Write(encoded)
	mac.Write([]byte{0})
	mac.Write([]byte(real))
	source := hex.EncodeToString(mac.Sum(nil))
	random := make([]byte, 32)
	if _, err := rand.Read(random); err != nil {
		return result, err
	}
	alias := CodexTurnStateAliasPrefix + base64.RawURLEncoding.EncodeToString(random)
	nonce := make([]byte, db.turnStateCipher.NonceSize())
	if _, err := rand.Read(nonce); err != nil {
		return result, err
	}
	sealed := db.turnStateCipher.Seal(nonce, nonce, []byte(real), []byte(alias+"\x00"+string(encoded)))
	now := time.Now().UTC()
	err = db.withWriteTx(ctx, func(tx *sql.Tx) error {
		// Bound cleanup work; index-backed batches avoid full-table scans on request paths.
		if db.turnStateWrites.Add(1)%256 == 0 {
			if _, err := tx.ExecContext(ctx, `DELETE FROM codex_turn_states WHERE alias IN (SELECT alias FROM codex_turn_states WHERE expires_at <= $1 ORDER BY expires_at LIMIT 512)`, now.Unix()); err != nil {
				return err
			}
		}
		if _, err := tx.ExecContext(ctx, `DELETE FROM codex_turn_states WHERE source_key=$1 AND expires_at <= $2`, source, now.Unix()); err != nil {
			return err
		}
		_, err := tx.ExecContext(ctx, `INSERT INTO codex_turn_states(alias,source_key,binding,ciphertext,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6) ON CONFLICT(source_key) DO UPDATE SET expires_at=excluded.expires_at`, alias, source, string(encoded), base64.StdEncoding.EncodeToString(sealed), now.Unix(), now.Add(CodexTurnStateTTL).Unix())
		if err != nil {
			return err
		}
		var created, expires int64
		if err := tx.QueryRowContext(ctx, `SELECT alias,created_at,expires_at FROM codex_turn_states WHERE source_key=$1`, source).Scan(&result.Alias, &created, &expires); err != nil {
			return err
		}
		result.CodexTurnStateBinding, result.Real = binding, real
		result.CreatedAt, result.ExpiresAt = time.Unix(created, 0).UTC(), time.Unix(expires, 0).UTC()
		return nil
	})
	return result, err
}

func (db *DB) ReadCodexTurnState(ctx context.Context, alias string) (CodexTurnStateRecord, bool, error) {
	var result CodexTurnStateRecord
	if !ValidCodexTurnStateAlias(alias) {
		return result, false, nil
	}
	var binding, ciphertext string
	var created, expires int64
	err := db.conn.QueryRowContext(ctx, `SELECT binding,ciphertext,created_at,expires_at FROM codex_turn_states WHERE alias=$1 AND expires_at > $2`, alias, time.Now().Unix()).Scan(&binding, &ciphertext, &created, &expires)
	if errors.Is(err, sql.ErrNoRows) {
		return result, false, nil
	}
	if err != nil {
		return result, false, err
	}
	if json.Unmarshal([]byte(binding), &result.CodexTurnStateBinding) != nil || db.turnStateCipher == nil {
		return result, false, errors.New("invalid turn-state mapping")
	}
	sealed, err := base64.StdEncoding.DecodeString(ciphertext)
	if err != nil || len(sealed) < db.turnStateCipher.NonceSize() {
		return result, false, errors.New("invalid turn-state ciphertext")
	}
	n := db.turnStateCipher.NonceSize()
	real, err := db.turnStateCipher.Open(nil, sealed[:n], sealed[n:], []byte(alias+"\x00"+binding))
	if err != nil {
		return result, false, fmt.Errorf("turn-state decryption failed: %w", err)
	}
	result.Alias, result.Real = alias, string(real)
	result.CreatedAt, result.ExpiresAt = time.Unix(created, 0).UTC(), time.Unix(expires, 0).UTC()
	return result, true, nil
}
