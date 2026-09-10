package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/codex2api/security"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
)

func newServiceErrorTestHandler(test *testing.T) *Handler {
	test.Helper()
	db, err := database.New("sqlite", filepath.Join(test.TempDir(), "service-errors.db"))
	if err != nil {
		test.Fatal(err)
	}
	test.Cleanup(func() { _ = db.Close() })
	store := auth.NewStore(db, nil, nil)
	test.Cleanup(store.Stop)
	return NewHandler(store, db, &config.Config{AllowAnonymousV1: true}, nil)
}

func serviceErrorTestPage(test *testing.T, handler *Handler) database.ServiceErrorPage {
	test.Helper()
	deadline := time.Now().Add(5 * time.Second)
	for handler.db.ServiceErrorCollectorStats().Pending > 0 && time.Now().Before(deadline) {
		time.Sleep(5 * time.Millisecond)
	}
	if stats := handler.db.ServiceErrorCollectorStats(); stats.Pending != 0 || stats.WriteFailures != 0 {
		test.Fatalf("collector unhealthy: %+v", stats)
	}
	page, err := handler.db.ListServiceErrors(context.Background(), database.ServiceErrorFilter{Start: time.Now().Add(-time.Hour), End: time.Now().Add(time.Second), Limit: 100})
	if err != nil {
		test.Fatal(err)
	}
	return page
}

func TestServiceErrorsGlobalRateLimitBeforeAuthentication(test *testing.T) {
	handler := newServiceErrorTestHandler(test)
	router := gin.New()
	router.Use(handler.ServiceErrorMiddleware(), NewRateLimiter(1).Middleware())
	router.POST("/v1/responses", handler.APIKeyAuthMiddleware(), func(ctx *gin.Context) { ctx.Status(http.StatusOK) })
	var rejected int
	for range 6 {
		recorder := httptest.NewRecorder()
		router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
		if recorder.Code == http.StatusTooManyRequests {
			rejected++
			if recorder.Header().Get("X-Codex2API-Request-ID") == "" {
				test.Fatal("missing correlation header")
			}
		}
	}
	page := serviceErrorTestPage(test, handler)
	if rejected == 0 || page.Summary.Total != int64(rejected) {
		test.Fatalf("missing global rejections: rejected=%d page=%+v", rejected, page)
	}
	for _, event := range page.Items {
		if event.StatusCode != 429 || event.Stage != "rate_limit" || event.Code != "rate_limit_exceeded" || event.APIKeyID != 0 {
			test.Fatalf("incorrect global error: %+v", event)
		}
	}
}

func TestServiceErrorsHTTPDedupeRedactionAndUpstreamExclusion(test *testing.T) {
	handler := newServiceErrorTestHandler(test)
	router := gin.New()
	router.Use(handler.ServiceErrorMiddleware())
	router.POST("/v1/local", handler.APIKeyAuthMiddleware(), func(ctx *gin.Context) {
		ctx.Set("apiKey", "local-secret-that-must-not-leak")
		ctx.Set("x-model", "gpt-6-astra")
		api.SendErrorWithStatus(ctx, api.NewAPIError(api.ErrCodeRateLimitReached, "limit local-secret-that-must-not-leak", api.ErrorTypeRateLimit), 429)
	})
	router.POST("/v1/upstream", handler.APIKeyAuthMiddleware(), func(ctx *gin.Context) {
		handler.sendUpstreamError(ctx, 429, []byte(`{"error":{"code":"usage_limit_reached","message":"upstream quota exhausted"}}`))
	})
	router.POST("/v1/pool", handler.APIKeyAuthMiddleware(), func(ctx *gin.Context) {
		handler.sendFinalUpstreamError(ctx, 429, []byte(`{"error":{"code":"usage_limit_reached","message":"upstream quota exhausted"}}`))
	})
	router.GET("/api/admin/unrelated", func(ctx *gin.Context) { api.SendError(ctx, api.ErrInvalidAPIKey) })
	for _, path := range []string{"/v1/local", "/v1/upstream", "/v1/pool", "/api/admin/unrelated"} {
		method := http.MethodPost
		if strings.Contains(path, "/admin/") {
			method = http.MethodGet
		}
		router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(method, path+"?token=query-secret", nil))
	}
	page := serviceErrorTestPage(test, handler)
	if page.Summary.Total != 1 || len(page.Items) != 1 {
		test.Fatalf("duplicated or included upstream errors: %+v", page)
	}
	event := page.Items[0]
	payload, _ := json.Marshal(event)
	if strings.Contains(string(payload), "local-secret") || strings.Contains(string(payload), "query-secret") || event.Model != "gpt-6-astra" || event.RequestID == "" || event.Stage != "rate_limit" {
		test.Fatalf("unsafe service error: %s", payload)
	}
}

