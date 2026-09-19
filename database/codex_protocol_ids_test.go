package database

import (
	"path/filepath"
	"sync"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestProtocolIdentityPersistenceConflictAndIsolation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "protocol.db")
	db, err := New("sqlite", path)
	require.NoError(t, err)
	binding := CodexTurnStateBinding{Scope: "user", RootKey: "root", AccountID: 12, AccountHash: "account"}
	pair := CodexProtocolPair{Public: "user-turn-1", Upstream: "official-turn-1"}
	var wg sync.WaitGroup
	errors := make(chan error, 8)
	for i := 0; i < 8; i++ {
		wg.Add(1)
		go func() { defer wg.Done(); errors <- db.PutCodexProtocolPair(t.Context(), binding, "turn", pair) }()
	}
	wg.Wait()
	close(errors)
	for err := range errors {
		require.NoError(t, err)
	}
	require.ErrorIs(t, db.PutCodexProtocolPair(t.Context(), binding, "turn", CodexProtocolPair{Public: pair.Public, Upstream: "conflict"}), ErrCodexIdentityAliasCollision)
	require.ErrorIs(t, db.PutCodexProtocolPair(t.Context(), binding, "turn", CodexProtocolPair{Public: "conflict", Upstream: pair.Upstream}), ErrCodexIdentityAliasCollision)
	var ciphertext string
	require.NoError(t, db.conn.QueryRow(`SELECT ciphertext FROM codex_protocol_ids`).Scan(&ciphertext))
	require.NotContains(t, ciphertext, pair.Upstream)
	require.NotContains(t, ciphertext, pair.Public)
	alias := db.CodexConversationAlias(binding, "conv_official")
	require.True(t, db.IsManagedCodexConversationAlias(alias))
	require.False(t, db.IsManagedCodexResponseID(alias))
	require.NoError(t, db.Close())
	db, err = New("sqlite", path)
	require.NoError(t, err)
	defer db.Close()
	for _, public := range []bool{false, true} {
		value := pair.Upstream
		if public {
			value = pair.Public
		}
		read, found, err := db.ReadCodexProtocolPair(t.Context(), binding, "turn", value, public)
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, pair, read)
	}
	for _, field := range []string{"scope", "root", "account", "hash", "generation", "kind"} {
		changed := binding
		kind := "turn"
		switch field {
		case "scope":
			changed.Scope = "other"
		case "root":
			changed.RootKey = "other"
		case "account":
			changed.AccountID++
		case "hash":
			changed.AccountHash = "other"
		case "generation":
			changed.Generation++
		case "kind":
			kind = "conversation"
		}
		_, found, err := db.ReadCodexProtocolPair(t.Context(), changed, kind, pair.Public, true)
		require.NoError(t, err)
		require.False(t, found)
	}
	require.Equal(t, alias, db.CodexConversationAlias(binding, "conv_official"))
	_, err = db.conn.Exec(`UPDATE codex_protocol_ids SET ciphertext='broken'`)
	require.NoError(t, err)
	_, _, err = db.ReadCodexProtocolPair(t.Context(), binding, "turn", pair.Public, true)
	require.Error(t, err)
}
