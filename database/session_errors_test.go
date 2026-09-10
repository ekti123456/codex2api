package database

import (
	"context"
	"database/sql"
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
