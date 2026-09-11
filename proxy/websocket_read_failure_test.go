package proxy

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func resetMessageTooBigRoutingForTest(test *testing.T) {
	test.Helper()
	settings := CurrentRuntimeSettings()
	updated := settings
	updated.CodexWSSizeRouter = true
	ApplyRuntimeSettings(updated)
	test.Setenv("CODEX_WS_SIZE_ROUTER", "")
	globalWSSizeRouter.mu.Lock()
	previousSize, previousTime := globalWSSizeRouter.minTooBig, globalWSSizeRouter.learnedAt
	globalWSSizeRouter.minTooBig, globalWSSizeRouter.learnedAt = 0, time.Time{}
	globalWSSizeRouter.mu.Unlock()
	test.Cleanup(func() {
		ApplyRuntimeSettings(settings)
		globalWSSizeRouter.mu.Lock()
		globalWSSizeRouter.minTooBig, globalWSSizeRouter.learnedAt = previousSize, previousTime
		globalWSSizeRouter.mu.Unlock()
	})
}

func TestWebsocketMessageTooBigReplaySafetyAndIndependentLearning(test *testing.T) {
	peerClose := &websocket.CloseError{Code: websocket.CloseMessageTooBig}
	for _, scenario := range []struct {
		name              string
		cause             error
		started           bool
		localContinuation bool
		blocked           bool
		learned           bool
		source            string
		decision          string
	}{
		{name: "peer rejection before response", cause: peerClose, learned: true, source: "peer_close", decision: "peer_rejected_before_response"},
		{name: "peer close after response", cause: peerClose, started: true, blocked: true, learned: true, source: "peer_close", decision: "blocked_response_already_started"},
		{name: "local receive limit", cause: fmt.Errorf("%w: %w", peerClose, websocket.ErrReadLimit), blocked: true, learned: true, source: "local_read_limit", decision: "blocked_local_receive_limit"},
		{name: "ambiguous disconnect", cause: io.ErrUnexpectedEOF, blocked: true, decision: "blocked_ambiguous_delivery"},
		{name: "connection local continuation", cause: peerClose, localContinuation: true, blocked: true, learned: true, source: "peer_close", decision: "blocked_connection_local_continuation"},
		{name: "already ambiguous write", cause: BlockTransportReplay(peerClose), blocked: true, learned: true, source: "peer_close", decision: "blocked_ambiguous_delivery"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			resetMessageTooBigRoutingForTest(test)
			request := transportTestContext()
			beginUpstreamTrace(request.Request.Context(), &auth.Account{DBID: 1549}, "", true)
			observer := UpstreamTransportObserver(request.Request.Context())
			body := []byte(`{"input":"` + strings.Repeat("x", 128*1024) + `"}`)
			observer.ResponsesInput(body, nil, "/v1/responses")
			observer.Phase("after_payload")
			if scenario.localContinuation {
				observer.Continuation("connection_local", "original_connection")
			}
			failure := observer.WebsocketReadFailure(scenario.cause, scenario.started, scenario.localContinuation)
			require.Equal(test, scenario.blocked, TransportReplayBlocked(failure))
			require.Equal(test, scenario.blocked, TransportReplayBlocked(observer.TransportError(failure)))
			outcome := classifyStreamOutcome(nil, failure, nil, false)
			require.Equal(test, !scenario.blocked, shouldFallbackWebsocketMessageTooBigToHTTP(outcome, true, false, nil, nil))
			require.False(test, shouldFallbackWebsocketMessageTooBigToHTTP(outcome, true, true, nil, nil))
			require.False(test, shouldFallbackWebsocketMessageTooBigToHTTP(outcome, true, false, context.Canceled, nil))
			require.Equal(test, scenario.learned, globalWSSizeRouter.PreferHTTP(len(body)))
			diagnostic := snapshotUpstreamTrace(request.Request.Context()).Transport
			require.Equal(test, scenario.source, diagnostic.MessageTooBigSource)
			require.Equal(test, scenario.blocked, diagnostic.ReplayBlocked)
			require.Equal(test, scenario.decision, diagnostic.ReplayDecision)
			require.Equal(test, scenario.learned, diagnostic.HTTPSizeRouteLearned)
			if scenario.source != "" {
				require.Equal(test, "message_too_big", diagnostic.FailureCategory)
			}
		})
	}
}

func TestMessageTooBigFallbackRetainsAccountAndSubsequentRequestsSkipWS(test *testing.T) {
	resetMessageTooBigRoutingForTest(test)
	previousExecutor, previousResin := WebsocketExecuteFunc, resinCfg.Load()
	test.Cleanup(func() { WebsocketExecuteFunc = previousExecutor; resinCfg.Store(previousResin) })
	settings := CurrentRuntimeSettings()
	settings.CodexForceWebsocket = true
	ApplyRuntimeSettings(settings)
	var websocketCalls, httpCalls atomic.Int32
	WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, body []byte, sessionID, proxyURL, apiKey string, device *DeviceProfileConfig, headers http.Header, poolKey string) (*http.Response, error) {
		websocketCalls.Add(1)
		require.EqualValues(test, 1, account.ID())
		observer := UpstreamTransportObserver(ctx)
		observer.ResponsesInput(body, headers, "/v1/responses")
		observer.Phase("after_payload")
		failure := observer.WebsocketReadFailure(&websocket.CloseError{Code: websocket.CloseMessageTooBig}, false, false)
		return &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": []string{"text/event-stream"}}, Body: errReadCloser{err: failure}}, nil
	}
	accounts := make(chan string, 2)
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		httpCalls.Add(1)
		accounts <- request.Header.Get("X-Resin-Account")
		writer.Header().Set("Content-Type", "text/event-stream")
		_, _ = io.WriteString(writer, "data: "+`{"type":"response.completed","response":{"status":"completed","output":[],"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}}}`+"\n\n")
	}))
	test.Cleanup(upstream.Close)
	SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "size-routing-test"})
	store := auth.NewStore(nil, nil, &database.SystemSettings{MaxConcurrency: 1, TestConcurrency: 1, TestModel: "gpt-5.5"})
	test.Cleanup(store.Stop)
	primary := &auth.Account{DBID: 1, AccessToken: "fixture-primary", PlanType: "pro", AccountID: "fixture-primary"}
	primary.SetDispatchCountLimit(1)
	secondary := &auth.Account{DBID: 2, AccessToken: "fixture-secondary", PlanType: "free", AccountID: "fixture-secondary"}
	store.AddAccount(primary)
	store.AddAccount(secondary)
	handler := NewHandler(store, nil, &config.Config{AllowAnonymousV1: true}, nil)
	body := `{"model":"gpt-5.5","stream":true,"input":"` + strings.Repeat("x", 128*1024) + `"}`
	for turn := 0; turn < 2; turn++ {
		recorder := httptest.NewRecorder()
		request, _ := gin.CreateTestContext(recorder)
		request.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(body))
		handler.Responses(request)
		require.Equal(test, http.StatusOK, recorder.Code)
		require.Contains(test, recorder.Body.String(), "response.completed")
		if turn == 0 {
			require.Equal(test, "1", <-accounts)
			require.EqualValues(test, 1, atomic.LoadInt64(&primary.TotalRequests))
			require.Zero(test, atomic.LoadInt64(&secondary.TotalRequests))
		}
	}
	require.EqualValues(test, 1, websocketCalls.Load())
	require.EqualValues(test, 2, httpCalls.Load())
	require.Zero(test, atomic.LoadInt64(&primary.ActiveRequests))
	require.Zero(test, atomic.LoadInt64(&secondary.ActiveRequests))
}
