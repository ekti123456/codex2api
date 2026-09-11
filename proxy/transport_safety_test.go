package proxy

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestTransportDeliveryBlocksFiniteAndUnlimitedReplay(test *testing.T) {
	for _, policy := range []database.ContinuousRetryPolicy{{}, {Enabled: true, CatchAll: true}} {
		failure := fmt.Errorf("outer: %w", ErrUpstream(0, "transport failed", BlockTransportReplay(io.ErrUnexpectedEOF)))
		require.ErrorIs(test, failure, io.ErrUnexpectedEOF)
		require.False(test, IsRetryableError(failure))
		require.False(test, isRetryableRequestErrorForContext(context.Background(), failure, policy))
		require.False(test, continuousRetryRequestErrorSelected(policy, failure))
		retries, rateRetries := 0, 0
		require.False(test, shouldRetryRequestError(failure, &retries, -1, policy))
		outcome := classifyStreamOutcome(nil, failure, nil, false)
		require.True(test, outcome.replayBlocked)
		require.False(test, continuousRetryStreamFailureSelected(outcome, nil, "", policy))
		require.False(test, shouldTransparentRetryStreamWithBudgets(outcome, &retries, &rateRetries, -1, -1, false, nil, nil, policy))
		require.Zero(test, retries)
	}
}

func TestHTTPDeliverySafetyDoesNotDependOnAuditMiddleware(test *testing.T) {
	for _, written := range []bool{false, true} {
		client := &http.Client{Transport: diagnosticRoundTripper(func(request *http.Request) (*http.Response, error) {
			require.Nil(test, request.GetBody)
			if written {
				_, _ = io.ReadAll(request.Body)
			}
			return nil, io.ErrUnexpectedEOF
		})}
		request, err := http.NewRequest(http.MethodPost, "http://upstream.invalid/responses", strings.NewReader("business-payload"))
		require.NoError(test, err)
		request.Header.Set("Idempotency-Key", "client-provided")
		_, err = doTracedUpstreamRequest(client, request, &auth.Account{DBID: 17}, "")
		require.ErrorIs(test, err, io.ErrUnexpectedEOF)
		require.Equal(test, written, TransportReplayBlocked(err))
	}
}

func TestHTTPResponsesEOFRequiresAnObservedTerminal(test *testing.T) {
	for _, terminal := range []bool{false, true} {
		request := transportTestContext()
		beginUpstreamTrace(request.Request.Context(), &auth.Account{DBID: 17}, "", false)
		observer := UpstreamTransportObserver(request.Request.Context())
		observer.ResponseHeaders(200, http.Header{}, false)
		payload := "data: {\"type\":\"response.created\"}\n\n"
		if terminal {
			payload += "data: {\"type\":\"response.completed\",\"response\":{\"id\":\"response-1\"}}\n\n"
		}
		body := &tracedResponseBody{ReadCloser: io.NopCloser(strings.NewReader(payload)), observer: observer, requireTerminal: true}
		err := ReadSSEStream(body, func([]byte) bool { return true })
		if terminal {
			require.NoError(test, err)
		} else {
			require.ErrorIs(test, err, io.ErrUnexpectedEOF)
			require.True(test, TransportReplayBlocked(err))
			require.True(test, snapshotUpstreamTrace(request.Request.Context()).Transport.ReplayBlocked)
		}
	}
}

func TestTransportScopeUsesOnlyVerifiedUserIdentity(test *testing.T) {
	request := transportTestContext()
	var policy verifiedNewAPIPolicyContext
	policy.APIKeyID = 7
	policy.Platform = "newapi"
	policy.MetaVerified = true
	policy.Identity.UserID = "user-a"
	policy.Meta.TokenID = 33
	bindTransportOwner(request, policy, true)
	first := WebsocketTransportOwner(request.Request.Context(), "shared-key")
	request.Request.Header.Set("X-NewAPI-User-Id", "spoofed-user")
	request.Request.Header.Set("X-Codex-Installation-Id", "spoofed-device")
	require.Equal(test, first, WebsocketTransportOwner(request.Request.Context(), "shared-key"))
	policy.Identity.UserID = "user-b"
	bindTransportOwner(request, policy, true)
	require.NotEqual(test, first, WebsocketTransportOwner(request.Request.Context(), "shared-key"))
	bindTransportOwner(request, policy, false)
	require.Equal(test, WebsocketTransportOwner(context.Background(), "shared-key"), WebsocketTransportOwner(request.Request.Context(), "shared-key"))
	require.NotEqual(test, first, WebsocketTransportOwner(request.Request.Context(), "other-key"))
	require.NotEqual(test, WebsocketTransportOwner(nil, ""), WebsocketTransportOwner(nil, ""))
	parent := WithDownstreamWebsocketConnection(request.Request.Context())
	child, cancel := context.WithCancel(parent)
	defer cancel()
	require.Equal(test, DownstreamWebsocketConnectionID(parent), DownstreamWebsocketConnectionID(child))
	require.NotEqual(test, DownstreamWebsocketConnectionID(parent), DownstreamWebsocketConnectionID(WithDownstreamWebsocketConnection(parent)))
	require.NotContains(test, first, "user-a")
	require.NotContains(test, first, "shared-key")
}

