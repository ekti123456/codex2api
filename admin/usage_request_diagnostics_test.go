package admin

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestUsageRequestDiagnosticsEndpointRequiresAdmin(test *testing.T) {
	db := newTestAdminDB(test)
	if err := db.InsertUsageLog(test.Context(), &database.UsageLogInput{StatusCode: 200, Endpoint: "/v1/responses", RequestType: "user", RequestDiagnostics: `{"version":1,"selected_account_id":17}`}); err != nil {
		test.Fatal(err)
	}
	db.FlushUsageLogs()
	logs, err := db.ListRecentUsageLogs(test.Context(), 10)
	if err != nil || len(logs) != 1 {
		test.Fatalf("logs=%+v, err=%v", logs, err)
	}
	handler := &Handler{db: db, adminSecretEnv: "test-diagnostic-admin"}
	router := gin.New()
	handler.RegisterRoutes(router)
	for _, item := range []struct {
		id     string
		secret string
		status int
	}{
		{fmt.Sprint(logs[0].ID), "", http.StatusUnauthorized},
		{fmt.Sprint(logs[0].ID), "invalid", http.StatusUnauthorized},
		{fmt.Sprint(logs[0].ID), "test-diagnostic-admin", http.StatusOK},
		{"-1", "test-diagnostic-admin", http.StatusBadRequest},
		{"invalid", "test-diagnostic-admin", http.StatusBadRequest},
		{"999999", "test-diagnostic-admin", http.StatusNotFound},
	} {
		request := httptest.NewRequest(http.MethodGet, "/api/admin/usage/logs/"+item.id+"/diagnostics", nil)
		request.Header.Set("X-Admin-Key", item.secret)
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, request)
		if recorder.Code != item.status {
			test.Fatalf("id=%s auth=%t: status=%d, body=%s", item.id, item.secret != "", recorder.Code, recorder.Body.String())
		}
		if strings.Contains(recorder.Body.String(), "selected_account_id") != (item.status == http.StatusOK) {
			test.Fatalf("unexpected detail visibility: %s", recorder.Body.String())
		}
	}
}

func TestUsageRequestDiagnosticsEnrichesConnectionLifecycle(test *testing.T) {
	db := newTestAdminDB(test)
	connectionID := proxy.NewUpstreamSessionUUID()
	requestDiagnostics := fmt.Sprintf(`{"version":1,"upstream":{"connection_id":%q,"account_id":17}}`, connectionID)
	if err := db.InsertUsageLog(test.Context(), &database.UsageLogInput{StatusCode: 200, Endpoint: "/v1/responses", RequestType: "user", RequestDiagnostics: requestDiagnostics}); err != nil {
		test.Fatal(err)
	}
	db.FlushUsageLogs()
	logs, err := db.ListRecentUsageLogs(test.Context(), 10)
	if err != nil || len(logs) != 1 {
		test.Fatalf("logs=%v err=%v", logs, err)
	}
	proxy.RecordWebsocketLifecycle(proxy.WebsocketConnectionLifecycle{ConnectionID: connectionID, AccountID: 17, State: "closed", ExitReason: "upstream_close", CloseCode: 1001, ObservedAt: time.Now()})
	handler := &Handler{db: db, adminSecretEnv: "lifecycle-admin-test"}
	router := gin.New()
	handler.RegisterRoutes(router)
	request := httptest.NewRequest(http.MethodGet, fmt.Sprintf("/api/admin/usage/logs/%d/diagnostics", logs[0].ID), nil)
	request.Header.Set("X-Admin-Key", "lifecycle-admin-test")
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	if response.Code != http.StatusOK || gjson.Get(response.Body.String(), "diagnostics.upstream.connection_lifecycle.exit_reason").String() != "upstream_close" {
		test.Fatalf("status=%d body=%s", response.Code, response.Body.String())
	}
	persisted, err := db.GetUsageRequestDiagnostics(test.Context(), logs[0].ID)
	if err != nil || strings.Contains(string(persisted.Diagnostics), "connection_lifecycle") {
		test.Fatalf("enrichment changed stored request snapshot: %+v %v", persisted, err)
	}
}
