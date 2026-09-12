package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/api"
	"github.com/gin-gonic/gin"
)

func TestServiceErrorsCaptureVerifiedNewAPIUser(test *testing.T) {
	for _, scenario := range []struct {
		name         string
		policy       bool
		verified     bool
		websocket    bool
		frame        bool
		userName     string
		wantVerified bool
		wantUserID   string
		wantUserName string
	}{
		{name: "verified HTTP", policy: true, verified: true, userName: " 示例用户 ", wantVerified: true, wantUserID: "1881", wantUserName: "示例用户"},
		{name: "verified without username", policy: true, verified: true, wantVerified: true, wantUserID: "1881"},
		{name: "unverified policy", policy: true, userName: "unverified"},
		{name: "unsigned headers only"},
		{name: "verified WS frame", policy: true, verified: true, websocket: true, frame: true, userName: "帧用户", wantVerified: true, wantUserID: "1881", wantUserName: "帧用户"},
		{name: "WS without current frame", policy: true, verified: true, websocket: true, userName: "old handshake user"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			handler := newServiceErrorTestHandler(test)
			router := gin.New()
			router.Use(handler.ServiceErrorMiddleware())
			router.POST("/v1/responses", func(ctx *gin.Context) {
				if scenario.policy {
					ctx.Set(newAPIPolicyMetaContextKey, verifiedNewAPIPolicyContext{
						Identity:     newAPIIdentity{UserID: "1881", RequestID: "01a09519-2ddd-7964-94f4-f12c0d10950f"},
						Meta:         newAPIPolicyMeta{UserName: scenario.userName},
						MetaVerified: scenario.verified,
					})
				}
				if scenario.websocket {
					resetServiceErrorFrame(ctx)
				}
				if scenario.frame {
					captureUsageRequestIngress(ctx, []byte(`{"type":"response.create","input":"test"}`))
				}
				api.SendErrorWithStatus(ctx, api.NewAPIError(api.ErrCodeRateLimitReached, "test limit", api.ErrorTypeRateLimit), http.StatusTooManyRequests)
			})
			request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
			request.Header.Set("X-NewAPI-User-ID", "9999")
			request.Header.Set("X-NewAPI-User-Name", "unsigned-name")
			router.ServeHTTP(httptest.NewRecorder(), request)
			page := serviceErrorTestPage(test, handler)
			if len(page.Items) != 1 {
				test.Fatalf("expected one service error, got %+v", page)
			}
			event := page.Items[0]
			if event.NewAPIIdentityVerified != scenario.wantVerified || event.NewAPIUserID != scenario.wantUserID || event.NewAPIUserName != scenario.wantUserName {
				test.Fatalf("incorrect NewAPI attribution: %+v", event)
			}
			if scenario.wantVerified && event.NewAPIRequestID != "01a09519-2ddd-7964-94f4-f12c0d10950f" {
				test.Fatalf("lost verified request ID: %+v", event)
			}
		})
	}
}

func TestServiceErrorsRedactNewAPIUserName(test *testing.T) {
	handler := newServiceErrorTestHandler(test)
	router := gin.New()
	router.Use(handler.ServiceErrorMiddleware())
	router.POST("/v1/responses", func(ctx *gin.Context) {
		ctx.Set("apiKey", "secret-that-must-not-leak")
		ctx.Set(newAPIPolicyMetaContextKey, verifiedNewAPIPolicyContext{
			Identity:     newAPIIdentity{UserID: "1881"},
			Meta:         newAPIPolicyMeta{UserName: "示例用户 secret-that-must-not-leak"},
			MetaVerified: true,
		})
		api.SendErrorWithStatus(ctx, api.ErrInvalidAPIKey, http.StatusUnauthorized)
	})
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
	page := serviceErrorTestPage(test, handler)
	if len(page.Items) != 1 || !strings.Contains(page.Items[0].NewAPIUserName, "示例用户") || strings.Contains(page.Items[0].NewAPIUserName, "secret-that-must-not-leak") {
		test.Fatalf("unsafe username logging: %+v", page)
	}
}
