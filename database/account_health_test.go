package database

import (
	"context"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestAccountHealthBucketsExcludeInternalCapabilityProbe(t *testing.T) {
	db := newGrokStateTestDB(t)
	ctx := context.Background()
	now := time.Now().UTC().Truncate(time.Second)
	insert := func(statusCode int, reason string) {
		t.Helper()
		if _, err := db.conn.ExecContext(ctx, `INSERT INTO usage_logs
			(account_id, status_code, internal_reason, created_at)
			VALUES (1, $1, $2, $3)`, statusCode, reason, sqliteTimeParam(now.Add(-time.Minute))); err != nil {
			t.Fatalf("insert usage log: %v", err)
		}
	}
	insert(200, "")
	insert(200, "grok_capability_probe")
	insert(500, "grok_capability_probe")

	buckets, err := db.GetAccountsHealthBuckets(ctx, now, 20, 10*time.Minute)
	if err != nil {
		t.Fatalf("GetAccountsHealthBuckets: %v", err)
	}
	var success, failed int
	for _, bucket := range buckets[1] {
		success += bucket.Success
		failed += bucket.Failed
	}
	if success != 1 || failed != 0 {
		t.Fatalf("health buckets = success %d failed %d, want 1/0 with probe rows excluded", success, failed)
	}
}

func TestAccountHealthOverloadCountsMatchWindowAndExactCode(test *testing.T) {
	db := newGrokStateTestDB(test)
	ctx := context.Background()
	now := time.Date(2026, 9, 11, 12, 0, 0, 0, time.UTC)
	start := now.Add(-200 * time.Minute)
	insert := func(accountID int64, createdAt time.Time, status int, message any, internal string, retry bool) {
		test.Helper()
		_, err := db.conn.ExecContext(ctx, `INSERT INTO usage_logs
			(account_id, status_code, error_message, internal_reason, is_retry_attempt, created_at)
			VALUES ($1, $2, $3, $4, $5, $6)`, accountID, status, message, internal, retry, sqliteTimeParam(createdAt))
		require.NoError(test, err)
	}
	insert(1, start, 500, "server_is_overloaded", "", false)
	insert(1, start.Add(10*time.Minute), 500, "server_is_overloaded · service_unavailable_error · busy", "", false)
	insert(1, now.Add(-time.Minute), 500, "  server_is_overloaded · upstream overloaded  ", "", true)
	insert(1, now, 500, "server_is_overloaded", "", false)
	insert(1, now.Add(-time.Minute), 200, "", "", false)
	insert(1, now.Add(-time.Minute), 200, "server_is_overloaded", "", false)
	insert(1, now.Add(-time.Minute), 500, "other_error · mentions server_is_overloaded", "", false)
	insert(1, now.Add(-time.Minute), 500, "server_is_overloaded_extra · busy", "", false)
	insert(1, now.Add(-time.Minute), 500, "serverXisYoverloaded · busy", "", false)
	insert(1, now.Add(-time.Minute), 500, nil, "", false)
	insert(1, now.Add(-time.Minute), 429, "server_is_overloaded", "", false)
	insert(1, now.Add(-time.Minute), 503, "server_is_overloaded", "", false)
	insert(1, now.Add(-time.Minute), 499, "server_is_overloaded", "", false)
	insert(1, now.Add(-time.Minute), 500, "server_is_overloaded", "grok_capability_probe", false)
	insert(1, start.Add(-time.Second), 500, "server_is_overloaded", "", false)
	insert(1, now.Add(time.Second), 500, "server_is_overloaded", "", false)
	insert(2, now.Add(-time.Minute), 500, "server_is_overloaded", "", false)
	insert(0, now.Add(-time.Minute), 500, "server_is_overloaded", "", false)

	buckets, err := db.GetAccountsHealthBucketsByIDs(ctx, []int64{1, 1, -1}, now, 20, 10*time.Minute)
	require.NoError(test, err)
	require.Len(test, buckets, 1)
	require.Len(test, buckets[1], 20)
	require.Equal(test, 1, buckets[1][0].Overloaded500)
	require.Equal(test, 1, buckets[1][1].Overloaded500)
	require.Equal(test, 2, buckets[1][19].Overloaded500)
	var success, failed, overloaded int
	for _, bucket := range buckets[1] {
		success += bucket.Success
		failed += bucket.Failed
		overloaded += bucket.Overloaded500
	}
	require.Equal(test, 2, success)
	require.Equal(test, 10, failed)
	require.Equal(test, 4, overloaded)

	all, err := db.GetAccountsHealthBuckets(ctx, now, 20, 10*time.Minute)
	require.NoError(test, err)
	require.Equal(test, 1, all[2][19].Overloaded500)
	require.NotContains(test, all, int64(0))

	expired, err := db.GetAccountsHealthBucketsByIDs(ctx, []int64{1}, now.Add(201*time.Minute), 20, 10*time.Minute)
	require.NoError(test, err)
	require.Empty(test, expired)
}
