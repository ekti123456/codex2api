package proxy

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func sessionOperationsTestMeta(session string) newAPIPolicyMeta {
	return newAPIPolicyMeta{RootSessionVersion: 1, RootSessionState: newAPIPolicyRootSessionResolved, RootSessionRelation: newAPIPolicyRootSessionRelationRoot, RootSessionID: session, RootSessionFingerprint: newAPIRootSessionFingerprint("test-platform", "42", session), ThreadSource: "user", RequestKind: "turn"}
}

func TestSessionOperationsOnlyFinal500AndNoDuplicateCollection(test *testing.T) {
	handler := newWindowAuthorizationHandler(test)
	meta := sessionOperationsTestMeta(continuityTestThread)
	for _, status := range []int{500, 503, 429, 400, 200} {
		test.Run(fmt.Sprint(status), func(test *testing.T) {
			_, body := continuityTestRequest(0, "turn")
			request, _ := signedRootlessPassiveModelContext(test, http.MethodPost, "/v1/responses", body, meta)
			finish := handler.beginServiceErrorAudit(request)
			captureUsageRequestIngress(request, body)
			handler.primeNewAPIPolicyContext(request, body)
			handler.resolveRequestSessionIdentityForContext(request, body)
			identity, known := sessionOperationsIdentity(request)
			require.True(test, known)
			require.Equal(test, continuityTestThread, identity.SessionID)
			require.Equal(test, "42", identity.UserID)
			request.Set("apiKey", "known-private-key")
			failure := api.NewAPIError(api.ErrorCode(overloadErrorCode), "failure known-private-key", api.ErrorType("service_unavailable_error"))
			api.SendErrorWithStatus(request, failure, status)
			api.ObserveError(request, status, failure)
			finish()
		})
	}
	require.Eventually(test, func() bool { return handler.db.SessionErrorCollectorStats().Pending == 0 }, 5*time.Second, 10*time.Millisecond)
	page, err := handler.db.ListSessionErrors(context.Background(), database.SessionErrorQuery{})
	require.NoError(test, err)
	require.Equal(test, int64(1), page.Errors)
	require.Len(test, page.Items, 1)
	require.NotContains(test, page.Items[0].Latest.Message, "known-private-key")

	request, _ := gin.CreateTestContext(httptest.NewRecorder())
	request.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	finish := handler.beginServiceErrorAudit(request)
	request.Set(sessionOperationsContextKey, page.Items[0].Identity)
	rememberSessionErrorUsage(request, &database.UsageLogInput{StatusCode: 500, ErrorMessage: "server_is_overloaded · retried attempt", IsRetryAttempt: true})
	rememberSessionErrorUsage(request, &database.UsageLogInput{StatusCode: 200})
	finish()
	require.Equal(test, uint64(1), handler.db.SessionErrorCollectorStats().Written)
}

