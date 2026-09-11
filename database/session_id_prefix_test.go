package database

import (
	"path/filepath"
	"testing"
	"time"
)

func TestUsageSessionIDPrefixSearchAcrossRequestTypes(test *testing.T) {
	db, err := New("sqlite", filepath.Join(test.TempDir(), "session-prefix.db"))
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = db.Close() })
	for _, input := range []UsageLogInput{
		{Model: "main", RequestType: "user", SessionIDPrefix: "01a09012", StatusCode: 200},
		{Model: "title", RequestType: "related_internal", SessionIDPrefix: "01A09012", StatusCode: 500},
		{Model: "compact", RequestType: "compaction", SessionIDPrefix: "01a09012", StatusCode: 429},
		{Model: "independent", RequestType: "independent_internal", SessionIDPrefix: "01a0901a", StatusCode: 200},
		{Model: "historical", RequestDiagnostics: `{"incoming":{"client_metadata":{"session_id":"01a09012-b9de-7b40-a04b-612ef4dc3d7d"}}}`, StatusCode: 200},
		{Model: "invalid", SessionIDPrefix: "01a09012-unbounded", StatusCode: 200},
	} {
		input.Endpoint = "/v1/responses"
		if err := db.InsertUsageLog(test.Context(), &input); err != nil {
			test.Fatal(err)
		}
	}
	db.FlushUsageLogs()
	filter := UsageLogFilter{Start: time.Now().Add(-time.Hour), End: time.Now().Add(time.Hour), Query: "01A09012", PageSize: 2}
	page, err := db.ListUsageLogsByTimeRangePaged(test.Context(), filter)
	if err != nil || page.Total != 3 || len(page.Logs) != 2 {
		test.Fatalf("filtered page = %+v, err = %v", page, err)
	}
	for _, entry := range page.Logs {
		if entry.SessionIDPrefix != "01a09012" {
			test.Fatalf("wrong prefix in results: %+v", entry)
		}
	}
	summary, err := db.GetUsageErrorSummary(test.Context(), filter)
	if err != nil || summary.TotalErrors != 2 {
		test.Fatalf("filtered error summary = %+v, err = %v", summary, err)
	}
	filter.Model = "title"
	page, err = db.ListUsageLogsByTimeRangePaged(test.Context(), filter)
	if err != nil || page.Total != 1 || len(page.Logs) != 1 || page.Logs[0].Model != "title" {
		test.Fatalf("combined filters = %+v, err = %v", page, err)
	}
	logs, err := db.ListRecentUsageLogs(test.Context(), 20)
	if err != nil || len(logs) != 6 {
		test.Fatalf("all logs = %+v, err = %v", logs, err)
	}
	for _, entry := range logs {
		if (entry.Model == "historical" || entry.Model == "invalid") && entry.SessionIDPrefix != "" {
			test.Fatalf("unexpected inferred or invalid prefix: %+v", entry)
		}
	}
}
