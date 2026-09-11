package admin

import (
	"net/http"
	"net/http/httptest"
	"net/url"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestUsageLogPrefixSearchAPI(test *testing.T) {
	db := newTestAdminDB(test)
	for _, prefix := range []string{"01a09012", "01a0901a"} {
		if err := db.InsertUsageLog(test.Context(), &database.UsageLogInput{Endpoint: "/v1/responses", Model: "test", StatusCode: 200, SessionIDPrefix: prefix}); err != nil {
			test.Fatal(err)
		}
	}
	db.FlushUsageLogs()
	handler := &Handler{db: db, adminSecretEnv: "test-prefix-admin"}
	router := gin.New()
	handler.RegisterRoutes(router)
	params := url.Values{
		"start": {time.Now().Add(-time.Hour).UTC().Format(time.RFC3339)},
		"end":   {time.Now().Add(time.Hour).UTC().Format(time.RFC3339)},
		"page":  {"1"}, "page_size": {"20"}, "q": {"01A09012"},
	}
	request := httptest.NewRequest(http.MethodGet, "/api/admin/usage/logs?"+params.Encode(), nil)
	request.Header.Set("X-Admin-Key", "test-prefix-admin")
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	body := recorder.Body.String()
	if recorder.Code != http.StatusOK || gjson.Get(body, "total").Int() != 1 || gjson.Get(body, "logs.0.session_id_prefix").String() != "01a09012" {
		test.Fatalf("search response status=%d body=%s", recorder.Code, body)
	}
	if strings.Contains(body, `"session_id":`) {
		test.Fatalf("list should not expose full session ID: %s", body)
	}
}
