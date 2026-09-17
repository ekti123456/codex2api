package proxy

import (
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSessionErrorDetailsPreserveErrors(t *testing.T) {
	for _, tc := range []struct {
		name, message, response, code, wantMessage, source string
		ws                                                 bool
	}{
		{name: "usage", message: "upstream_failed · server_error · original failure", code: "upstream_failed", wantMessage: "original failure", source: "final_usage"},
		{name: "websocket", ws: true, message: `{"response":{"error":{"code":"stream_failed","message":"original stream failure","type":"server_error"}}}`, code: "stream_failed", wantMessage: "original stream failure", source: "final_usage"},
		{name: "raw_http", response: `{"error":{"code":"provider_failure","message":"raw upstream failure","type":"server_error"}}`, code: "provider_failure", wantMessage: "raw upstream failure", source: "http_response"},
		{name: "nested_http", response: `{"response":{"error":{"code":1234,"message":"numeric error","type":"server_error"}}}`, code: "1234", wantMessage: "numeric error", source: "http_response"},
		{name: "transport", message: "connection reset by peer", code: "http_500", wantMessage: "connection reset by peer", source: "final_usage"},
		{name: "empty", code: "http_500", wantMessage: "请求返回 HTTP 500", source: "http_status_fallback"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newWindowAuthorizationHandler(t)
			require.NoError(t, h.db.SetSessionAutoLockSettings(t.Context(), database.SessionAutoLockSettings{Enabled: true, Threshold: 3}))
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			finish := h.beginServiceErrorAudit(ctx)
			state := serviceErrorAuditForRequest(ctx)
			state.autoLockSettings, state.websocket = h.db.GetSessionAutoLockSettings(), tc.ws
			ctx.Set(sessionOperationsContextKey, database.SessionErrorIdentity{Key: sessionOperationKey("newapi", "test", "42", continuityTestThread), Kind: "newapi", Platform: "test", UserID: "42", SessionID: continuityTestThread})
			const requestID = "01a0ad88-0ada-7434-950f-a31ed617a74d"
			if tc.message != "" {
				rememberSessionErrorUsage(ctx, &database.UsageLogInput{RequestID: requestID, StatusCode: 500, ErrorMessage: tc.message, UpstreamErrorKind: "upstream_response"})
			}
			if tc.response != "" {
				ctx.Writer.WriteHeader(500)
				_, err := ctx.Writer.WriteString(tc.response)
				require.NoError(t, err)
			} else if tc.message == "" {
				ctx.Writer.WriteHeader(500)
			}
			finish()
			require.Eventually(t, func() bool { return h.db.SessionErrorCollectorStats().Pending == 0 }, 5*time.Second, 10*time.Millisecond)
			page, err := h.db.ListSessionErrors(t.Context(), database.SessionErrorQuery{})
			require.NoError(t, err)
			require.EqualValues(t, 1, page.Errors)
			require.Len(t, page.Items, 1)
			event := page.Items[0].Latest
			require.Equal(t, tc.code, event.Code)
			if tc.name == "empty" {
				// The writer may supply its existing generic fallback text.
				require.Contains(t, []string{tc.wantMessage, "Service request rejected"}, event.Message)
			} else {
				require.Equal(t, tc.wantMessage, event.Message)
			}
			require.Equal(t, tc.source, event.Diagnostics.ErrorSource)
			require.Equal(t, tc.message != "", event.Diagnostics.UsageCaptured)
			if tc.message != "" {
				require.Equal(t, requestID, event.RequestID)
				require.Equal(t, requestID, event.Diagnostics.UsageRequestID)
				require.Equal(t, "final_usage", event.Diagnostics.StatusSource)
			}
		})
	}
}

func TestSessionErrorDetailsFinalUsageDoesNotReuseRetryError(t *testing.T) {
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx.Set("apiKey", "private-key-to-redact")
	state := &serviceErrorAudit{observedStatus: 500, observedError: api.NewAPIError("earlier_error", "earlier private-key-to-redact", api.ErrorTypeServer)}
	ctx.Set(serviceErrorContextKey, state)
	rememberSessionErrorUsage(ctx, &database.UsageLogInput{StatusCode: 500, IsRetryAttempt: true, ErrorMessage: "earlier_error · earlier failure"})
	rememberSessionErrorUsage(ctx, &database.UsageLogInput{StatusCode: 500, ErrorMessage: "final_error · final private-key-to-redact"})
	failure, diagnostic := sessionErrorDetails(ctx, state, 500, state.observedError)
	require.Equal(t, api.ErrorCode("final_error"), failure.Code)
	require.Equal(t, "earlier_error", diagnostic.ObservedError.Code)
	payload, err := json.Marshal(struct {
		Failure     *api.APIError
		Diagnostics *database.SessionErrorDiagnostics
	}{failure, diagnostic})
	require.NoError(t, err)
	require.NotContains(t, string(payload), "private-key-to-redact")
	require.Contains(t, string(payload), "REDACTED")
	rememberSessionErrorUsage(ctx, &database.UsageLogInput{StatusCode: 200})
	require.Empty(t, state.finalUsageMessage)
}

func TestSessionErrorUsageCapturesNormalizedStatus(t *testing.T) {
	h := newWindowAuthorizationHandler(t)
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	h.beginServiceErrorAudit(ctx)
	ctx.Set(upstreamPromptSafetyContextKey, &database.PromptSafetyDiagnostic{})
	input := &database.UsageLogInput{StatusCode: 500, ErrorMessage: "invalid_prompt · rejected"}
	h.logUsageForRequest(ctx, input)
	require.Equal(t, 400, input.StatusCode)
	state := serviceErrorAuditForRequest(ctx)
	require.Equal(t, 400, state.finalUsageStatus)
	require.Equal(t, "safety_policy", state.finalUsageErrorKind)
}
