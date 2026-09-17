package database

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/base64"
	"encoding/binary"
	"encoding/hex"
	"encoding/json"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestTurnStateAliasEnvelopeAuthenticationAndLegacyRejection(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "aliases.db"))
	require.NoError(t, err)
	defer db.Close()
	binding := CodexTurnStateBinding{Scope: "scope", RootKey: "root", AccountID: 7}
	for _, size := range []int{73, 217, 249} {
		original := []byte(strings.Repeat("x", size))
		original[0] = 0x80
		binary.BigEndian.PutUint64(original[1:9], 1789660000)
		real := base64.URLEncoding.EncodeToString(original)
		record, err := db.IssueCodexTurnState(t.Context(), binding, real)
		require.NoError(t, err)
		require.Len(t, record.Alias, len(real))
		require.NotEqual(t, real, record.Alias)
		decoded, err := base64.URLEncoding.DecodeString(record.Alias)
		require.NoError(t, err)
		require.Equal(t, original[:9], decoded[:9])
		require.NotEqual(t, original[9:25], decoded[9:25])
		require.NotEqual(t, original[25:size-32], decoded[25:size-32])
		require.True(t, db.IsManagedCodexTurnStateAlias(record.Alias))
		require.False(t, db.IsManagedCodexTurnStateAlias(real))
		for _, offset := range []int{8, 9, 25, size - 1} {
			modified := append([]byte(nil), decoded...)
			modified[offset] ^= 1
			tampered := base64.URLEncoding.EncodeToString(modified)
			require.True(t, ValidCodexTurnStateAlias(tampered))
			require.False(t, db.IsManagedCodexTurnStateAlias(tampered))
			_, found, err := db.ReadCodexTurnState(t.Context(), tampered)
			require.NoError(t, err)
			require.False(t, found)
		}
		foreign := &DB{turnStateKey: []byte(strings.Repeat("z", 32))}
		require.False(t, foreign.IsManagedCodexTurnStateAlias(record.Alias))
	}
	// Even an unexpired row produced by the old source-key scheme is not reused.
	encoded, err := json.Marshal(binding)
	require.NoError(t, err)
	mac := hmac.New(sha256.New, db.turnStateKey)
	mac.Write(encoded)
	mac.Write([]byte{0})
	mac.Write([]byte("legacy-real"))
	legacy := "c2ts_v1_" + strings.Repeat("A", 43)
	_, err = db.conn.Exec(`INSERT INTO codex_turn_states(alias,source_key,binding,ciphertext,created_at,expires_at) VALUES($1,$2,$3,$4,$5,$6)`, legacy, hex.EncodeToString(mac.Sum(nil)), string(encoded), "unused", time.Now().Unix(), time.Now().Add(time.Hour).Unix())
	require.NoError(t, err)
	_, found, err := db.ReadCodexTurnState(t.Context(), legacy)
	require.NoError(t, err)
	require.False(t, found)
	fresh, err := db.IssueCodexTurnState(t.Context(), binding, "legacy-real")
	require.NoError(t, err)
	require.True(t, db.IsManagedCodexTurnStateAlias(fresh.Alias))
	require.Len(t, fresh.Alias, 292)
}

