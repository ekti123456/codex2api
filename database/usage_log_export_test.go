package database

import (
	"context"
	"errors"
	"fmt"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestUsageLogExportAllRetainedAndFilteredWithoutPageLimit(test *testing.T) {
	db := newGrokStateTestDB(test)
	now := time.Now().UTC()
	for index := 0; index < 503; index++ {
		require.NoError(test, db.InsertUsageLog(test.Context(), &UsageLogInput{
			Endpoint: "/v1/responses", Model: "export-model", RequestID: fmt.Sprintf("export-%d", index),
			RequestType: "compaction", SessionIDPrefix: "01a09012", Channel: "codex", StatusCode: 500,
			ViaWebsocket: true, NewAPIUserName: "export-user", RequestDiagnostics: `{"version":1,"upstream":{"send_phase":"after_payload"}}`,
		}))
	}
	db.FlushUsageLogs()
	for _, row := range []struct {
		status   int
		internal string
		at       time.Time
	}{
		{200, "", now.Add(-365 * 24 * time.Hour)},
		{499, "", now},
		{200, "grok_capability_probe", now},
	} {
		_, err := db.conn.ExecContext(test.Context(), `INSERT INTO usage_logs
			(status_code, channel, internal_reason, request_diagnostics, created_at)
			VALUES ($1, 'grok', $2, '', $3)`, row.status, row.internal, sqliteTimeParam(row.at))
		require.NoError(test, err)
	}
	filter := UsageLogFilter{Start: now.Add(-time.Hour), End: now.Add(time.Hour), Page: 2, PageSize: 1,
		RequestType: "compaction", Query: "01a09012", Channel: "codex", StatusCode: 500, Model: "export-model"}
	seen := make(map[int64]bool)
	var previous *UsageLogExportEntry
	err := db.WalkUsageLogsForExport(test.Context(), &filter, func(entry *UsageLogExportEntry) error {
		require.False(test, seen[entry.ID])
		if previous != nil {
			require.NotSame(test, previous, entry)
		}
		previous = entry
		seen[entry.ID] = true
		require.Equal(test, "export-user", entry.NewAPIUserName)
		require.JSONEq(test, `{"version":1,"upstream":{"send_phase":"after_payload"}}`, string(entry.Diagnostics))
		return nil
	})
	require.NoError(test, err)
	require.Len(test, seen, 503)
	allCount := 0
	err = db.WalkUsageLogsForExport(test.Context(), nil, func(entry *UsageLogExportEntry) error {
		allCount++
		return nil
	})
	require.NoError(test, err)
	require.Equal(test, 506, allCount)
	filter.RequestID = "not-present"
	err = db.WalkUsageLogsForExport(test.Context(), &filter, func(entry *UsageLogExportEntry) error {
		test.Fatal("empty filter exported a row")
		return nil
	})
	require.NoError(test, err)
}

func TestUsageLogExportPropagatesCancellationAndConsumerFailure(test *testing.T) {
	db := newGrokStateTestDB(test)
	require.NoError(test, db.InsertUsageLog(test.Context(), &UsageLogInput{StatusCode: 200, Endpoint: "/v1/responses"}))
	db.FlushUsageLogs()
	consumerError := errors.New("disk full")
	err := db.WalkUsageLogsForExport(test.Context(), nil, func(*UsageLogExportEntry) error { return consumerError })
	require.ErrorIs(test, err, consumerError)
	ctx, cancel := context.WithCancel(test.Context())
	cancel()
	err = db.WalkUsageLogsForExport(ctx, nil, func(*UsageLogExportEntry) error { return nil })
	require.ErrorIs(test, err, context.Canceled)
	require.JSONEq(test, `{"capture_status":"invalid_stored_json"}`, string(usageExportDiagnosticJSON("{bad")))
	require.Nil(test, usageExportDiagnosticJSON(""))
}
