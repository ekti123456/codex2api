package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSessionAutoLockClassifiesFinalFailure(t *testing.T) {
	for _, tc := range []struct {
		name, usage, response           string
		finalUsage, priorOverload, lock bool
	}{
		{name: "handshake_timeout", response: `{"error":{"code":"internal_error","type":"server_error","message":"websocket handshake failed: context deadline exceeded"}}`},
		{name: "connection_reset", finalUsage: true, usage: "connection reset by peer"},
		{name: "tls", finalUsage: true, usage: "internal_error · TLS handshake timeout"},
		{name: "code_in_message_is_not_a_code", response: `{"error":{"code":"internal_error","message":"prior server_is_overloaded; current DNS lookup failed"}}`},
		{name: "unknown_http_500"},
		{name: "usage_overload", finalUsage: true, usage: "server_is_overloaded · service_unavailable_error · busy", lock: true},
		{name: "json_usage_overload", finalUsage: true, usage: `{"response":{"error":{"code":"server_is_overloaded","message":"busy"}}}`, lock: true},
		{name: "http_overload", response: `{"error":{"code":"server_is_overloaded","message":"busy"}}`, lock: true},
		{name: "retry_overload_then_timeout", priorOverload: true, response: `{"error":{"code":"internal_error","message":"websocket handshake failed: context deadline exceeded"}}`},
		{name: "retry_overload_then_unknown_usage", priorOverload: true, finalUsage: true},
		{name: "retry_overload_then_transport", priorOverload: true, finalUsage: true, usage: "context deadline exceeded"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h := newWindowAuthorizationHandler(t)
			require.NoError(t, h.db.SetSessionAutoLockSettings(t.Context(), database.SessionAutoLockSettings{Enabled: true, Threshold: 1}))
			ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
			ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			finish := h.beginServiceErrorAudit(ctx)
			state := serviceErrorAuditForRequest(ctx)
			state.autoLockSettings = h.db.GetSessionAutoLockSettings()
			identity := database.SessionErrorIdentity{Key: sessionOperationKey("newapi", "test", "42", continuityTestThread), Kind: "newapi", Platform: "test", UserID: "42", SessionID: continuityTestThread}
			ctx.Set(sessionOperationsContextKey, identity)
			if tc.priorOverload {
				h.recordObservedError(ctx, 500, api.NewAPIError(overloadErrorCode, "old retry", api.ErrorTypeServer))
				rememberSessionErrorUsage(ctx, &database.UsageLogInput{StatusCode: 500, IsRetryAttempt: true, ErrorMessage: "server_is_overloaded · old retry"})
			}
			if tc.finalUsage {
				rememberSessionErrorUsage(ctx, &database.UsageLogInput{StatusCode: 500, ErrorMessage: tc.usage})
			}
			ctx.Writer.WriteHeader(500)
			if tc.response != "" {
				_, err := ctx.Writer.WriteString(tc.response)
				require.NoError(t, err)
			}
			finish()
			owner, err := h.db.SessionBlacklistStatus(t.Context(), identity.Key, "")
			require.NoError(t, err)
			require.Equal(t, tc.lock, owner != "")
			if !tc.priorOverload {
				require.Eventually(t, func() bool { return h.db.SessionErrorCollectorStats().Pending == 0 }, 5*time.Second, 10*time.Millisecond)
				page, err := h.db.ListSessionErrors(t.Context(), database.SessionErrorQuery{})
				require.NoError(t, err)
				require.Len(t, page.Items, 1, "excluded failures must remain visible")
				require.NotNil(t, page.Items[0].Latest.Diagnostics.AutoLockEligible)
				require.Equal(t, tc.lock, *page.Items[0].Latest.Diagnostics.AutoLockEligible)
			}
		})
	}
}
