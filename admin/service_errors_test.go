package admin

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func TestServiceErrorFilterValidation(test *testing.T) {
	now := time.Now().UTC()
	for _, query := range []string{"start=bad", "status=200", "stage=upstream", "limit=101", "limit=-1", "cursor=invalid", "request_id=" + strings.Repeat("x", 161), "start=2026-09-01T00:00:00Z&end=2026-09-10T00:00:00Z"} {
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		ctx.Request = httptest.NewRequest(http.MethodGet, "/?"+query, nil)
		if _, err := parseServiceErrorFilter(ctx, now); err == nil {
			test.Errorf("accepted invalid filter %s", query)
		}
	}
}

func TestServiceErrorAdminList(test *testing.T) {
	db := newTestAdminDB(test)
	db.EnqueueServiceError(database.ServiceErrorEvent{ID: "admin-service-error", StatusCode: 429, Stage: "rate_limit", Message: "concurrency full"})
	deadline := time.Now().Add(5 * time.Second)
	for db.ServiceErrorCollectorStats().Pending > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	handler := &Handler{db: db}
	router := gin.New()
	router.GET("/service-errors", handler.GetServiceErrorLogs)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/service-errors?status=429", nil))
	var page database.ServiceErrorPage
	if err := json.Unmarshal(recorder.Body.Bytes(), &page); err != nil || recorder.Code != http.StatusOK || page.Summary.Total != 1 || len(page.Items) != 1 {
		test.Fatalf("list status=%d body=%s err=%v", recorder.Code, recorder.Body.String(), err)
	}
}

func TestServiceErrorAdminRouteRequiresAuthentication(test *testing.T) {
	handler := &Handler{db: newTestAdminDB(test)}
	router := gin.New()
	handler.RegisterRoutes(router)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodGet, "/api/admin/ops/service-errors", nil))
	if recorder.Code != http.StatusUnauthorized && recorder.Code != http.StatusServiceUnavailable {
		test.Fatalf("unauthenticated access status=%d body=%s", recorder.Code, recorder.Body.String())
	}
}
