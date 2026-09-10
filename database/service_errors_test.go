package database

import (
	"context"
	"encoding/json"
	"fmt"
	"path/filepath"
	"strings"
	"testing"
	"time"
	"unicode/utf8"
)

func waitServiceErrorQueue(test *testing.T, db *DB) {
	test.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for db.ServiceErrorCollectorStats().Pending > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if stats := db.ServiceErrorCollectorStats(); stats.Pending != 0 {
		test.Fatalf("collector did not drain: %+v", stats)
	}
}

func TestServiceErrorsPersistencePaginationAndIsolation(test *testing.T) {
	path := filepath.Join(test.TempDir(), "service-errors.db")
	db, err := New("sqlite", path)
	if err != nil {
		test.Fatal(err)
	}
	now := time.Now().UTC().Truncate(time.Millisecond)
	for index := 0; index < 4; index++ {
		event := ServiceErrorEvent{ID: fmt.Sprintf("event-%d", index), CreatedAt: now, StatusCode: 429, Stage: "rate_limit", RequestID: "request-one", NewAPIRequestID: "newapi-one", Message: "concurrency exhausted"}
		if index == 3 {
			event.StatusCode, event.Stage = 400, "root_binding"
		}
		if !db.EnqueueServiceError(event) {
			test.Fatal("event rejected")
		}
	}
	if err := db.Close(); err != nil {
		test.Fatal(err)
	}
	db, err = New("sqlite", path)
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = db.Close() })
	filter := ServiceErrorFilter{Start: now.Add(-time.Hour), End: now.Add(time.Second), Limit: 2}
	first, err := db.ListServiceErrors(context.Background(), filter)
	if err != nil || len(first.Items) != 2 || first.Summary.Total != 4 || first.Summary.Status429 != 3 || first.NextCursor == "" || first.Items[0].ID != "event-3" {
		test.Fatalf("first page=%+v err=%v", first, err)
	}
	filter.Cursor = first.NextCursor
	second, err := db.ListServiceErrors(context.Background(), filter)
	if err != nil || len(second.Items) != 2 || second.NextCursor != "" || second.Items[0].ID != "event-1" || second.Summary.Total != 4 {
		test.Fatalf("second page=%+v err=%v", second, err)
	}
	filter.Cursor, filter.Status, filter.RequestID = "", "429", "newapi-one"
	filtered, err := db.ListServiceErrors(context.Background(), filter)
	if err != nil || filtered.Summary.Total != 3 {
		test.Fatalf("NewAPI request filter=%+v err=%v", filtered, err)
	}
	filter.RequestID, filter.Stage = "request-one", "root_binding"
	filtered, err = db.ListServiceErrors(context.Background(), filter)
	if err != nil || filtered.Summary.Total != 0 {
		test.Fatalf("combined filter=%+v err=%v", filtered, err)
	}
	filter.RequestID, filter.Stage, filter.Status = "' OR 1=1 --", "", ""
	filtered, err = db.ListServiceErrors(context.Background(), filter)
	if err != nil || filtered.Summary.Total != 0 {
		test.Fatalf("unsafe request filter=%+v err=%v", filtered, err)
	}
	for _, table := range []string{"usage_logs", "accounts"} {
		var count int
		if err := db.conn.QueryRow("SELECT COUNT(*) FROM " + table).Scan(&count); err != nil || count != 0 {
			test.Fatalf("service errors changed %s: count=%d err=%v", table, count, err)
		}
	}
}

