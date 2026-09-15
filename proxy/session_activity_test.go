package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSessionActivityProxyLifecycle(t *testing.T) {
	handler := newWindowAuthorizationHandler(t)
	meta := sessionOperationsTestMeta(continuityTestThread)
	_, body := continuityTestRequest(0, "turn")
	request, _ := signedRootlessPassiveModelContext(t, http.MethodPost, "/v1/responses", body, meta)
	finish := handler.beginServiceErrorAudit(request)
	captureUsageRequestIngress(request, body)
	handler.primeNewAPIPolicyContext(request, body)
	handler.resolveRequestSessionIdentityForContext(request, body)
	identity, known := sessionOperationsIdentity(request)
	require.True(t, known)
	require.True(t, handler.db.EnqueueSessionError(database.SessionErrorEvent{Identity: identity, CreatedAt: time.Now().Add(-time.Minute)}))
	require.Eventually(t, func() bool { return handler.db.SessionErrorCollectorStats().Pending == 0 }, 5*time.Second, 10*time.Millisecond)
	get := func() database.SessionActivity {
		page, err := handler.db.SessionActivities(context.Background(), []string{identity.Key}, time.Now())
		require.NoError(t, err)
		return page.Items[identity.Key]
	}
	require.Equal(t, 1, get().ActiveRequests)
	// Re-resolving the same ingress identity during retries/account changes is
	// idempotent; it does not create another lease or key using outbound IDs.
	handler.resolveRequestSessionIdentityForContext(request, body)
	rememberSessionErrorUsage(request, &database.UsageLogInput{StatusCode: 500, IsRetryAttempt: true})
	require.Equal(t, 1, get().ActiveRequests)
	require.False(t, get().Recovered)
	rememberSessionErrorUsage(request, &database.UsageLogInput{StatusCode: 200})
	finish()
	finish()
	require.Zero(t, get().ActiveRequests)
	require.True(t, get().Recovered)
	// Each WebSocket frame gets a fresh lease; an idle connection has none.
	for _, sample := range []struct {
		source, kind string
		failed       bool
	}{
		{"user", "turn", false}, {"subagent", "naming", false}, {"user", "turn", true},
	} {
		resetServiceErrorFrame(request)
		request.Set(usageRequestDiagnosticsContextKey, &usageRequestDiagnostics{Resolved: &usageRequestResolution{ThreadSource: sample.source, RequestKind: sample.kind}})
		request.Set(sessionOperationsContextKey, identity)
		handler.beginSessionActivity(request, identity)
		if sample.source == "user" {
			require.Equal(t, 1, get().ActiveRequests)
		} else {
			require.Equal(t, 1, get().AuxiliaryRequests)
			require.Zero(t, get().ActiveRequests)
		}
		previousSuccess := *get().LastSuccessAt
		if sample.failed {
			// A 200 handshake followed by an SSE/WS error must not record success.
			rememberSessionErrorUsage(request, &database.UsageLogInput{StatusCode: 200, ErrorMessage: "stream failed"})
		} else {
			rememberSessionErrorUsage(request, &database.UsageLogInput{StatusCode: 200})
		}
		handler.finishSessionActivity(request)
		handler.finishSessionActivity(request)
		require.Zero(t, get().ActiveRequests)
		require.Zero(t, get().AuxiliaryRequests)
		if sample.failed || sample.source != "user" {
			require.Equal(t, previousSuccess, *get().LastSuccessAt)
		}
	}
	// No usage outcome (including a successful handshake alone) is not success.
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	end := handler.beginServiceErrorAudit(ctx)
	handler.beginSessionActivity(ctx, identity)
	previousSuccess := *get().LastSuccessAt
	end()
	require.Equal(t, previousSuccess, *get().LastSuccessAt)
	// Cancellation and a terminal observed error release the lease without success.
	for _, canceled := range []bool{false, true} {
		ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
		requestContext, cancel := context.WithCancel(context.Background())
		ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil).WithContext(requestContext)
		end := handler.beginServiceErrorAudit(ctx)
		handler.beginSessionActivity(ctx, identity)
		rememberSessionErrorUsage(ctx, &database.UsageLogInput{StatusCode: 200})
		if canceled {
			cancel()
		} else {
			handler.recordObservedError(ctx, 500, api.NewAPIError(api.ErrCodeServerError, "failed", api.ErrorTypeServer))
		}
		end()
		cancel()
		require.Zero(t, get().ActiveRequests)
		require.Equal(t, previousSuccess, *get().LastSuccessAt)
	}
}
