package database

import (
	"path/filepath"
	"testing"
	"time"
)

func TestUsageRequestTypeFilters(test *testing.T) {
	db, err := New("sqlite", filepath.Join(test.TempDir(), "request-types.db"))
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = db.Close() })
	types := []string{"user", "related_internal", "independent_internal", "related_unclassified", "compaction", "gateway_internal", "unknown"}
	for _, requestType := range types {
		if err := db.InsertUsageLog(test.Context(), &UsageLogInput{Endpoint: "/v1/responses", Model: "test", StatusCode: 500, RequestType: requestType, SessionIDPrefix: "01a09012"}); err != nil {
			test.Fatal(err)
		}
	}
	if err := db.InsertUsageLog(test.Context(), &UsageLogInput{Endpoint: "/v1/responses", Model: "second", StatusCode: 200, RequestType: "related_internal", SessionIDPrefix: "01a0901a"}); err != nil {
		test.Fatal(err)
	}
	db.FlushUsageLogs()
	if _, err := db.conn.ExecContext(test.Context(), `INSERT INTO usage_logs (endpoint, model, status_code, request_type) VALUES ('/v1/responses', 'legacy-null', 200, NULL), ('/v1/responses', 'legacy-empty', 200, '')`); err != nil {
		test.Fatal(err)
	}
	filter := UsageLogFilter{Start: time.Now().Add(-time.Hour), End: time.Now().Add(time.Hour), Page: 1, PageSize: 20}
	for _, requestType := range append(types, "not_recorded", "") {
		test.Run(requestType, func(test *testing.T) {
			filter.RequestType = requestType
			page, err := db.ListUsageLogsByTimeRangePaged(test.Context(), filter)
			want := 1
			switch requestType {
			case "related_internal", "not_recorded":
				want = 2
			case "":
				want = 10
			}
			if err != nil || page.Total != int64(want) || len(page.Logs) != want {
				test.Fatalf("page=%+v err=%v, want %d", page, err, want)
			}
			for _, entry := range page.Logs {
				if requestType == "not_recorded" && entry.RequestType != "" || requestType != "" && requestType != "not_recorded" && entry.RequestType != requestType {
					test.Fatalf("unexpected classification in filtered result: %+v", entry)
				}
			}
		})
	}
	filter.RequestType, filter.Query, filter.PageSize = "related_internal", "01a09012", 1
	page, err := db.ListUsageLogsByTimeRangePaged(test.Context(), filter)
	if err != nil || page.Total != 1 || len(page.Logs) != 1 || page.Logs[0].Model != "test" {
		test.Fatalf("combined filters: page=%+v err=%v", page, err)
	}
	summary, err := db.GetUsageErrorSummary(test.Context(), filter)
	if err != nil || summary.TotalErrors != 1 {
		test.Fatalf("filtered summary=%+v err=%v", summary, err)
	}
	filter.RequestType, filter.Query = "not_recorded", ""
	summary, err = db.GetUsageErrorSummary(test.Context(), filter)
	if err != nil || summary.TotalErrors != 0 {
		test.Fatalf("historical rows misclassified: summary=%+v err=%v", summary, err)
	}
}