func TestServiceErrorsPreBodyValidationAndPanic(test *testing.T) {
	handler := newServiceErrorTestHandler(test)
	router := gin.New()
	router.Use(api.RecoveryMiddleware(), handler.ServiceErrorMiddleware(), security.RequestSizeLimiter(16))
	router.POST("/v1/responses", func(ctx *gin.Context) { ctx.Status(200) })
	router.GET("/v1/panic", func(ctx *gin.Context) { panic("secret panic details") })
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(strings.Repeat("secret-body", 100))))
	if recorder.Code != http.StatusRequestEntityTooLarge {
		test.Fatalf("body rejection changed: %d", recorder.Code)
	}
	router.ServeHTTP(httptest.NewRecorder(), httptest.NewRequest(http.MethodGet, "/v1/panic", nil))
	page := serviceErrorTestPage(test, handler)
	if page.Summary.Total != 2 || page.Summary.Status5xx != 1 {
		test.Fatalf("missing early service error: %+v", page)
	}
	for _, event := range page.Items {
		if strings.Contains(event.Message, "secret") {
			test.Fatalf("request or panic body persisted: %+v", event)
		}
	}
}

func TestServiceErrorsWebSocketFramesBeforeClose(test *testing.T) {
	handler := newServiceErrorTestHandler(test)
	router := gin.New()
	router.Use(handler.ServiceErrorMiddleware())
	router.GET("/v1/responses", handler.APIKeyAuthMiddleware(), func(ctx *gin.Context) {
		connection, err := responsesWSUpgrader.Upgrade(ctx.Writer, ctx.Request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		for {
			_, body, err := connection.ReadMessage()
			if err != nil {
				return
			}
			resetServiceErrorFrame(ctx)
			resetUpstreamRequestTrace(ctx)
			captureUsageRequestIngress(ctx, body)
			diagnostics := usageRequestDiagnosticState(ctx)
			diagnostics.Resolved = &usageRequestResolution{ThreadSource: "subagent", RequestKind: "turn", SubagentKind: "guardian", Passive: true, Related: true, RootFingerprint: "root-fingerprint"}
			diagnostics.Recent.Scope = "scope-same-user"
			_ = writeAuditedResponsesWSError(ctx, connection, api.NewAPIError(api.ErrCodeRateLimitReached, "background concurrency full", api.ErrorTypeRateLimit))
		}
	})
	server := httptest.NewServer(router)
	defer server.Close()
	connection, response, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	if err != nil {
		test.Fatal(err)
	}
	defer connection.Close()
	if response.StatusCode != 101 {
		test.Fatalf("upgrade failed: %d", response.StatusCode)
	}
	for range 2 {
		if err := connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create","model":"gpt-6-astra","input":"private prompt"}`)); err != nil {
			test.Fatal(err)
		}
		if _, _, err := connection.ReadMessage(); err != nil {
			test.Fatal(err)
		}
	}
	page := serviceErrorTestPage(test, handler)
	if page.Summary.Total != 2 || page.Items[0].RequestID == page.Items[1].RequestID {
		test.Fatalf("per-turn errors not persisted before WS close: %+v", page)
	}
	for _, event := range page.Items {
		if event.Transport != "websocket" || event.StatusCode != 429 || event.RequestType != "related_internal" || event.SubagentKind != "guardian" || event.ScopeHash != "scope-same-user" {
			test.Fatalf("missing WS context: %+v", event)
		}
	}
}

func TestServiceErrorWriterPreservesSuccessfulStream(test *testing.T) {
	recorder := httptest.NewRecorder()
	ctx, _ := gin.CreateTestContext(recorder)
	writer := &serviceErrorResponseWriter{ResponseWriter: ctx.Writer}
	body := strings.Repeat("private successful output", 10000)
	_, _ = io.WriteString(writer, body)
	writer.Flush()
	if recorder.Body.String() != body || len(writer.body) != 0 {
		test.Fatal("successful stream was buffered or changed")
	}
	recorder = httptest.NewRecorder()
	ctx, _ = gin.CreateTestContext(recorder)
	writer = &serviceErrorResponseWriter{ResponseWriter: ctx.Writer}
	writer.WriteHeader(429)
	_, _ = io.WriteString(writer, body)
	if len(writer.body) != 8192 || recorder.Body.String() != body {
		test.Fatal("error buffer unbounded or client body changed")
	}
}

