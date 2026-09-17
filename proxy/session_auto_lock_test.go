package proxy

import (
	"context"
	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
	"net/http"
	"testing"
	"time"
)

func TestSessionAutoLockExcludesAPIRelay(t *testing.T) {
	for _, selected := range []bool{false, true} {
		t.Run(map[bool]string{false: "route_exemption", true: "selected_from_mixed_pool"}[selected], func(t *testing.T) {
			h := newWindowAuthorizationHandler(t)
			require.NoError(t, h.db.SetSessionAutoLockSettings(t.Context(), database.SessionAutoLockSettings{Enabled: true, Threshold: 2}))
			relay := apiRelayPolicyTestAccount()
			h.store.AddAccount(relay)
			var identity database.SessionErrorIdentity
			for range 3 {
				_, body := continuityTestRequest(0, "turn")
				r, _ := signedRootlessPassiveModelContext(t, http.MethodPost, "/v1/responses", body, sessionOperationsTestMeta(continuityTestThread))
				finish := h.beginServiceErrorAudit(r)
				captureUsageRequestIngress(r, body)
				h.primeNewAPIPolicyContext(r, body)
				h.resolveRequestSessionIdentityForContext(r, body)
				identity, _ = sessionOperationsIdentity(r)
				require.NotEmpty(t, identity.Key)
				if selected {
					r.Set(apiRelaySessionExemptContextKey, false)
					attachUpstreamTrace(r, h.store)
					beginUpstreamTrace(r.Request.Context(), relay, "", false)
				} else {
					r.Set(apiRelaySessionExemptContextKey, true)
				}
				rememberSessionErrorUsage(r, &database.UsageLogInput{StatusCode: 500, ErrorMessage: "test failure"})
				finish()
			}
			owner, err := h.db.SessionBlacklistStatus(t.Context(), identity.Key, "")
			require.NoError(t, err)
			require.Empty(t, owner)
			// One later eligible failure is still below threshold: relay failures
			// must not silently accumulate a streak even when not enforced.
			locked, err := h.db.ObserveSessionFinalStatus(t.Context(), identity, 500, time.Now(), h.db.GetSessionAutoLockSettings())
			require.NoError(t, err)
			require.False(t, locked)
		})
	}
}

func TestSessionAutoLockFinalRequestsAndWebSocketFrames(t *testing.T) {
	for _, ws := range []bool{false, true} {
		t.Run(map[bool]string{false: "http", true: "websocket"}[ws], func(t *testing.T) {
			h := newWindowAuthorizationHandler(t)
			require.NoError(t, h.db.SetSessionAutoLockSettings(context.Background(), database.SessionAutoLockSettings{Enabled: true, Threshold: 3}))
			for i, status := range []int{500, 500, 200, 500, 500, 500} {
				_, body := continuityTestRequest(0, "turn")
				r, _ := signedRootlessPassiveModelContext(t, http.MethodPost, "/v1/responses", body, sessionOperationsTestMeta(continuityTestThread))
				finish := h.beginServiceErrorAudit(r)
				if ws {
					resetServiceErrorFrame(r)
				}
				captureUsageRequestIngress(r, body)
				h.primeNewAPIPolicyContext(r, body)
				h.resolveRequestSessionIdentityForContext(r, body)
				identity, known := sessionOperationsIdentity(r)
				require.True(t, known)
				// Any number of internal failed attempts must count as one final request.
				for range 5 {
					rememberSessionErrorUsage(r, &database.UsageLogInput{StatusCode: 500, IsRetryAttempt: true})
				}
				rememberSessionErrorUsage(r, &database.UsageLogInput{StatusCode: status, ErrorMessage: map[bool]string{true: "upstream_error", false: ""}[status == 500]})
				if status == 500 {
					h.recordObservedError(r, 500, api.NewAPIError("upstream_error", "failure", api.ErrorTypeServer))
				}
				if ws {
					h.finishSessionErrorAudit(r)
				}
				finish()
				h.finishSessionErrorAudit(r) // Duplicate finalizers must be harmless.
				owner, err := h.db.SessionBlacklistStatus(context.Background(), identity.Key, "")
				require.NoError(t, err)
				if i == 5 {
					require.Equal(t, identity.Key, owner)
					require.NotNil(t, h.sessionBlacklistError(r))
				} else {
					require.Empty(t, owner)
				}
			}
		})
	}
}
