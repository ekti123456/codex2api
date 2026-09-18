package database

import (
	"context"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAccountHealthLatestRequests(t *testing.T) {
	db := newGrokStateTestDB(t)
	now := time.Now().UTC().Truncate(time.Second)
	insert := func(account int64, at time.Time, status int, length any, reason string) {
		t.Helper()
		_, err := db.conn.ExecContext(t.Context(), `INSERT INTO usage_logs
			(account_id, created_at, status_code, turn_state_length, internal_reason)
			VALUES ($1,$2,$3,$4,$5)`, account, db.timeArg(at), status, length, reason)
		require.NoError(t, err)
	}
	insert(1, now.Add(-time.Minute), 200, 292, "")
	insert(1, now.Add(-time.Minute), 500, 0, "")  // Same timestamp: newest ID wins, even without a value.
	insert(1, now.Add(time.Second), 200, 999, "") // Not yet in this snapshot.
	insert(2, now.Add(-time.Hour), 200, 292, "")
	insert(2, now.Add(-time.Minute), 200, nil, "")     // Do not skip an unrecorded value.
	insert(3, now.Add(-500*time.Minute), 499, 217, "") // Latest request may be outside the bar window.
	insert(3, now.Add(-time.Second), 200, 999, "grok_capability_probe")
	insert(4, now.Add(-time.Second), 200, 292, "continuation")
	insert(5, now.Add(-time.Second), 200, 292, "") // Outside the requested page.
	ids := []int64{1, 2, 3, 4, 6, 1, -1, 0}
	latest, err := db.GetAccountHealthLatestRequests(t.Context(), ids, now)
	require.NoError(t, err)
	require.Len(t, latest, 3)
	require.NotNil(t, latest[1].TurnStateLength)
	require.Zero(t, *latest[1].TurnStateLength)
	require.True(t, latest[1].CreatedAt.Equal(now.Add(-time.Minute)))
	require.Nil(t, latest[2].TurnStateLength)
	require.Equal(t, 217, *latest[3].TurnStateLength)

	query, args := db.accountHealthLatestQuery([]int64{1, 2, 3}, now)
	plan, err := db.conn.QueryContext(t.Context(), "EXPLAIN QUERY PLAN "+query, args...)
	require.NoError(t, err)
	defer plan.Close()
	var steps []string
	for plan.Next() {
		var id, parent, unused int
		var detail string
		require.NoError(t, plan.Scan(&id, &parent, &unused, &detail))
		steps = append(steps, detail)
	}
	require.NoError(t, plan.Err())
	text := strings.Join(steps, "\n")
	require.Contains(t, text, "idx_usage_logs_account_created_at")
	require.NotContains(t, text, "SCAN usage_logs")
	t.Log(text)
}

func TestAccountHealthLatestRequestsNeverExpandsToFullPool(t *testing.T) {
	// A nil DB proves empty or oversized selections issue no SQL at all.
	var db *DB
	for _, ids := range [][]int64{nil, {}, {-1, 0}, func() []int64 {
		ids := make([]int64, accountRequestCountBreakdownMaxIDs+1)
		for i := range ids {
			ids[i] = int64(i + 1)
		}
		return ids
	}()} {
		result, err := db.GetAccountHealthLatestRequests(t.Context(), ids, time.Now())
		require.NoError(t, err)
		require.Empty(t, result)
	}
}

func BenchmarkAccountHealthLatestRequestsPage(b *testing.B) {
	db, err := New("sqlite", filepath.Join(b.TempDir(), "latest.db"))
	if err != nil {
		b.Fatal(err)
	}
	b.Cleanup(func() { _ = db.Close() })
	now := time.Now().UTC()
	_, err = db.conn.ExecContext(context.Background(), `WITH RECURSIVE seq(n) AS (
		SELECT 1 UNION ALL SELECT n+1 FROM seq WHERE n < 100000
	) INSERT INTO usage_logs (account_id, created_at, status_code, turn_state_length)
	SELECT (n % 1000)+1, $1, 200, 292 FROM seq`, db.timeArg(now.Add(-time.Minute)))
	if err != nil {
		b.Fatal(err)
	}
	ids := make([]int64, 20)
	for i := range ids {
		ids[i] = int64(i + 1)
	}
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		rows, err := db.GetAccountHealthLatestRequests(context.Background(), ids, now)
		if err != nil || len(rows) != len(ids) {
			b.Fatalf("rows=%d, err=%v", len(rows), err)
		}
	}
}
