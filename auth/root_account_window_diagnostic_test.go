package auth

import (
	"encoding/json"
	"github.com/stretchr/testify/require"
	"testing"
	"time"
)

func TestRootAccountWindowDiagnosticIsReadOnlyAndRedacted(t *testing.T) {
	store, account := newSessionCapacityTestStore(2)
	key, now := "private-original-session-key", time.Now()
	require.True(t, store.AdmitAccountSession(account, key, now))
	active := store.RootAccountWindowDiagnostic(key, account.ID(), now)
	require.Equal(t, "active", active.SlotState)
	require.Equal(t, "local", active.Scope)
	expired := store.RootAccountWindowDiagnostic(key, account.ID(), now.Add(2*time.Minute))
	require.Equal(t, "expired", expired.SlotState)
	require.Equal(t, active.LastSeen, expired.LastSeen)
	require.Contains(t, store.accountSessions[account.ID()], key, "diagnostics must not purge or refresh slots")
	require.True(t, store.RemoveAccountSession(account.ID(), key))
	missing := store.RootAccountWindowDiagnostic(key, account.ID(), now)
	require.Equal(t, "missing", missing.SlotState)
	require.True(t, missing.AccountPresent)
	require.False(t, store.RootAccountWindowDiagnostic(key, 999, now).AccountPresent)
	payload, err := json.Marshal(expired)
	require.NoError(t, err)
	require.NotContains(t, string(payload), key)
	require.NotContains(t, string(payload), "token")
}
