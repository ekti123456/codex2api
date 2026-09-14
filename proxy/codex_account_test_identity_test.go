package proxy

import (
	"context"
	"path/filepath"
	"sync"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/google/uuid"
	"github.com/stretchr/testify/require"
)

func TestCodexAccountTestRootPersistenceAndIsolation(test *testing.T) {
	dbPath := filepath.Join(test.TempDir(), "test-roots.db")
	db, err := database.New("sqlite", dbPath)
	require.NoError(test, err)
	closed := false
	test.Cleanup(func() {
		if !closed {
			require.NoError(test, db.Close())
		}
	})
	account := &auth.Account{DBID: 42, AccountID: "workspace-a", AccessToken: "old-token"}
	var wg sync.WaitGroup
	values, failures := make(chan string, 8), make(chan error, 8)
	for range 8 {
		wg.Go(func() {
			value, err := ResolveCodexAccountTestSessionID(test.Context(), db, account)
			values <- value
			failures <- err
		})
	}
	wg.Wait()
	root := <-values
	for range 8 {
		require.NoError(test, <-failures)
	}
	for range 7 {
		require.Equal(test, root, <-values)
	}
	parsed, err := uuid.Parse(root)
	require.NoError(test, err)
	require.Equal(test, uuid.Version(7), parsed.Version())
	require.NoError(test, db.Close())
	db, err = database.New("sqlite", dbPath)
	require.NoError(test, err)
	reloaded := &auth.Account{DBID: 42, AccountID: "workspace-a", AccessToken: "refreshed-token", CodexInstallationID: "new-device"}
	resolve := func(account *auth.Account) string {
		value, err := ResolveCodexAccountTestSessionID(test.Context(), db, account)
		require.NoError(test, err)
		return value
	}
	require.Equal(test, root, resolve(reloaded))
	reloaded.DBID = 43
	require.NotEqual(test, root, resolve(reloaded))
	reloaded.DBID = 42
	reloaded.CustomHeaders = map[string]string{"Chatgpt-Account-Id": "workspace-b"}
	require.NotEqual(test, root, resolve(reloaded))
	_, err = ResolveCodexAccountTestSessionID(test.Context(), db, nil)
	require.Error(test, err)
	require.NoError(test, db.Close())
	closed = true
	value, err := ResolveCodexAccountTestSessionID(test.Context(), db, account)
	require.Error(test, err)
	require.Empty(test, value, "database failure must not silently generate a new root")
}

func TestCodexAccountTestOwnerExcludesOrdinaryAnonymousRequests(test *testing.T) {
	test.Setenv("CODEX_OUTBOUND_SESSION_MODE", "preserve")
	db, err := database.New("sqlite", filepath.Join(test.TempDir(), "test-owner.db"))
	require.NoError(test, err)
	test.Cleanup(func() { require.NoError(test, db.Close()) })
	account := &auth.Account{DBID: 42, AccountID: "workspace-a"}
	session, err := ResolveCodexAccountTestSessionID(test.Context(), db, account)
	require.NoError(test, err)
	body := []byte(`{"model":"gpt-5.5","client_metadata":{"session_id":"` + session + `","thread_id":"` + session + `"}}`)
	claim := func(ctx context.Context) error {
		fingerprint := NewCodexTransportFingerprint(account, nil, body, session, ctx)
		return fingerprint.ClaimSessionIdentity(ctx, account, "")
	}
	first := WithCodexAccountTestIdentityStore(test.Context(), db, account)
	second := WithCodexAccountTestIdentityStore(test.Context(), db, account)
	require.NoError(test, claim(first))
	require.NoError(test, claim(second))
	require.Equal(test, WebsocketTransportOwner(first, ""), WebsocketTransportOwner(second, ""))
	require.Error(test, claim(WithCodexIdentityStore(test.Context(), db)))
}