func TestSessionOperationsBlacklistBlocksHTTPAndForkWebSocketBeforeDispatch(test *testing.T) {
	handler := newWindowAuthorizationHandler(test)
	meta := sessionOperationsTestMeta(continuityTestThread)
	rootKey := sessionOperationKey("newapi", "test-platform", "42", meta.RootSessionFingerprint)
	require.True(test, handler.db.EnqueueSessionError(database.SessionErrorEvent{Identity: database.SessionErrorIdentity{Key: rootKey, Kind: "newapi", Platform: "test-platform", UserID: "42", SessionID: continuityTestThread}, CreatedAt: time.Now()}))
	require.Eventually(test, func() bool { return handler.db.SessionErrorCollectorStats().Pending == 0 }, 5*time.Second, 10*time.Millisecond)
	require.NoError(test, handler.db.SetSessionBlacklist(context.Background(), []string{rootKey}, true))
	_, body := continuityTestRequest(0, "turn")
	body = []byte(strings.Replace(string(body), `"model":`, `"type":"response.create","input":"continue","model":`, 1))
	test.Run("main_http", func(test *testing.T) {
		request, recorder := signedRootlessPassiveModelContext(test, http.MethodPost, "/v1/responses", body, meta)
		handler.Responses(request)
		require.Equal(test, http.StatusBadRequest, recorder.Code, recorder.Body.String())
		require.Equal(test, "session_blacklisted", gjson.GetBytes(recorder.Body.Bytes(), "error.code").String())
		require.Equal(test, "false", recorder.Header().Get("X-Should-Retry"))
	})

	forkID := "01a084f2-7593-7372-a60c-f648ce2eb337"
	forkMeta := sessionOperationsTestMeta(forkID)
	forkMeta.RootSessionRelation = newAPIPolicyRootSessionRelationRelated
	forkMeta.ForkedFromSessionFingerprint = meta.RootSessionFingerprint
	forkBody := []byte(strings.ReplaceAll(string(body), continuityTestThread, forkID))
	forkBody = []byte(strings.Replace(string(forkBody), `"thread_source":"user"`, `"forked_from_thread_id":"`+continuityTestThread+`","thread_source":"user"`, 1))
	router := gin.New()
	observed := make(chan database.SessionErrorIdentity, 1)
	router.GET("/v1/responses", func(ctx *gin.Context) {
		defer func() {
			identity, _ := sessionOperationsIdentity(ctx)
			observed <- identity
		}()
		ctx.Set(contextAPIKeyID, int64(101))
		handler.ResponsesWebSocket(ctx)
	})
	server := httptest.NewServer(router)
	defer server.Close()
	request, _ := signedRootlessPassiveModelContext(test, http.MethodGet, "/v1/responses", nil, forkMeta)
	connection, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", request.Request.Header)
	require.NoError(test, err)
	defer connection.Close()
	require.NoError(test, connection.WriteMessage(websocket.TextMessage, forkBody))
	require.NoError(test, connection.SetReadDeadline(time.Now().Add(5*time.Second)))
	_, response, err := connection.ReadMessage()
	require.NoError(test, err)
	require.NoError(test, connection.Close())
	select {
	case identity := <-observed:
		require.Equal(test, "newapi", identity.Kind)
		require.Equal(test, rootKey, identity.ParentKey)
		require.Equal(test, "session_blacklisted", gjson.GetBytes(response, "error.code").String(), string(response))
	case <-time.After(5 * time.Second):
		test.Fatal("websocket handler did not exit")
	}
}

func TestSessionOperationsIdentitySeparatesUsersAndSessionsAndRejectsSpoofedCanonicalID(test *testing.T) {
	handler := newRootlessPassiveModelTestHandler(test)
	request, body := continuityTestRequest(0, "turn")
	root := requestRootSessionIdentity{stable: true, sessionID: continuityTestThread}
	request.Set(contextAPIKeyID, int64(17))
	handler.captureSessionOperationsIdentity(request, body, root, verifiedNewAPIPolicyContext{Identity: newAPIIdentity{UserID: "forged"}}, false)
	first, known := sessionOperationsIdentity(request)
	require.True(test, known)
	require.Equal(test, "17", first.UserID)
	request.Set(contextAPIKeyID, int64(18))
	handler.captureSessionOperationsIdentity(request, body, root, verifiedNewAPIPolicyContext{}, false)
	otherUser, _ := sessionOperationsIdentity(request)
	require.NotEqual(test, first.Key, otherUser.Key)
	root.sessionID = "01a084f2-7593-7372-a60c-f648ce2eb337"
	handler.captureSessionOperationsIdentity(request, body, root, verifiedNewAPIPolicyContext{}, false)
	otherSession, _ := sessionOperationsIdentity(request)
	require.NotEqual(test, otherUser.Key, otherSession.Key)
	meta := sessionOperationsTestMeta(continuityTestThread)
	meta.RootSessionID = root.sessionID
	request, _ = signedRootlessPassiveModelContext(test, http.MethodPost, "/v1/responses", body, meta)
	handler.primeNewAPIPolicyContext(request, body)
	resolved := handler.resolveRequestRootSessionIdentityForContext(request, body)
	require.True(test, resolved.conflict)
}

