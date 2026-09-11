package proxy

import (
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
)

const unlinkedFallbackRuntimeNamespace = "codex-unlinked-account-fallback"

type unlinkedFallbackRuntimeRecord struct {
	AccountID  int64     `json:"account_id"`
	ObservedAt time.Time `json:"observed_at"`
}

func TestUnlinkedFallbackVerifiedNewAPIScopeIgnoresDownstreamChannelCredential(t *testing.T) {
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx.Request.Header.Set("Authorization", "Bearer channel-a")
	policy := verifiedNewAPIPolicyContext{
		Identity:     newAPIIdentity{UserID: "42"},
		Platform:     "newapi",
		MetaVerified: true,
		Meta: newAPIPolicyMeta{
			TokenID:        7,
			InstallationID: "device-a",
		},
	}
	first := unlinkedFallbackScopeForRequest(ctx, nil, policy, true)
	if first == "" {
		t.Fatal("expected a verified NewAPI fallback scope")
	}

	ctx.Request.Header.Set("Authorization", "Bearer channel-b")
	second := unlinkedFallbackScopeForRequest(ctx, nil, policy, true)
	if second != first {
		t.Fatalf("scope changed with downstream channel credential: first=%q second=%q", first, second)
	}

	policy.Meta.InstallationID = "device-b"
	if got := unlinkedFallbackScopeForRequest(ctx, nil, policy, true); got == first {
		t.Fatal("installation ID did not isolate the fallback scope")
	}
}

func TestUnlinkedFallbackSettingCannotRestoreTemporalSelection(test *testing.T) {
	for _, enabled := range []bool{false, true} {
		test.Run(fmt.Sprint(enabled), func(test *testing.T) {
			handler := newRootlessPassiveModelTestHandler(test)
			body := []byte(`{"model":"gpt-5.6-sol","input":"background"}`)
			handler.store.SetCodexUnlinkedAccountFallbackEnabled(enabled)
			requestContext, _ := signedRootlessPassiveModelContext(test, http.MethodPost, "/v1/responses", body,
				newAPIPolicyMeta{RootSessionVersion: 1, RootSessionState: newAPIPolicyRootSessionUnavailable, ThreadSource: "agent_created_thread", RequestKind: "turn"})
			handler.primeNewAPIPolicyContext(requestContext, body)
			identity := handler.resolveRequestSessionIdentityForContext(requestContext, body)
			if identity.unlinkedFallbackOnly || identity.unlinkedFallbackScope == "" || !identity.requiresRootAccount {
				test.Fatalf("legacy switch changed strict routing: %+v", identity)
			}
			if err := handler.waitForBackgroundRootAccount(requestContext, identity); err == nil {
				test.Fatal("rootless request bypassed required root binding")
			}
			diagnostic := usageRequestDiagnosticState(requestContext)
			if diagnostic.Recent.Enabled || diagnostic.Recent.Scope == "" {
				test.Fatalf("invalid retired fallback diagnostic: %+v", diagnostic.Recent)
			}
		})
	}
}
