package database

import (
	"context"
	"fmt"
	"path/filepath"
	"strings"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestSessionActivityLifecyclePersistenceAndIsolation(t *testing.T) {
	path := filepath.Join(t.TempDir(), "activity.db")
	db, err := New("sqlite", path)
	require.NoError(t, err)
	closed := false
	t.Cleanup(func() {
		if !closed {
			require.NoError(t, db.Close())
		}
	})
	ctx, now := context.Background(), time.Now().UTC().Truncate(time.Millisecond)
	key, other, untracked := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	for _, k := range []string{key, other} {
		require.NoError(t, db.insertSessionErrors(ctx, []SessionErrorEvent{{Identity: SessionErrorIdentity{Key: k, UserID: k, SessionID: "same-client-session"}, CreatedAt: now.Add(-time.Minute)}}))
	}
	get := func(at time.Time) SessionActivityPage {
		page, err := db.SessionActivities(ctx, []string{key, other, untracked}, at)
		require.NoError(t, err)
		require.Len(t, page.Items, 3)
		return page
	}
	require.Equal(t, "unknown", get(now).Items[key].State)
	first, second := db.BeginSessionActivity(key, false, now), db.BeginSessionActivity(key, false, now)
	aux := db.BeginSessionActivity(key, true, now)
	page := get(now.Add(time.Hour)) // Long-running request does not become idle.
	require.Equal(t, "running", page.Items[key].State)
	require.Equal(t, 2, page.Items[key].ActiveRequests)
	require.Equal(t, 1, page.Items[key].AuxiliaryRequests)
	require.Equal(t, "unknown", page.Items[other].State)
	first.Finish(false, now)
	first.Finish(true, now) // Cleanup is idempotent and cannot invent success.
	require.False(t, get(now).Items[key].Recovered)
	second.Finish(true, now.Add(time.Second))
	require.Equal(t, "auxiliary", get(now.Add(time.Second)).Items[key].State)
	require.True(t, get(now.Add(time.Second)).Items[key].Recovered)
	aux.Finish(true, now.Add(2*time.Second))
	require.Equal(t, "recent", get(now.Add(30 * time.Minute)).Items[key].State)
	require.Equal(t, "idle", get(now.Add(31 * time.Minute)).Items[key].State)
	// Unseen/non-overloaded sessions never expand the persistent table.
	db.BeginSessionActivity(untracked, false, now).Finish(true, now)
	_, err = db.flushSessionActivity(ctx, now, true)
	require.NoError(t, err)
	var count int
	require.NoError(t, db.conn.QueryRow(`SELECT COUNT(*) FROM session_activity`).Scan(&count))
	require.Equal(t, 1, count)
	// A live counter deliberately does not survive closing/reopening the DB.
	db.BeginSessionActivity(key, false, now.Add(3*time.Second))
	require.NoError(t, db.Close())
	closed = true
	db, err = New("sqlite", path)
	require.NoError(t, err)
	closed = false
	page = get(now.Add(4 * time.Second))
	require.Zero(t, page.Items[key].ActiveRequests)
	require.Equal(t, "recent", page.Items[key].State)
	require.NotNil(t, page.Items[key].LastSuccessAt)
	require.True(t, page.Items[key].Recovered)
	// Auxiliary success cannot replace the last main-request success.
	previous := *page.Items[key].LastSuccessAt
	db.BeginSessionActivity(key, true, now.Add(5*time.Second)).Finish(true, now.Add(6*time.Second))
	require.Equal(t, previous, *get(now.Add(7 * time.Second)).Items[key].LastSuccessAt)
	require.NoError(t, db.pruneSessionErrors(ctx, now.Add(8*24*time.Hour)))
	require.NoError(t, db.conn.QueryRow(`SELECT COUNT(*) FROM session_activity`).Scan(&count))
	require.Zero(t, count)
}

func TestSessionActivityFailedFlushRetainsLatestAndBoundsRequests(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "flush.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	ctx, now, key := context.Background(), time.Now(), strings.Repeat("a", 64)
	require.NoError(t, db.insertSessionErrors(ctx, []SessionErrorEvent{{Identity: SessionErrorIdentity{Key: key, UserID: "17"}, CreatedAt: now.Add(-time.Minute)}}))
	lease := db.BeginSessionActivity(key, false, now)
	count, err := db.flushSessionActivity(ctx, now, false)
	require.NoError(t, err)
	require.Zero(t, count)
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = db.flushSessionActivity(canceled, now, true)
	require.Error(t, err)
	lease.Finish(true, now.Add(time.Second))
	_, err = db.flushSessionActivity(ctx, now.Add(2*time.Second), true)
	require.NoError(t, err)
	var success int64
	require.NoError(t, db.conn.QueryRow(`SELECT last_success_at FROM session_activity WHERE session_key=$1`, key).Scan(&success))
	require.Equal(t, now.Add(time.Second).UnixMilli(), success)
	for _, keys := range [][]string{nil, {key, key}, {"invalid"}, make([]string, 101)} {
		_, err := db.SessionActivities(ctx, keys, now)
		require.Error(t, err)
	}
}

func TestSessionActivityCacheBoundAndConcurrentLeases(t *testing.T) {
	db := &DB{sessionActivity: newSessionActivityTracker(2)}
	now := time.Now()
	a, b, c := strings.Repeat("a", 64), strings.Repeat("b", 64), strings.Repeat("c", 64)
	first := db.BeginSessionActivity(a, false, now)
	db.BeginSessionActivity(b, false, now)
	require.Nil(t, db.BeginSessionActivity(c, false, now))
	require.Len(t, db.sessionActivity.entries, 2)
	require.True(t, db.sessionActivity.limited)
	var wg sync.WaitGroup
	for i := 0; i < 100; i++ {
		wg.Add(1)
		go func() {
			defer wg.Done()
			lease := db.BeginSessionActivity(a, false, now)
			lease.Finish(true, now)
			lease.Finish(true, now)
		}()
	}
	wg.Wait()
	require.Equal(t, 1, db.sessionActivity.entries[a].active)
	first.Finish(false, now)
	require.Zero(t, db.sessionActivity.entries[a].active)
}

func TestSessionActivityFirstOverloadAfterActivityFlush(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "first-overload.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	key, now, ctx := strings.Repeat("a", 64), time.Now(), context.Background()
	db.BeginSessionActivity(key, false, now).Finish(false, now)
	_, err = db.flushSessionActivity(ctx, now, true)
	require.NoError(t, err)
	require.NoError(t, db.insertSessionErrors(ctx, []SessionErrorEvent{{Identity: SessionErrorIdentity{Key: key, UserID: "17"}, CreatedAt: now}}))
	count, err := db.flushSessionActivity(ctx, now, true)
	require.NoError(t, err)
	require.Equal(t, 1, count)
	var active int64
	require.NoError(t, db.conn.QueryRow(`SELECT last_active_at FROM session_activity WHERE session_key=$1`, key).Scan(&active))
	require.Equal(t, now.UnixMilli(), active)
}

func BenchmarkSessionActivityCurrentPage(b *testing.B) {
	for _, total := range []int{100, 100000} {
		b.Run(fmt.Sprint(total), func(b *testing.B) {
			db, err := New("sqlite", filepath.Join(b.TempDir(), "page.db"))
			if err != nil {
				b.Fatal(err)
			}
			b.Cleanup(func() { db.Close() })
			ctx, now := context.Background(), time.Now()
			_, err = db.conn.Exec(`WITH RECURSIVE n(x) AS (SELECT 1 UNION ALL SELECT x+1 FROM n WHERE x<$1)
			INSERT INTO session_error_stats(session_key,user_id,session_id,first_at,last_at,error_count,identity_data,latest_data)
			SELECT printf('%064x',x),'17','root',$2,$2,1,'{}','{}' FROM n`, total, now.UnixMilli())
			if err != nil {
				b.Fatal(err)
			}
			keys := make([]string, 20)
			for i := range keys {
				keys[i] = fmt.Sprintf("%064x", i+1)
				db.BeginSessionActivity(keys[i], false, now).Finish(true, now)
			}
			b.ReportAllocs()
			b.ResetTimer()
			for i := 0; i < b.N; i++ {
				page, err := db.SessionActivities(ctx, keys, now)
				if err != nil || len(page.Items) != 20 {
					b.Fatalf("page: %v", err)
				}
			}
		})
	}
}

func BenchmarkSessionActivityRequestPath(b *testing.B) {
	db := &DB{sessionActivity: newSessionActivityTracker(sessionActivityCapacity)}
	key, now := strings.Repeat("a", 64), time.Now()
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		db.BeginSessionActivity(key, false, now).Finish(true, now)
	}
}