func TestSessionOperationsRawHTTP500AndSeparateWebSocketFrames(test *testing.T) {
	handler := newWindowAuthorizationHandler(test)
	identity := database.SessionErrorIdentity{Key: sessionOperationKey("api_key", "codex-local", "17", continuityTestThread), Kind: "api_key", Platform: "codex-local", UserID: "17", SessionID: continuityTestThread}
	request, _ := gin.CreateTestContext(httptest.NewRecorder())
	request.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	finish := handler.beginServiceErrorAudit(request)
	request.Set(sessionOperationsContextKey, identity)
	request.Writer.WriteHeader(500)
	_, err := request.Writer.WriteString(`{"error":{"code":"server_is_overloaded","message":"raw upstream 500"}}`)
	require.NoError(test, err)
	finish()
	for index := 0; index < 2; index++ {
		resetServiceErrorFrame(request)
		request.Set(sessionOperationsContextKey, identity)
		failure := api.NewAPIError(api.ErrorCode(overloadErrorCode), "frame failure", api.ErrorTypeUpstream)
		api.ObserveError(request, 500, failure)
		rememberSessionErrorUsage(request, &database.UsageLogInput{StatusCode: 500, ErrorMessage: "server_is_overloaded · frame failure"})
		handler.finishSessionErrorAudit(request)
	}
	resetServiceErrorFrame(request)
	api.ObserveError(request, 500, api.NewAPIError(api.ErrorCode(overloadErrorCode), "no frame identity", api.ErrorTypeUpstream))
	require.Eventually(test, func() bool { return handler.db.SessionErrorCollectorStats().Pending == 0 }, 5*time.Second, 10*time.Millisecond)
	page, err := handler.db.ListSessionErrors(context.Background(), database.SessionErrorQuery{})
	require.NoError(test, err)
	require.Equal(test, int64(3), page.Errors)
	require.Equal(test, "websocket", page.Items[0].Latest.Transport)
}

func TestSessionOperationsFinalHTTPStatusOverridesAttemptUsage(test *testing.T) {
	handler := newWindowAuthorizationHandler(test)
	identity := database.SessionErrorIdentity{Key: sessionOperationKey("api_key", "codex-local", "17", continuityTestThread), Kind: "api_key", Platform: "codex-local", UserID: "17", SessionID: continuityTestThread}
	for _, status := range []int{400, 429, 503, 500} {
		request, _ := gin.CreateTestContext(httptest.NewRecorder())
		request.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		finish := handler.beginServiceErrorAudit(request)
		request.Set(sessionOperationsContextKey, identity)
		rememberSessionErrorUsage(request, &database.UsageLogInput{StatusCode: 500, ErrorMessage: "server_is_overloaded · earlier attempt", IsRetryAttempt: true})
		request.Writer.WriteHeader(status)
		_, err := request.Writer.WriteString(`{"error":{"code":"server_is_overloaded","message":"final response"}}`)
		require.NoError(test, err)
		finish()
	}
	require.Eventually(test, func() bool { return handler.db.SessionErrorCollectorStats().Pending == 0 }, 5*time.Second, 10*time.Millisecond)
	page, err := handler.db.ListSessionErrors(context.Background(), database.SessionErrorQuery{})
	require.NoError(test, err)
	require.Equal(test, int64(1), page.Errors)
	require.Equal(test, overloadErrorCode, page.Items[0].Latest.Code)
	require.Equal(test, "final response", page.Items[0].Latest.Message)

	request, _ := gin.CreateTestContext(httptest.NewRecorder())
	request.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	finish := handler.beginServiceErrorAudit(request)
	request.Set(sessionOperationsContextKey, identity)
	request.Header("Content-Type", "text/event-stream")
	rememberSessionErrorUsage(request, &database.UsageLogInput{StatusCode: 500, ErrorMessage: "server_is_overloaded · stream failed"})
	finish()
	require.Eventually(test, func() bool { return handler.db.SessionErrorCollectorStats().Pending == 0 }, 5*time.Second, 10*time.Millisecond)
	page, err = handler.db.ListSessionErrors(context.Background(), database.SessionErrorQuery{})
	require.NoError(test, err)
	require.Equal(test, int64(2), page.Errors)
	require.Equal(test, "sse", page.Items[0].Latest.Transport)
}

