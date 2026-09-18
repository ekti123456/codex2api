package database

import (
	"fmt"
	"path/filepath"
	"sync"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestUsageTurnStartElectionAndRestart(t *testing.T) {
	path := filepath.Join(t.TempDir(), "turns.db")
	db, err := New("sqlite", path)
	require.NoError(t, err)
	started := time.Now().Add(time.Second)
	first, err := db.ObserveUsageTurnStart(t.Context(), "owner/thread/turn", "request-a", started, true)
	require.NoError(t, err)
	require.NotNil(t, first)
	require.True(t, *first)
	// A second process and a server restart must retain the same election.
	second, err := New("sqlite", path)
	require.NoError(t, err)
	t.Cleanup(func() { _ = second.Close() })
	for _, request := range []string{"request-a", "request-b"} {
		value, err := second.ObserveUsageTurnStart(t.Context(), "owner/thread/turn", request, started, true)
		require.NoError(t, err)
		require.Equal(t, request == "request-a", *value)
	}
	require.NoError(t, db.Close())
	value, err := second.ObserveUsageTurnStart(t.Context(), "other-owner/thread/turn", "request-b", started, true)
	require.NoError(t, err)
	require.True(t, *value)
	value, err = second.ObserveUsageTurnStart(t.Context(), "new-turn", "request-b", started, true)
	require.NoError(t, err)
	require.True(t, *value)
	value, err = second.ObserveUsageTurnStart(t.Context(), "pre-upgrade", "request-old", time.Now().Add(-time.Hour), true)
	require.NoError(t, err)
	require.Nil(t, value)
	for _, direct := range []bool{false, true} {
		value, err = second.ObserveUsageTurnStart(t.Context(), "observed-mid-turn", "request-later", started, direct)
		require.NoError(t, err)
		require.Nil(t, value)
	}
}

func TestUsageTurnStartConcurrentElection(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "concurrent.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	started := time.Now().Add(time.Second)
	var wg sync.WaitGroup
	results := make(chan bool, 12)
	for i := range 12 {
		wg.Go(func() {
			first, err := db.ObserveUsageTurnStart(t.Context(), "scope", fmt.Sprint(i), started, true)
			if err != nil || first == nil {
				t.Errorf("observe: first=%v err=%v", first, err)
				return
			}
			results <- *first
		})
	}
	wg.Wait()
	close(results)
	count := 0
	for first := range results {
		if first {
			count++
		}
	}
	require.Equal(t, 1, count)
}

func TestUsageTurnStartStorageListsAndExport(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "usage.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	first := true
	require.NoError(t, db.InsertUsageLog(t.Context(), &UsageLogInput{TurnID: "turn-a", IsTurnFirstRequest: &first, TurnPromptPreview: "请检查接口", RequestType: "user", StatusCode: 500}))
	first = false
	require.NoError(t, db.InsertUsageLog(t.Context(), &UsageLogInput{TurnID: "turn-a", IsTurnFirstRequest: &first, RequestType: "user", StatusCode: 500}))
	require.NoError(t, db.InsertUsageLog(t.Context(), &UsageLogInput{StatusCode: 500}))
	db.FlushUsageLogs()
	filter := UsageLogFilter{Start: time.Now().Add(-time.Hour), End: time.Now().Add(time.Hour), Page: 1, PageSize: 10}
	check := func(logs []*UsageLog) {
		require.Len(t, logs, 3)
		var yes, no, unknown int
		for _, log := range logs {
			if log.IsTurnFirstRequest == nil {
				unknown++
				continue
			}
			require.Equal(t, "turn-a", log.TurnID)
			if *log.IsTurnFirstRequest {
				yes++
				require.Equal(t, "请检查接口", log.TurnPromptPreview)
			} else {
				no++
				require.Empty(t, log.TurnPromptPreview)
			}
		}
		require.Equal(t, []int{1, 1, 1}, []int{yes, no, unknown})
	}
	logs, err := db.ListRecentUsageLogs(t.Context(), 10)
	require.NoError(t, err)
	check(logs)
	logs, err = db.ListUsageLogsByTimeRange(t.Context(), filter.Start, filter.End)
	require.NoError(t, err)
	check(logs)
	page, err := db.ListUsageLogsByTimeRangePaged(t.Context(), filter)
	require.NoError(t, err)
	check(page.Logs)
	var exported []*UsageLog
	require.NoError(t, db.WalkUsageLogsForExport(t.Context(), &filter, func(entry *UsageLogExportEntry) error { exported = append(exported, entry.UsageLog); return nil }))
	check(exported)
}
