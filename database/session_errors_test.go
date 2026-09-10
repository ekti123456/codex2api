package database

import (
	"context"
	"database/sql"
	"encoding/json"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSessionErrorAggregationAndBlacklistPersistence(test *testing.T) {
	path := filepath.Join(test.TempDir(), "session-errors.db")
	db, err := New("sqlite", path)
	require.NoError(test, err)
	now := time.Now().UTC().Truncate(time.Millisecond)
	rootKey, otherUserKey, forkKey := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	root := SessionErrorIdentity{Key: rootKey, Kind: "newapi", Platform: "platform", UserID: "17", SessionID: "root-session"}
	otherUser, fork := root, root
	otherUser.Key, otherUser.UserID = otherUserKey, "18"
	fork.Key, fork.SessionID = forkKey, "fork-session"
	for _, event := range []SessionErrorEvent{
		{Identity: root, CreatedAt: now, RequestID: "newest", Code: "server_error"},
		{Identity: root, CreatedAt: now.Add(-time.Minute), RequestID: "older"},
		{Identity: otherUser, CreatedAt: now},
		{Identity: fork, CreatedAt: now},
	} {
		require.True(test, db.EnqueueSessionError(event))
	}
	require.NoError(test, db.Close())
	db, err = New("sqlite", path)
	require.NoError(test, err)
	test.Cleanup(func() { require.NoError(test, db.Close()) })
	page, err := db.ListSessionErrors(context.Background(), SessionErrorQuery{UserID: "17", Limit: 20})
	require.NoError(test, err)
	require.Equal(test, int64(2), page.Groups)
	require.Equal(test, int64(3), page.Errors)
	for _, row := range page.Items {
		if row.Identity.Key == rootKey {
			require.Equal(test, int64(2), row.Count)
			require.Equal(test, "newest", row.Latest.RequestID)
		}
	}
	first, err := db.ListSessionErrors(context.Background(), SessionErrorQuery{Limit: 1})
	require.NoError(test, err)
	require.NotEmpty(test, first.NextCursor)
	second, err := db.ListSessionErrors(context.Background(), SessionErrorQuery{Limit: 1, Cursor: first.NextCursor})
	require.NoError(test, err)
	require.NotEqual(test, first.Items[0].Identity.Key, second.Items[0].Identity.Key)
	require.NoError(test, db.RecordSessionParent(context.Background(), forkKey, rootKey))
	require.NoError(test, db.SetSessionBlacklist(context.Background(), []string{rootKey}, true))
	lockedBy, err := db.SessionBlacklistStatus(context.Background(), forkKey, "")
	require.NoError(test, err)
	require.Equal(test, rootKey, lockedBy)
	lockedBy, err = db.SessionBlacklistStatus(context.Background(), otherUserKey, "")
	require.NoError(test, err)
	require.Empty(test, lockedBy)
	require.ErrorIs(test, db.SetSessionBlacklist(context.Background(), []string{otherUserKey, strings.Repeat("d", 64)}, true), sql.ErrNoRows)
	lockedBy, err = db.SessionBlacklistStatus(context.Background(), otherUserKey, "")
	require.NoError(test, err)
	require.Empty(test, lockedBy)
	require.NoError(test, db.pruneSessionErrors(context.Background(), now.Add(8*24*time.Hour)))
	page, err = db.ListSessionErrors(context.Background(), SessionErrorQuery{LockedOnly: true})
	require.NoError(test, err)
	require.Len(test, page.Items, 1)
	require.Equal(test, int64(0), page.Items[0].Count)
	require.True(test, page.Items[0].Locked)
	require.NoError(test, db.Close())
	db, err = New("sqlite", path)
	require.NoError(test, err)
	lockedBy, err = db.SessionBlacklistStatus(context.Background(), forkKey, "")
	require.NoError(test, err)
	require.Equal(test, rootKey, lockedBy)
	require.NoError(test, db.SetSessionBlacklist(context.Background(), []string{rootKey}, false))
	lockedBy, err = db.SessionBlacklistStatus(context.Background(), forkKey, "")
	require.NoError(test, err)
	require.Empty(test, lockedBy)
}

func TestSessionBlacklistLateForkEvidenceInvalidatesCachedAllowance(test *testing.T) {
	db, err := New("sqlite", filepath.Join(test.TempDir(), "lineage.db"))
	require.NoError(test, err)
	test.Cleanup(func() { require.NoError(test, db.Close()) })
	rootKey, childKey, grandchildKey := strings.Repeat("1", 64), strings.Repeat("2", 64), strings.Repeat("3", 64)
	root := SessionErrorIdentity{Key: rootKey, UserID: "17", SessionID: "root"}
	require.NoError(test, db.insertSessionErrors(context.Background(), []SessionErrorEvent{{Identity: root, CreatedAt: time.Now()}}))
	require.NoError(test, db.SetSessionBlacklist(context.Background(), []string{rootKey}, true))
	lockedBy, err := db.SessionBlacklistStatus(context.Background(), childKey, "")
	require.NoError(test, err)
	require.Empty(test, lockedBy)
	lockedBy, err = db.SessionBlacklistStatus(context.Background(), childKey, rootKey)
	require.NoError(test, err)
	require.Equal(test, rootKey, lockedBy)
	lockedBy, err = db.SessionBlacklistStatus(context.Background(), childKey, "")
	require.NoError(test, err)
	require.Equal(test, rootKey, lockedBy)
	lockedBy, err = db.SessionBlacklistStatus(context.Background(), grandchildKey, childKey)
	require.NoError(test, err)
	require.Equal(test, rootKey, lockedBy)
	require.ErrorIs(test, db.RecordSessionParent(context.Background(), childKey, grandchildKey), ErrSessionLineageConflict)
}

func TestSessionErrorCollectorBoundedAndRejectsMissingIdentity(test *testing.T) {
	db := &DB{}
	db.sessionErrors = newSessionErrorQueue(db)
	test.Cleanup(db.sessionErrors.cancel)
	event := SessionErrorEvent{CreatedAt: time.Now()}
	require.False(test, db.EnqueueSessionError(event))
	event.Identity = SessionErrorIdentity{Key: strings.Repeat("a", 64), UserID: "17", SessionID: "root"}
	for index := 0; index < 512; index++ {
		require.True(test, db.EnqueueSessionError(event))
	}
	require.False(test, db.EnqueueSessionError(event))
	require.Equal(test, uint64(1), db.SessionErrorCollectorStats().Dropped)
	require.Equal(test, int64(512), db.SessionErrorCollectorStats().Pending)
}

func TestSessionBlacklistCycleFailsClosedWithoutHidingOtherRows(test *testing.T) {
	db, err := New("sqlite", filepath.Join(test.TempDir(), "cycle.db"))
	require.NoError(test, err)
	test.Cleanup(func() { require.NoError(test, db.Close()) })
	first, second, normal := strings.Repeat("4", 64), strings.Repeat("5", 64), strings.Repeat("6", 64)
	for _, key := range []string{first, normal} {
		require.NoError(test, db.insertSessionErrors(context.Background(), []SessionErrorEvent{{Identity: SessionErrorIdentity{Key: key, UserID: "17", SessionID: key}, CreatedAt: time.Now()}}))
	}
	require.NoError(test, db.RecordSessionParent(context.Background(), first, second))
	require.NoError(test, db.RecordSessionParent(context.Background(), second, first))
	_, err = db.SessionBlacklistStatus(context.Background(), first, "")
	require.ErrorIs(test, err, ErrSessionLineageConflict)
	page, err := db.ListSessionErrors(context.Background(), SessionErrorQuery{})
	require.NoError(test, err)
	require.Len(test, page.Items, 2)
	for _, row := range page.Items {
		require.Equal(test, row.Identity.Key == first, row.LineageInvalid)
		require.Equal(test, row.Identity.Key == first, row.Locked)
	}
	for _, state := range []string{"unlocked", "locked"} {
		filtered, err := db.ListSessionErrors(context.Background(), SessionErrorQuery{LockState: state})
		require.NoError(test, err)
		require.Equal(test, int64(1), filtered.Groups)
		require.Len(test, filtered.Items, 1)
		require.Equal(test, state == "locked", filtered.Items[0].LineageInvalid)
	}
}

func BenchmarkSessionBlacklistCachedLookup(benchmark *testing.B) {
	db, err := New("sqlite", filepath.Join(benchmark.TempDir(), "cache.db"))
	if err != nil {
		benchmark.Fatal(err)
	}
	benchmark.Cleanup(func() { _ = db.Close() })
	key := strings.Repeat("7", 64)
	ctx := context.Background()
	if _, err := db.SessionBlacklistStatus(ctx, key, ""); err != nil {
		benchmark.Fatal(err)
	}
	benchmark.ReportAllocs()
	benchmark.ResetTimer()
	for index := 0; index < benchmark.N; index++ {
		if lockedBy, err := db.SessionBlacklistStatus(ctx, key, ""); err != nil || lockedBy != "" {
			benchmark.Fatalf("unexpected blacklist result: %q, %v", lockedBy, err)
		}
	}
}

func TestSessionErrorAccountsUseLatestRequestAndExposeOnlyLabels(test *testing.T) {
	db, err := New("sqlite", filepath.Join(test.TempDir(), "account-labels.db"))
	require.NoError(test, err)
	test.Cleanup(func() { require.NoError(test, db.Close()) })
	ctx := context.Background()
	older, err := db.InsertAccountWithCredentials(ctx, "older account", map[string]interface{}{"email": "old@example.com", "refresh_token": "old-private-token"}, "")
	require.NoError(test, err)
	latest, err := db.InsertAccountWithCredentials(ctx, "latest account", map[string]interface{}{"email": "latest@example.com", "refresh_token": "latest-private-token"}, "")
	require.NoError(test, err)
	now := time.Now()
	first := SessionErrorIdentity{Key: strings.Repeat("8", 64), UserID: "17", SessionID: "first"}
	second := SessionErrorIdentity{Key: strings.Repeat("9", 64), UserID: "17", SessionID: "second"}
	missing := SessionErrorIdentity{Key: strings.Repeat("a", 64), UserID: "18", SessionID: "missing-account"}
	require.NoError(test, db.insertSessionErrors(ctx, []SessionErrorEvent{
		{Identity: first, CreatedAt: now.Add(-time.Minute), AccountID: older},
		{Identity: first, CreatedAt: now, AccountID: latest},
		{Identity: second, CreatedAt: now, AccountID: latest},
		{Identity: missing, CreatedAt: now, AccountID: latest + 1000},
	}))
	page, err := db.ListSessionErrors(ctx, SessionErrorQuery{})
	require.NoError(test, err)
	require.Len(test, page.Items, 3)
	for _, row := range page.Items {
		if row.Identity.Key == missing.Key {
			require.Empty(test, row.AccountName)
			require.Empty(test, row.AccountEmail)
			require.Equal(test, latest+1000, row.Latest.AccountID)
		} else {
			require.Equal(test, "latest account", row.AccountName)
			require.Equal(test, "latest@example.com", row.AccountEmail)
			require.Equal(test, latest, row.Latest.AccountID)
		}
	}
	payload, err := json.Marshal(page)
	require.NoError(test, err)
	require.NotContains(test, string(payload), "private-token")
	require.NotContains(test, string(payload), "refresh_token")
	require.NotContains(test, string(payload), "credentials")
	require.NoError(test, db.SetSessionBlacklist(ctx, []string{first.Key}, true))
	locked, err := db.ListSessionErrors(ctx, SessionErrorQuery{LockedOnly: true})
	require.NoError(test, err)
	require.Len(test, locked.Items, 1)
	require.Equal(test, "latest account", locked.Items[0].AccountName)
}

func TestSessionErrorLockFiltersApplyBeforeCountsAndPagination(test *testing.T) {
	db, err := New("sqlite", filepath.Join(test.TempDir(), "lock-filter.db"))
	require.NoError(test, err)
	test.Cleanup(func() { require.NoError(test, db.Close()) })
	ctx := context.Background()
	keys := []string{strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64), strings.Repeat("d", 64), strings.Repeat("e", 64)}
	now := time.Now()
	for index, key := range keys {
		userID := "17"
		if index == 4 {
			userID = "18"
		}
		require.NoError(test, db.insertSessionErrors(ctx, []SessionErrorEvent{{Identity: SessionErrorIdentity{Key: key, UserID: userID, SessionID: "shared-session"}, CreatedAt: now.Add(-time.Duration(index) * time.Second)}}))
	}
	require.NoError(test, db.RecordSessionParent(ctx, keys[1], keys[0]))
	require.NoError(test, db.RecordSessionParent(ctx, keys[2], keys[1]))
	require.NoError(test, db.SetSessionBlacklist(ctx, []string{keys[0]}, true))
	first, err := db.ListSessionErrors(ctx, SessionErrorQuery{LockState: "unlocked", Limit: 1})
	require.NoError(test, err)
	require.Equal(test, int64(2), first.Groups)
	require.Equal(test, int64(2), first.Errors)
	require.Len(test, first.Items, 1)
	require.Equal(test, keys[3], first.Items[0].Identity.Key)
	require.NotEmpty(test, first.NextCursor)
	second, err := db.ListSessionErrors(ctx, SessionErrorQuery{LockState: "unlocked", Limit: 1, Cursor: first.NextCursor})
	require.NoError(test, err)
	require.Len(test, second.Items, 1)
	require.Equal(test, keys[4], second.Items[0].Identity.Key)
	require.Empty(test, second.NextCursor)
	locked, err := db.ListSessionErrors(ctx, SessionErrorQuery{LockState: "locked"})
	require.NoError(test, err)
	require.Equal(test, int64(3), locked.Groups)
	require.Len(test, locked.Items, 3)
	for _, row := range locked.Items {
		require.True(test, row.Locked)
		require.Equal(test, keys[0], row.LockedBy)
	}
	isolated, err := db.ListSessionErrors(ctx, SessionErrorQuery{UserID: "18", SessionID: "shared-session", LockState: "locked"})
	require.NoError(test, err)
	require.Zero(test, isolated.Groups)
	require.Empty(test, isolated.Items)
	all, err := db.ListSessionErrors(ctx, SessionErrorQuery{LockState: "all"})
	require.NoError(test, err)
	require.Equal(test, int64(5), all.Groups)
	require.NoError(test, db.SetSessionBlacklist(ctx, []string{keys[0]}, false))
	unlocked, err := db.ListSessionErrors(ctx, SessionErrorQuery{LockState: "unlocked"})
	require.NoError(test, err)
	require.Equal(test, int64(5), unlocked.Groups)
	_, err = db.ListSessionErrors(ctx, SessionErrorQuery{LockState: "invalid"})
	require.Error(test, err)
}