func TestSessionOperationsOnlyExactOverloadErrorsQualify(test *testing.T) {
	handler := newWindowAuthorizationHandler(test)
	identity := database.SessionErrorIdentity{Key: sessionOperationKey("api_key", "codex-local", "17", continuityTestThread), Kind: "api_key", Platform: "codex-local", UserID: "17", SessionID: continuityTestThread}
	for _, sample := range []struct {
		code    api.ErrorCode
		message string
	}{
		{api.ErrCodeServerError, "generic 500"},
		{api.ErrCodeUpstreamTimeout, "upstream timed out"},
		{"slow_down", "another capacity code"},
		{api.ErrCodeUpstreamError, "Our servers are currently overloaded. Please try again later."},
		{api.ErrCodeUpstreamError, "other_error · service_unavailable_error · mentions server_is_overloaded"},
		{"server_is_overloaded_extra", "not an exact code"},
	} {
		request, _ := gin.CreateTestContext(httptest.NewRecorder())
		request.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
		finish := handler.beginServiceErrorAudit(request)
		request.Set(sessionOperationsContextKey, identity)
		rememberSessionErrorUsage(request, &database.UsageLogInput{StatusCode: 500, ErrorMessage: sample.message})
		api.SendErrorWithStatus(request, api.NewAPIError(sample.code, sample.message, api.ErrorTypeServer), 500)
		finish()
	}
	require.Zero(test, handler.db.SessionErrorCollectorStats().Pending)
	require.Zero(test, handler.db.SessionErrorCollectorStats().Written)
	require.True(test, isSessionOverloadUsageMessage("server_is_overloaded"))
	require.False(test, isSessionOverloadUsageMessage("server_is_overloaded_extra · failure"))

	request, _ := gin.CreateTestContext(httptest.NewRecorder())
	request.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	finish := handler.beginServiceErrorAudit(request)
	request.Set(sessionOperationsContextKey, identity)
	rememberSessionErrorUsage(request, &database.UsageLogInput{StatusCode: 500, ErrorMessage: "server_is_overloaded · service_unavailable_error · Our servers are currently overloaded. Please try again later.", IsRetryAttempt: false})
	request.Writer.WriteHeader(400)
	_, err := request.Writer.WriteString(codexCapacityTestBody)
	require.NoError(test, err)
	finish()
	require.Eventually(test, func() bool { return handler.db.SessionErrorCollectorStats().Pending == 0 }, 5*time.Second, 10*time.Millisecond)
	page, err := handler.db.ListSessionErrors(context.Background(), database.SessionErrorQuery{})
	require.NoError(test, err)
	require.Equal(test, int64(1), page.Errors)
	require.Equal(test, overloadErrorCode, page.Items[0].Latest.Code)
}

func TestSessionOperationsWebSocketDoesNotReclassifyOtherStatusesAs500(test *testing.T) {
	handler := newWindowAuthorizationHandler(test)
	identity := database.SessionErrorIdentity{Key: sessionOperationKey("api_key", "codex-local", "17", continuityTestThread), Kind: "api_key", Platform: "codex-local", UserID: "17", SessionID: continuityTestThread}
	for _, status := range []int{200, 400, 429, 502, 503} {
		request, _ := gin.CreateTestContext(httptest.NewRecorder())
		request.Request = httptest.NewRequest(http.MethodGet, "/v1/responses", nil)
		finish := handler.beginServiceErrorAudit(request)
		resetServiceErrorFrame(request)
		request.Set(sessionOperationsContextKey, identity)
		rememberSessionErrorUsage(request, &database.UsageLogInput{StatusCode: status, ErrorMessage: "server_is_overloaded · final non-500"})
		failure := api.NewAPIError(api.ErrorCode(overloadErrorCode), "overloaded", api.ErrorTypeUpstream)
		api.ObserveError(request, api.HTTPStatusCode(failure.Code), failure)
		finish()
	}
	require.Zero(test, handler.db.SessionErrorCollectorStats().Pending)
	require.Zero(test, handler.db.SessionErrorCollectorStats().Written)
}
