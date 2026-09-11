package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestServiceErrorsReadTopLevelAndNestedErrorFields(test *testing.T) {
	for _, scenario := range []struct {
		name, body, code, message string
	}{
		{"flat", `{"message":"窗口额度已用尽","code":"window_admission_failed","details":{"ticket":"private-ticket"}}`, "window_admission_failed", "窗口额度已用尽"},
		{"nested", `{"message":"outer","code":"outer_code","error":{"message":"inner","code":"window_inner"}}`, "window_inner", "inner"},
		{"flat message only", `{"message":"invalid window policy"}`, "http_400", "invalid window policy"},
		{"string error", `{"error":"rejected"}`, "http_400", "rejected"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			handler := newServiceErrorTestHandler(test)
			router := gin.New()
			router.Use(handler.ServiceErrorMiddleware())
			router.POST("/v1/session-windows", func(ctx *gin.Context) {
				ctx.Data(http.StatusBadRequest, "application/json", []byte(scenario.body))
			})
			recorder := httptest.NewRecorder()
			router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/session-windows", nil))
			require.Equal(test, scenario.body, recorder.Body.String())
			page := serviceErrorTestPage(test, handler)
			require.Len(test, page.Items, 1)
			event := page.Items[0]
			require.Equal(test, scenario.message, event.Message)
			require.Equal(test, scenario.code, event.Code)
			require.Equal(test, "invalid_request_error", event.ErrorType)
			encoded, err := json.Marshal(event)
			require.NoError(test, err)
			require.NotContains(test, string(encoded), "private-ticket")
		})
	}
}

func TestWindowControlRejectionsRetainOperationAndCause(test *testing.T) {
	for _, scenario := range []struct {
		name, body, operation, code, message string
	}{
		{"malformed", `{`, "invalid", "window_request_invalid", "窗口控制请求格式无效"},
		{"multiplier", `{"operation":"quote","multiplier":0}`, "quote", "window_multiplier_invalid", "窗口扩容倍率必须在 1–10 之间"},
		{"extra limit", `{"operation":"quote","multiplier":1,"extra_limit":101}`, "quote", "window_extra_limit_invalid", "额外窗口上限必须在 0–100 之间"},
		{"unsupported", `{"operation":"private-command","multiplier":1}`, "unsupported", "window_operation_unsupported", "不支持的窗口操作"},
		{"confirmation", `{"operation":"upgrade","multiplier":1}`, "upgrade", "window_upgrade_confirmation_required", "请先开启扩容并单独确认此窗口的新倍率"},
		{"stale grant", `{"operation":"upgrade","multiplier":1.5,"allow_expansion":true,"extra_limit":1,"root":"missing","grant_id":"private-ticket"}`, "upgrade", "window_upgrade_stale", "窗口状态已变化或尚无可恢复的账号归属，请刷新后重试"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			handler := newRootlessPassiveModelTestHandler(test)
			db, err := database.New("sqlite", filepath.Join(test.TempDir(), "window-errors.db"))
			require.NoError(test, err)
			test.Cleanup(func() { _ = db.Close() })
			handler.db = db
			request, response := windowExpansionTestContext(test, "/v1/session-windows", []byte(scenario.body), newAPIPolicyMeta{})
			finish := handler.beginServiceErrorAudit(request)
			handler.ControlNewAPIUserWindows(request)
			finish()
			require.Equal(test, http.StatusBadRequest, response.Code)
			var payload map[string]string
			require.NoError(test, json.Unmarshal(response.Body.Bytes(), &payload))
			require.Equal(test, scenario.message, payload["message"])
			require.Equal(test, scenario.code, payload["code"])
			page := serviceErrorTestPage(test, handler)
			require.Len(test, page.Items, 1)
			event := page.Items[0]
			require.Equal(test, scenario.message, event.Message)
			require.Equal(test, scenario.code, event.Code)
			require.Equal(test, "window", event.Stage)
			require.Equal(test, "gateway_internal", event.RequestType)
			require.Equal(test, "window_control", event.RequestKind)
			require.Equal(test, scenario.operation, event.ClientInfo["window_control.operation"])
			require.True(test, event.NewAPIIdentityVerified)
			encoded, err := json.Marshal(event)
			require.NoError(test, err)
			require.NotContains(test, string(encoded), "private-ticket")
			require.NotContains(test, string(encoded), "private-command")
		})
	}
}