func TestTurnStateMappingPersistenceIsolationAndEncryption(t *testing.T) {
	path := filepath.Join(t.TempDir(), "turn-state.db")
	db, err := New("sqlite", path)
	require.NoError(t, err)
	ctx := context.Background()
	binding := CodexTurnStateBinding{Scope: "owner-root-turn", RootKey: "root", AccountID: 7, AccountHash: "account"}
	record, err := db.IssueCodexTurnState(ctx, binding, "real-sensitive-state")
	require.NoError(t, err)
	require.True(t, ValidCodexTurnStateAlias(record.Alias))
	again, err := db.IssueCodexTurnState(ctx, binding, record.Real)
	require.NoError(t, err)
	require.Equal(t, record.Alias, again.Alias)
	var ciphertext string
	require.NoError(t, db.conn.QueryRow(`SELECT ciphertext FROM codex_turn_states WHERE alias=$1`, record.Alias).Scan(&ciphertext))
	require.NotContains(t, ciphertext, record.Real)
	encoded, err := json.Marshal(record)
	require.NoError(t, err)
	require.NotContains(t, string(encoded), record.Real)
	for _, changed := range []CodexTurnStateBinding{
		{Scope: "different-user", RootKey: "root", AccountID: 7, AccountHash: "account"},
		{Scope: binding.Scope, RootKey: "root", AccountID: 8, AccountHash: "other-account"},
		{Scope: binding.Scope, RootKey: "root", AccountID: 7, AccountHash: "account", Generation: 2},
	} {
		different, err := db.IssueCodexTurnState(ctx, changed, record.Real)
		require.NoError(t, err)
		require.NotEqual(t, record.Alias, different.Alias)
	}
	require.NoError(t, db.Close())
	db, err = New("sqlite", path)
	require.NoError(t, err)
	defer db.Close()
	restored, found, err := db.ReadCodexTurnState(ctx, record.Alias)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, record.Real, restored.Real)
	again, err = db.IssueCodexTurnState(ctx, binding, record.Real)
	require.NoError(t, err)
	require.Equal(t, record.Alias, again.Alias)
	_, err = db.conn.Exec(`UPDATE codex_turn_states SET binding=$1 WHERE alias=$2`, `{"scope":"tampered"}`, record.Alias)
	require.NoError(t, err)
	_, found, err = db.ReadCodexTurnState(ctx, record.Alias)
	require.Error(t, err)
	require.False(t, found)
}

func TestTurnStateMappingSharedInstancesConcurrentIssueAndExpiry(t *testing.T) {
	path := filepath.Join(t.TempDir(), "turn-state.db")
	first, err := New("sqlite", path)
	require.NoError(t, err)
	defer first.Close()
	second, err := New("sqlite", path)
	require.NoError(t, err)
	defer second.Close()
	binding := CodexTurnStateBinding{Scope: "scope", RootKey: "root", AccountID: 9, AccountHash: "account"}
	results := make([]CodexTurnStateRecord, 16)
	errs := make([]error, 16)
	var wg sync.WaitGroup
	for i := range results {
		wg.Add(1)
		go func(i int) {
			defer wg.Done()
			db := first
			if i%2 != 0 {
				db = second
			}
			results[i], errs[i] = db.IssueCodexTurnState(context.Background(), binding, "real")
		}(i)
	}
	wg.Wait()
	for i := range results {
		require.NoError(t, errs[i])
		require.Equal(t, results[0].Alias, results[i].Alias)
	}
	_, err = first.conn.Exec(`UPDATE codex_turn_states SET expires_at=$1`, time.Now().Add(-time.Hour).Unix())
	require.NoError(t, err)
	_, found, err := second.ReadCodexTurnState(context.Background(), results[0].Alias)
	require.NoError(t, err)
	require.False(t, found)
	fresh, err := second.IssueCodexTurnState(context.Background(), binding, "real")
	require.NoError(t, err)
	require.NotEqual(t, results[0].Alias, fresh.Alias)
	for _, bad := range []string{"", "real", "c2ts_v1_" + strings.Repeat("A", 43)} {
		_, found, err = first.ReadCodexTurnState(context.Background(), bad)
		require.NoError(t, err)
		require.False(t, found)
	}
}

func TestTurnStateMappingMissingKeyFailsClosed(t *testing.T) {
	path := filepath.Join(t.TempDir(), "turn-state.db")
	db, err := New("sqlite", path)
	require.NoError(t, err)
	_, err = db.IssueCodexTurnState(context.Background(), CodexTurnStateBinding{Scope: "scope", AccountID: 1}, "real")
	require.NoError(t, err)
	_, err = db.conn.Exec(`DELETE FROM codex_turn_state_secret`)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	_, err = New("sqlite", path)
	require.ErrorContains(t, err, "turn-state encryption key missing")
}