func TestTransportFailureClassificationUsesStructuredEvidence(test *testing.T) {
	for _, scenario := range []struct {
		name                                              string
		status                                            int
		identity, authorization, body, category, evidence string
	}{
		{"identity", 500, "identity_verification_required", "", `{"error":{"code":"server_is_overloaded"}}`, "identity_verification_required", "identity_error_header"},
		{"authorization", 403, "", "account_disabled", `{}`, "account_disabled", "authorization_error_header"},
		{"quota", 429, "", "", `{"error":{"type":"usage_limit_reached"}}`, "usage_limit_exhausted", "error_type"},
		{"rate", 429, "", "", `{"error":{"message":"usage_limit_reached"}}`, "rate_limited", "http_status"},
		{"plain401", 401, "", "", `{}`, "authentication", "http_status"},
		{"not_identity", 500, "unrecognized", "", `{"error":{"code":"server_is_overloaded"}}`, "unavailable", "error_code"},
		{"no_inference", 500, "", "", `{"error":{"message":"account_disabled identity_verification_required"}}`, "unavailable", "http_status"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			request := transportTestContext()
			beginUpstreamTrace(request.Request.Context(), &auth.Account{DBID: 17}, "", false)
			observer := UpstreamTransportObserver(request.Request.Context())
			headers := http.Header{}
			headers.Set("X-Error-Json", base64.StdEncoding.EncodeToString([]byte(fmt.Sprintf(`{"error":{"code":%q},"secret":"do-not-log"}`, scenario.identity))))
			headers.Set("X-OpenAI-Authorization-Error", scenario.authorization)
			observer.ResponseHeaders(scenario.status, headers, false)
			observer.HTTPErrorBody([]byte(scenario.body))
			diagnostic := snapshotUpstreamTrace(request.Request.Context()).Transport
			require.Equal(test, scenario.category, diagnostic.FailureCategory)
			require.Equal(test, scenario.evidence, diagnostic.FailureEvidence)
			require.NotContains(test, transportDiagnosticJSON(diagnostic), "do-not-log")
		})
	}
}

func TestWebsocketHandshakeIdentityDoesNotClassifyLaterTurns(test *testing.T) {
	request := transportTestContext()
	beginUpstreamTrace(request.Request.Context(), &auth.Account{DBID: 17}, "", true)
	observer := UpstreamTransportObserver(request.Request.Context())
	observer.ResponseHeaders(101, http.Header{"X-Openai-Authorization-Error": {"account_disabled"}}, true)
	observer.Event([]byte(`{"type":"error","error":{"code":"server_is_overloaded"}}`))
	diagnostic := snapshotUpstreamTrace(request.Request.Context()).Transport
	require.Equal(test, "unavailable", diagnostic.FailureCategory)
	require.Equal(test, "error_code", diagnostic.FailureEvidence)
}

func TestWebsocketLifecycleEnrichmentIsBoundedAndOwnerChecked(test *testing.T) {
	websocketLifecycleHistory.Lock()
	previous := websocketLifecycleHistory.items
	websocketLifecycleHistory.items = make(map[string]WebsocketConnectionLifecycle)
	websocketLifecycleHistory.Unlock()
	test.Cleanup(func() {
		websocketLifecycleHistory.Lock()
		websocketLifecycleHistory.items = previous
		websocketLifecycleHistory.Unlock()
	})
	websocketLifecycleHistory.Lock()
	for index := 0; index < websocketLifecycleLimit; index++ {
		id := fmt.Sprint(index)
		websocketLifecycleHistory.items[id] = WebsocketConnectionLifecycle{ConnectionID: id, ObservedAt: time.Now()}
	}
	websocketLifecycleHistory.Unlock()
	event := WebsocketConnectionLifecycle{ConnectionID: "connection-1", AccountID: 17, State: "closed", ExitReason: "heartbeat_pong_timeout", ObservedAt: time.Now()}
	RecordWebsocketLifecycle(event)
	websocketLifecycleHistory.Lock()
	count := len(websocketLifecycleHistory.items)
	websocketLifecycleHistory.Unlock()
	require.Equal(test, websocketLifecycleLimit, count)
	payload := []byte(`{"upstream":{"connection_id":"connection-1","account_id":17}}`)
	enriched := EnrichWebsocketLifecycle(payload)
	require.Equal(test, "heartbeat_pong_timeout", gjson.GetBytes(enriched, "upstream.connection_lifecycle.exit_reason").String())
	require.NotContains(test, string(payload), "connection_lifecycle")
	other := []byte(`{"upstream":{"connection_id":"connection-1","account_id":18}}`)
	require.Equal(test, other, EnrichWebsocketLifecycle(other))
	event.ObservedAt = time.Now().Add(-websocketLifecycleRetention - time.Second)
	websocketLifecycleHistory.Lock()
	websocketLifecycleHistory.items[event.ConnectionID] = event
	websocketLifecycleHistory.Unlock()
	require.Equal(test, payload, EnrichWebsocketLifecycle(payload))
}
