package database

import (
	"context"
	"github.com/stretchr/testify/require"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"
)

func autoLockIdentity(key string) SessionErrorIdentity {
	return SessionErrorIdentity{Key: strings.Repeat(key, 64), Kind: "newapi", Platform: "test", UserID: key, SessionID: "same-session"}
}

func TestSessionAutoLockConsecutiveResetAndFiltering(t *testing.T) {
	db := newGrokStateTestDB(t)
	ctx := context.Background()
	identity := autoLockIdentity("a")
	settings := db.GetSessionAutoLockSettings()
	require.False(t, settings.Enabled)
	require.Equal(t, 3, settings.Threshold)
	for range 4 {
		locked, err := db.ObserveSessionFinalStatus(ctx, identity, 500, time.Now(), settings)
		require.NoError(t, err)
		require.False(t, locked)
	}
	require.NoError(t, db.SetSessionAutoLockSettings(ctx, SessionAutoLockSettings{Enabled: true, Threshold: 3}))
	settings = db.GetSessionAutoLockSettings()
	for _, status := range []int{500, 500, 503, 500, 200, 500, 500} {
		locked, err := db.ObserveSessionFinalStatus(ctx, identity, status, time.Now(), settings)
		require.NoError(t, err)
		require.False(t, locked)
	}
	locked, err := db.ObserveSessionFinalStatus(ctx, identity, 500, time.Now(), settings)
	require.NoError(t, err)
	require.True(t, locked)
	owner, err := db.SessionBlacklistStatus(ctx, identity.Key, "")
	require.NoError(t, err)
	require.Equal(t, identity.Key, owner)
	manual := autoLockIdentity("b")
	require.NoError(t, db.insertSessionErrors(ctx, []SessionErrorEvent{{Identity: identity, CreatedAt: time.Now()}, {Identity: manual, CreatedAt: time.Now()}}))
	require.NoError(t, db.SetSessionBlacklist(ctx, []string{manual.Key}, true))
	for _, blacklist := range []bool{false, true} {
		page, err := db.ListSessionErrors(ctx, SessionErrorQuery{LockState: "auto_locked", LockedOnly: blacklist, Limit: 1})
		require.NoError(t, err)
		require.EqualValues(t, 1, page.Groups)
		require.Len(t, page.Items, 1)
		require.Equal(t, "automatic", page.Items[0].LockSource)
		require.Equal(t, identity.Key, page.Items[0].Identity.Key)
		require.Empty(t, page.NextCursor)
	}
	oldStarted := time.Now().Add(-time.Second)
	require.NoError(t, db.SetSessionBlacklist(ctx, []string{identity.Key}, false))
	for range 4 {
		locked, err = db.ObserveSessionFinalStatus(ctx, identity, 500, oldStarted, settings)
		require.NoError(t, err)
		require.False(t, locked)
	}
	// Requests beginning after the persisted unlock start a new streak.
	for i := 1; i <= 3; i++ {
		locked, err = db.ObserveSessionFinalStatus(ctx, identity, 500, time.Now().Add(time.Millisecond), settings)
		require.NoError(t, err)
		require.Equal(t, i == 3, locked)
	}
	require.NoError(t, db.SetSessionAutoLockSettings(ctx, SessionAutoLockSettings{Enabled: false, Threshold: 500}))
	owner, err = db.SessionBlacklistStatus(ctx, identity.Key, "")
	require.NoError(t, err)
	require.Equal(t, identity.Key, owner)
	// Switching a direct automatic lock to manual preserves enforcement and attribution.
	require.NoError(t, db.SetSessionBlacklist(ctx, []string{identity.Key}, true))
	page, err := db.ListSessionErrors(ctx, SessionErrorQuery{LockedOnly: true, LockState: "auto_locked"})
	require.NoError(t, err)
	require.Empty(t, page.Items)
}

func TestSessionAutoLockRestartAndConcurrentResults(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "auto.db")
	db, err := New("sqlite", path)
	require.NoError(t, err)
	require.NoError(t, db.SetSessionAutoLockSettings(ctx, SessionAutoLockSettings{Enabled: true, Threshold: 3}))
	identity := autoLockIdentity("c")
	settings := db.GetSessionAutoLockSettings()
	_, err = db.ObserveSessionFinalStatus(ctx, identity, 500, time.Now(), settings)
	require.NoError(t, err)
	require.NoError(t, db.Close())
	db, err = New("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	require.Equal(t, settings, db.GetSessionAutoLockSettings())
	locked, err := db.ObserveSessionFinalStatus(ctx, identity, 500, time.Now(), settings)
	require.NoError(t, err)
	require.False(t, locked)
	locked, err = db.ObserveSessionFinalStatus(ctx, identity, 500, time.Now(), settings)
	require.NoError(t, err)
	require.True(t, locked)
	require.NoError(t, db.SetSessionAutoLockSettings(ctx, SessionAutoLockSettings{Enabled: true, Threshold: 20}))
	settings = db.GetSessionAutoLockSettings()
	other := autoLockIdentity("d")
	errs := make(chan error, 20)
	var wg sync.WaitGroup
	for range 20 {
		wg.Add(1)
		go func() {
			defer wg.Done()
			_, err := db.ObserveSessionFinalStatus(ctx, other, 500, time.Now(), settings)
			errs <- err
		}()
	}
	wg.Wait()
	close(errs)
	for err := range errs {
		require.NoError(t, err)
	}
	owner, err := db.SessionBlacklistStatus(ctx, other.Key, "")
	require.NoError(t, err)
	require.Equal(t, other.Key, owner)
	oldSettings := settings
	require.NoError(t, db.SetSessionAutoLockSettings(ctx, SessionAutoLockSettings{Enabled: true, Threshold: 1}))
	locked, err = db.ObserveSessionFinalStatus(ctx, autoLockIdentity("e"), 500, time.Now(), oldSettings)
	require.NoError(t, err)
	require.False(t, locked)
}
