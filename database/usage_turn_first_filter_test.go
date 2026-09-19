package database

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestUsageTurnFirstFilterKeepsPaginationSummaryAndExportConsistent(t *testing.T) {
	db := newGrokStateTestDB(t)
	first, later := true, false
	for _, row := range []struct {
		first *bool
		retry bool
	}{{&first, false}, {&later, false}, {&later, true}, {&first, true}, {nil, false}} {
		require.NoError(t, db.InsertUsageLog(t.Context(), &UsageLogInput{
			Endpoint: "/v1/responses", Model: "turn-filter", StatusCode: 500,
			RequestType: "user", IsTurnFirstRequest: row.first, IsRetryAttempt: row.retry,
		}))
	}
	db.FlushUsageLogs()
	waitUsageLogExportFixtureRows(t, db, 5)
	filter := UsageLogFilter{Start: time.Now().Add(-time.Hour), End: time.Now().Add(time.Hour), Page: 1, PageSize: 1}
	for _, scenario := range []struct {
		turnFirst string
		count     int64
	}{{"", 5}, {"true", 2}, {"false", 2}, {"unknown", 1}} {
		t.Run("turn="+scenario.turnFirst, func(t *testing.T) {
			filter.TurnFirst = scenario.turnFirst
			page, err := db.ListUsageLogsByTimeRangePaged(t.Context(), filter)
			require.NoError(t, err)
			require.Equal(t, scenario.count, page.Total)
			require.Len(t, page.Logs, 1)
			summary, err := db.GetUsageErrorSummary(t.Context(), filter)
			require.NoError(t, err)
			require.Equal(t, scenario.count, summary.TotalErrors)
			count := int64(0)
			require.NoError(t, db.WalkUsageLogsForExport(t.Context(), &filter, func(row *UsageLogExportEntry) error {
				count++
				if scenario.turnFirst == "unknown" {
					require.Nil(t, row.IsTurnFirstRequest)
				} else if scenario.turnFirst != "" {
					require.NotNil(t, row.IsTurnFirstRequest)
					require.Equal(t, scenario.turnFirst == "true", *row.IsTurnFirstRequest)
				}
				return nil
			}))
			require.Equal(t, scenario.count, count)
		})
	}
	// A turn's first request can fail and trigger retry; these are separate axes.
	filter.TurnFirst = "true"
	for _, retry := range []bool{false, true} {
		filter.RetryOnly = &retry
		page, err := db.ListUsageLogsByTimeRangePaged(t.Context(), filter)
		require.NoError(t, err)
		require.EqualValues(t, 1, page.Total)
		require.Equal(t, retry, page.Logs[0].IsRetryAttempt)
		require.True(t, *page.Logs[0].IsTurnFirstRequest)
	}
}