func TestServiceErrorQueueBoundedAndImmutable(test *testing.T) {
	db := &DB{}
	db.serviceErrors = newServiceErrorQueue(db)
	defer db.serviceErrors.cancel()
	reasons := []string{"root_unresolved"}
	clientInfo := map[string]string{"headers.X-Codex-Installation-Id": "device-before-mutation"}
	event := ServiceErrorEvent{ID: "bounded", StatusCode: 429, Message: strings.Repeat("中文", 3000), CandidateRejections: reasons, ClientInfo: clientInfo}
	for index := 0; index < serviceErrorQueueCapacity; index++ {
		if !db.EnqueueServiceError(event) {
			test.Fatalf("queue full at %d", index)
		}
	}
	if db.EnqueueServiceError(event) {
		test.Fatal("queue exceeded capacity")
	}
	reasons[0] = "mutated"
	clientInfo["headers.X-Codex-Installation-Id"] = "mutated"
	job := <-db.serviceErrors.jobs
	if len(job.event.Message) > 2048 || !utf8.ValidString(job.event.Message) || job.event.CandidateRejections[0] != "root_unresolved" || job.event.ClientInfo["headers.X-Codex-Installation-Id"] != "device-before-mutation" || !json.Valid([]byte(job.payload)) {
		test.Fatalf("unbounded or mutable event: %+v", job.event)
	}
	if stats := db.ServiceErrorCollectorStats(); stats.Pending != serviceErrorQueueCapacity || stats.Dropped != 1 {
		test.Fatalf("unexpected collector counters: %+v", stats)
	}
}

func TestServiceErrorRetentionAndRowLimit(test *testing.T) {
	db := newProxyTestDB(test)
	now := time.Now().UTC()
	for index := 0; index < 5; index++ {
		created := now.Add(time.Duration(index-5) * time.Minute)
		if index == 0 {
			created = now.Add(-8 * 24 * time.Hour)
		}
		db.EnqueueServiceError(ServiceErrorEvent{ID: fmt.Sprint(index), CreatedAt: created, StatusCode: 500})
	}
	waitServiceErrorQueue(test, db)
	if err := db.pruneServiceErrors(context.Background(), now, 2); err != nil {
		test.Fatal(err)
	}
	var count int
	if err := db.conn.QueryRow("SELECT COUNT(*) FROM service_error_events").Scan(&count); err != nil || count != 2 {
		test.Fatalf("retention count=%d err=%v", count, err)
	}
	page, err := db.ListServiceErrors(context.Background(), ServiceErrorFilter{Start: now.Add(-time.Hour), End: now, Limit: 20})
	if err != nil || len(page.Items) != 2 || page.Items[0].ID != "4" || page.Items[1].ID != "3" {
		test.Fatalf("retention page=%+v err=%v", page, err)
	}
}

func TestServiceErrorWriteFailuresAreVisible(test *testing.T) {
	db := newProxyTestDB(test)
	if _, err := db.conn.Exec("DROP TABLE service_error_events"); err != nil {
		test.Fatal(err)
	}
	if !db.EnqueueServiceError(ServiceErrorEvent{ID: "failed", StatusCode: 429}) {
		test.Fatal("enqueue failed")
	}
	waitServiceErrorQueue(test, db)
	stats := db.ServiceErrorCollectorStats()
	if stats.WriteFailures != 1 || stats.Written != 0 {
		test.Fatalf("failure not visible: %+v", stats)
	}
}

func TestServiceErrorCursorValidation(test *testing.T) {
	for _, value := range []string{"not-a-cursor", strings.Repeat("x", 513), "e30", "bnVsbA"} {
		if ValidateServiceErrorCursor(value) {
			test.Errorf("accepted cursor %q", value)
		}
	}
}

func BenchmarkServiceErrorEnqueueFull(benchmark *testing.B) {
	db := &DB{}
	db.serviceErrors = newServiceErrorQueue(db)
	defer db.serviceErrors.cancel()
	event := ServiceErrorEvent{ID: "benchmark", StatusCode: 429, Message: "concurrency limit exceeded"}
	for range serviceErrorQueueCapacity {
		db.EnqueueServiceError(event)
	}
	benchmark.ResetTimer()
	for range benchmark.N {
		db.EnqueueServiceError(event)
	}
}
