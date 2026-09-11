package admin

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"net/url"
	"os"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestUsageLogExportConfirmsScopeAndKeepsDiagnosticsWithoutCredentials(test *testing.T) {
	db := newTestAdminDB(test)
	accountID, err := db.InsertAccountWithCredentials(test.Context(), "account-name", map[string]interface{}{
		"refresh_token": "never-export-this", "email": "export@example.com",
	}, "")
	require.NoError(test, err)
	diagnostic := `{"incoming":{"headers":{"Session-Id":"01a09012-0000-7000-8000-000000000001","Authorization":"Bearer secret-auth","X-Codex-Turn-Metadata":"{\"session_id\":\"01a09012-0000-7000-8000-000000000001\",\"refresh_token\":\"secret-nested\"}"}},"upstream":{"send_phase":"after_payload","outbound_identity":{"body":{"client_metadata":{"session_id":"01a09012-0000-7000-8000-000000000001"}}}},"request_body":"secret-prompt","counter":9007199254740993}`
	for _, requestType := range []string{"user", "compaction"} {
		require.NoError(test, db.InsertUsageLog(test.Context(), &database.UsageLogInput{
			AccountID: accountID, StatusCode: 500, ErrorMessage: "server_is_overloaded · Bearer secret-text", Endpoint: "/v1/responses",
			RequestType: requestType, RequestDiagnostics: diagnostic, NewAPIUserName: "diagnostic-user", RequestID: "request-" + requestType,
		}))
	}
	db.FlushUsageLogs()
	handler := &Handler{db: db}
	temp := test.TempDir()
	test.Setenv("TMP", temp)
	test.Setenv("TMPDIR", temp)
	params := url.Values{"scope": {"filtered"}, "confirmed": {"true"}, "start": {time.Now().Add(-time.Hour).Format(time.RFC3339)}, "end": {time.Now().Add(time.Hour).Format(time.RFC3339)}, "request_type": {"compaction"}, "page": {"99"}, "page_size": {"1"}}
	for _, scope := range []string{"filtered", "all"} {
		params.Set("scope", scope)
		response := httptest.NewRecorder()
		request, _ := gin.CreateTestContext(response)
		request.Request = httptest.NewRequest(http.MethodPost, "/usage/logs/export?"+params.Encode(), nil)
		handler.ExportUsageLogs(request)
		require.Equal(test, http.StatusOK, response.Code, response.Body.String())
		require.Contains(test, response.Header().Get("Content-Disposition"), "usage-logs-"+scope)
		require.Equal(test, "no-store", response.Header().Get("Cache-Control"))
		var result struct {
			Scope    string            `json:"scope"`
			Total    int               `json:"total"`
			Complete bool              `json:"complete"`
			Filters  map[string]string `json:"filters"`
			Logs     []json.RawMessage `json:"logs"`
		}
		require.NoError(test, json.Unmarshal(response.Body.Bytes(), &result))
		require.Equal(test, scope, result.Scope)
		require.True(test, result.Complete)
		wantCount := 1
		if scope == "all" {
			wantCount = 2
			require.Empty(test, result.Filters)
		}
		require.Equal(test, wantCount, result.Total)
		require.Len(test, result.Logs, wantCount)
		for _, forbidden := range []string{"never-export-this", "secret-auth", "secret-nested", "secret-text", "secret-prompt"} {
			require.NotContains(test, response.Body.String(), forbidden)
		}
		require.Contains(test, response.Body.String(), "9007199254740993")
		require.Contains(test, response.Body.String(), "01a09012-0000-7000-8000-000000000001")
		require.Contains(test, response.Body.String(), "after_payload")
		require.Contains(test, response.Body.String(), "export@example.com")
		files, err := os.ReadDir(temp)
		require.NoError(test, err)
		require.Empty(test, files)
	}
}

func TestUsageLogExportRejectsMissingConfirmationAndInvalidFilters(test *testing.T) {
	handler := &Handler{}
	for _, query := range []string{"scope=all", "scope=all&confirmed=false", "scope=invalid&confirmed=true", "scope=filtered&confirmed=true", "scope=filtered&confirmed=true&start=2026-09-12T00:00:00Z&end=2026-09-11T00:00:00Z", "scope=filtered&confirmed=true&channel=invalid"} {
		response := httptest.NewRecorder()
		request, _ := gin.CreateTestContext(response)
		request.Request = httptest.NewRequest(http.MethodPost, "/usage/logs/export?"+query, nil)
		handler.ExportUsageLogs(request)
		require.Equal(test, http.StatusBadRequest, response.Code, query)
		require.Empty(test, response.Header().Get("Content-Disposition"))
	}
	handler.usageLogExportBusy.Store(true)
	response := httptest.NewRecorder()
	request, _ := gin.CreateTestContext(response)
	request.Request = httptest.NewRequest(http.MethodPost, "/usage/logs/export?scope=all&confirmed=true", nil)
	handler.ExportUsageLogs(request)
	require.Equal(test, http.StatusConflict, response.Code)
}

func TestUsageLogExportEmptyAndCanceledAreNotPartialDownloads(test *testing.T) {
	handler := &Handler{db: newTestAdminDB(test)}
	var output bytes.Buffer
	require.NoError(test, handler.writeUsageLogExport(test.Context(), &output, "all", map[string]string{}, nil, time.Now()))
	var result struct {
		Logs     []any `json:"logs"`
		Complete bool  `json:"complete"`
	}
	require.NoError(test, json.Unmarshal(output.Bytes(), &result))
	require.Empty(test, result.Logs)
	require.True(test, result.Complete)
	ctx, cancel := context.WithCancel(test.Context())
	cancel()
	response := httptest.NewRecorder()
	request, _ := gin.CreateTestContext(response)
	request.Request = httptest.NewRequest(http.MethodPost, "/usage/logs/export?scope=all&confirmed=true", nil).WithContext(ctx)
	handler.ExportUsageLogs(request)
	require.GreaterOrEqual(test, response.Code, http.StatusBadRequest)
	require.Empty(test, response.Header().Get("Content-Disposition"))
	require.False(test, strings.Contains(response.Body.String(), `"complete":true`))
	require.False(test, handler.usageLogExportBusy.Load())
}
