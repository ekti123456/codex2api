package database

import (
	"context"
	"encoding/json"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

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
	for _, bad := range []string{"", "real", CodexTurnStateAliasPrefix + "bad"} {
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