func TestServiceErrorUpstreamClassification(test *testing.T) {
	for _, code := range []api.ErrorCode{"upstream_429", "account_pool_unauthorized", "slow_down", "server_is_overloaded", "usage_limit_reached"} {
		if !serviceErrorIsUpstream(api.NewAPIError(code, "upstream", api.ErrorTypeServer)) {
			test.Errorf("upstream code classified local: %s", code)
		}
	}
	if serviceErrorIsUpstream(api.NewAPIError(api.ErrCodeAccountSessionCapacity, "local admission", api.ErrorTypeInvalidRequest)) {
		test.Fatal("local account admission classified as upstream")
	}
}

func TestServiceErrorsKeyQuotaKeepsCallerBeforeAuthenticationCompletes(test *testing.T) {
	handler := newServiceErrorTestHandler(test)
	keyID, err := handler.db.InsertAPIKeyWithOptions(context.Background(), database.APIKeyInput{
		Key: "quota-secret-never-log", Name: "quota caller", QuotaLimit: 1, QuotaUsed: 1,
	})
	if err != nil {
		test.Fatal(err)
	}
	router := gin.New()
	router.Use(handler.ServiceErrorMiddleware())
	router.POST("/v1/responses", handler.APIKeyAuthMiddleware(), func(ctx *gin.Context) { ctx.Status(200) })
	request := httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	request.Header.Set("Authorization", "Bearer quota-secret-never-log")
	request.Header.Set("X-NewAPI-Request-ID", "2026091007254567890123unsigned")
	request.Header.Set("X-Codex-Turn-Metadata", `{"thread_source":"subagent","request_kind":"turn","subagent_kind":"guardian","installation_id":"`+testRootSessionA+`"}`)
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, request)
	if recorder.Code != 429 {
		test.Fatalf("quota response changed: %d %s", recorder.Code, recorder.Body.String())
	}
	page := serviceErrorTestPage(test, handler)
	if len(page.Items) != 1 {
		test.Fatalf("missing quota error: %+v", page)
	}
	event := page.Items[0]
	if event.APIKeyID != keyID || event.APIKeyName != "quota caller" || event.NewAPIIdentityVerified || event.RequestType != "unknown" || event.NewAPIRequestID != "2026091007254567890123unsigned" || event.ThreadSource != "subagent" {
		test.Fatalf("incorrect pre-authentication identity: %+v", event)
	}
	if event.RequestID != recorder.Header().Get("X-Codex2API-Request-ID") {
		test.Fatal("request correlation header does not match log")
	}
	if event.ClientInfo["turn_metadata_header.installation_id"] != testRootSessionA {
		test.Fatalf("missing pre-auth device diagnostics: %+v", event.ClientInfo)
	}
}

func TestServiceErrorsCommittedSSEDispatchFailure(test *testing.T) {
	handler := newServiceErrorTestHandler(test)
	router := gin.New()
	router.Use(handler.ServiceErrorMiddleware())
	router.POST("/v1/responses", handler.APIKeyAuthMiddleware(), func(ctx *gin.Context) {
		ctx.Header("Content-Type", "text/event-stream")
		_, _ = ctx.Writer.WriteString(": keepalive\n\n")
		ctx.Writer.Flush()
		beginDispatchSelection(ctx)
		handler.sendDispatchUnavailable(ctx, true, false)
	})
	recorder := httptest.NewRecorder()
	router.ServeHTTP(recorder, httptest.NewRequest(http.MethodPost, "/v1/responses", nil))
	if recorder.Code != 200 || !strings.Contains(recorder.Body.String(), "response.failed") {
		test.Fatalf("stream protocol changed: %d %s", recorder.Code, recorder.Body.String())
	}
	page := serviceErrorTestPage(test, handler)
	if len(page.Items) != 1 || page.Items[0].Transport != "sse" || page.Items[0].StatusCode != 503 || page.Items[0].Stage != "dispatch" {
		test.Fatalf("missing committed SSE failure: %+v", page)
	}
}
