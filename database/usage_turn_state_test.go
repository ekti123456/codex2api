package database

import (
	"path/filepath"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestUsageTurnStateStorageFiltersAndExport(t *testing.T) {
	db, err := New("sqlite", filepath.Join(t.TempDir(), "turn-state.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	length, decoded := 292, 217
	for _, value := range []int{292, 217, 0, -1, 292} {
		input := &UsageLogInput{Endpoint: "/v1/responses", Model: "test", StatusCode: 500}
		if value >= 0 {
			length = value
			input.TurnStateLength = &length
		}
		if value == 292 {
			input.TurnStateDecodedBytes = &decoded
		}
		require.NoError(t, db.InsertUsageLog(t.Context(), input))
	}
	// Pending async rows must retain a value snapshot, not these pointers.
	length, decoded = 999, 999
	db.FlushUsageLogs()
	filter := UsageLogFilter{Start: time.Now().Add(-time.Hour), End: time.Now().Add(time.Hour), Page: 1, PageSize: 1}
	for _, state := range []struct {
		name  string
		count int64
	}{{"", 5}, {"received", 3}, {"missing", 1}, {"not_recorded", 1}} {
		filter.TurnState = state.name
		page, err := db.ListUsageLogsByTimeRangePaged(t.Context(), filter)
		require.NoError(t, err)
		require.Equal(t, state.count, page.Total)
		require.Len(t, page.Logs, 1)
	}
	length = 292
	filter.TurnState, filter.TurnStateLength = "received", &length
	page, err := db.ListUsageLogsByTimeRangePaged(t.Context(), filter)
	require.NoError(t, err)
	require.EqualValues(t, 2, page.Total)
	require.Equal(t, 217, *page.Logs[0].TurnStateDecodedBytes)
	summary, err := db.GetUsageErrorSummary(t.Context(), filter)
	require.NoError(t, err)
	require.EqualValues(t, 2, summary.TotalErrors)
	count := 0
	require.NoError(t, db.WalkUsageLogsForExport(t.Context(), &filter, func(entry *UsageLogExportEntry) error {
		count++
		require.Equal(t, 292, *entry.TurnStateLength)
		require.Equal(t, 217, *entry.TurnStateDecodedBytes)
		return nil
	}))
	require.Equal(t, 2, count)
	for _, list := range []func() ([]*UsageLog, error){
		func() ([]*UsageLog, error) { return db.ListRecentUsageLogs(t.Context(), 20) },
		func() ([]*UsageLog, error) { return db.ListUsageLogsByTimeRange(t.Context(), filter.Start, filter.End) },
	} {
		logs, err := list()
		require.NoError(t, err)
		require.Len(t, logs, 5)
		var received, missing, unknown int
		for _, log := range logs {
			if log.TurnStateLength == nil {
				unknown++
			} else if *log.TurnStateLength == 0 {
				missing++
			} else {
				received++
			}
		}
		require.Equal(t, []int{3, 1, 1}, []int{received, missing, unknown})
	}
}
