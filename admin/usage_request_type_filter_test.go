package admin

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestUsageRequestTypeFilterValidation(test *testing.T) {
	for _, requestType := range []string{"", "user", "related_internal", "independent_internal", "related_unclassified", "compaction", "gateway_internal", "unknown", "not_recorded", "invalid", "user,compaction", "' OR 1=1 --"} {
		test.Run(requestType, func(test *testing.T) {
			recorder := httptest.NewRecorder()
			request, _ := gin.CreateTestContext(recorder)
			request.Request = httptest.NewRequest(http.MethodGet, "/usage/logs?request_type="+url.QueryEscape(requestType), nil)
			filter, valid := parseUsageLogsFilter(request, time.Now().Add(-time.Hour), time.Now())
			wantValid := requestType != "invalid" && requestType != "user,compaction" && requestType != "' OR 1=1 --"
			if valid != wantValid || valid && filter.RequestType != requestType || !valid && recorder.Code != http.StatusBadRequest {
				test.Fatalf("valid=%t filter=%+v status=%d", valid, filter, recorder.Code)
			}
		})
	}
}

func TestUsageRequestTypeFilterAPIAndSummary(test *testing.T) {
	db := newTestAdminDB(test)
	for _, requestType := range []string{"user", "compaction"} {
		if err := db.InsertUsageLog(test.Context(), &database.UsageLogInput{Endpoint: "/v1/responses", Model: "test", StatusCode: 500, RequestType: requestType, SessionIDPrefix: "01a09012"}); err != nil {
			test.Fatal(err)
		}
	}
	db.FlushUsageLogs()
	handler := &Handler{db: db}
	params := url.Values{
		"start": {time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)},
		"end":   {time.Now().Add(time.Hour).UTC().Format(time.RFC3339)},
		"page":  {"1"}, "page_size": {"1"}, "request_type": {"compaction"}, "q": {"01a09012"},
	}
	for _, summary := range []bool{false, true} {
		recorder := httptest.NewRecorder()
		request, _ := gin.CreateTestContext(recorder)
		request.Request = httptest.NewRequest(http.MethodGet, "/usage/logs?"+params.Encode(), nil)
		if summary {
			handler.GetUsageLogsErrorSummary(request)
		} else {
			handler.GetUsageLogs(request)
		}
		body := recorder.Body.String()
		if recorder.Code != http.StatusOK {
			test.Fatalf("status=%d body=%s", recorder.Code, body)
		}
		if summary {
			if gjson.Get(body, "total_errors").Int() != 1 {
				test.Fatalf("summary ignored request type: %s", body)
			}
		} else if gjson.Get(body, "total").Int() != 1 || gjson.Get(body, "logs.0.request_type").String() != "compaction" {
			test.Fatalf("logs ignored request type: %s", body)
		}
	}
}
