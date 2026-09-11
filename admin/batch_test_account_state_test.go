package admin

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/stretchr/testify/require"
)

func TestBatchTestServiceFailuresPreserveAccountState(test *testing.T) {
	for _, scenario := range []struct {
		name   string
		status int
		body   string
	}{
		{name: "HTTP 500 overload", status: 500, body: `{"error":{"code":"server_is_overloaded","type":"service_unavailable_error"}}`},
		{name: "HTTP 502", status: 502, body: "bad gateway"},
		{name: "HTTP 503", status: 503, body: "service unavailable"},
		{name: "HTTP 504", status: 504, body: "gateway timeout"},
		{name: "HTTP 598", status: 598, body: "message_too_big"},
		{name: "HTTP 400 request error", status: 400, body: `{"error":{"code":"invalid_request_error"}}`},
		{name: "failed event overload", status: 200, body: `data: {"type":"response.failed","response":{"error":{"code":"server_is_overloaded","type":"service_unavailable_error","message":"Our servers are currently overloaded"}}}` + "\n\n"},
		{name: "error event overload", status: 200, body: `data: {"type":"error","error":{"code":"server_is_overloaded","message":"upstream unavailable"}}` + "\n\n"},
		{name: "completed with failure", status: 200, body: `data: {"type":"response.completed","response":{"status":"failed","status_details":{"error":{"code":"server_error"}}}}` + "\n\n"},
		{name: "incomplete response", status: 200, body: `data: {"type":"response.completed","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"}}}` + "\n\n"},
		{name: "empty output", status: 200, body: `data: {"type":"response.completed","response":{"status":"completed","output":[]}}` + "\n\n"},
		{name: "missing terminal", status: 200, body: `data: {"type":"response.output_text.delta","delta":"partial"}` + "\n\n"},
		{name: "empty stream", status: 200},
		{name: "account words in service message", status: 200, body: `data: {"type":"response.failed","response":{"error":{"code":"server_error","message":"unauthorized account_deactivated invalid_grant"}}}` + "\n\n"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				writer.Header().Set("Content-Type", "text/event-stream")
				writer.WriteHeader(scenario.status)
				_, _ = io.WriteString(writer, scenario.body)
			}))
			test.Cleanup(upstream.Close)
			for _, initial := range []string{"ready", "cooldown", "error"} {
				test.Run(initial, func(test *testing.T) {
					store := auth.NewStore(nil, nil, nil)
					test.Cleanup(store.Stop)
					account := &auth.Account{DBID: 1, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: upstream.URL, APIKey: "fixture-key", Models: []string{"gpt-4o-mini"}, Status: auth.StatusReady, HealthTier: auth.HealthTierHealthy}
					if initial == "cooldown" {
						account.Status = auth.StatusCooldown
						account.CooldownReason = "rate_limited"
						account.CooldownUtil = time.Now().Add(time.Hour)
					} else if initial == "error" {
						account.Status = auth.StatusError
						account.ErrorMsg = "existing credential error"
					}
					store.AddAccount(account)
					beforeStatus, beforeRuntime := account.Status, account.RuntimeStatus()
					beforeMessage, beforeCooldown := account.ErrorMsg, account.CooldownUtil
					beforeReason, beforeHealth := account.CooldownReason, account.HealthTier
					handler := &Handler{store: store}
					status, message := handler.runSingleBatchTest(context.Background(), account)
					require.Equal(test, "failed", status)
					require.NotEmpty(test, message)
					require.Equal(test, beforeRuntime, account.RuntimeStatus())
					require.Equal(test, beforeStatus, account.Status)
					require.Equal(test, beforeMessage, account.ErrorMsg)
					require.Equal(test, beforeCooldown, account.CooldownUtil)
					require.Equal(test, beforeReason, account.CooldownReason)
					require.Equal(test, beforeHealth, account.HealthTier)
					require.Zero(test, account.FailureStreak)
				})
			}
		})
	}
}

func TestBatchTestTransportAndModelFailuresPreserveAccount(test *testing.T) {
	for _, scenario := range []string{"request transport", "model selection", "stream read", "error body read"} {
		test.Run(scenario, func(test *testing.T) {
			store := auth.NewStore(nil, nil, nil)
			test.Cleanup(store.Stop)
			account := &auth.Account{DBID: 1, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: "://invalid-url", APIKey: "fixture-key", Models: []string{"gpt-4o-mini"}, Status: auth.StatusReady, HealthTier: auth.HealthTierHealthy}
			store.AddAccount(account)
			handler := &Handler{store: store}
			var status, message string
			switch scenario {
			case "request transport":
				status, message = handler.runSingleBatchTest(context.Background(), account)
			case "model selection":
				account.Models = nil
				status, message = handler.runSingleBatchTest(context.Background(), account)
			case "stream read":
				response := &http.Response{StatusCode: http.StatusOK, Body: failingBatchTestBody{}}
				status, message = handler.readBatchTestStreamResult(context.Background(), account, response, "gpt-4o-mini")
			case "error body read":
				status, message = handler.handleBatchTestReadError(context.Background(), account, io.ErrUnexpectedEOF)
			}
			require.Equal(test, "failed", status)
			require.NotEmpty(test, message)
			require.Equal(test, "active", account.RuntimeStatus())
			require.Empty(test, account.ErrorMsg)
		})
	}
}

type failingBatchTestBody struct{}

func (failingBatchTestBody) Read([]byte) (int, error) { return 0, io.ErrUnexpectedEOF }

func (failingBatchTestBody) Close() error { return nil }

func TestBatchTestStreamAccountErrorsStillMarkAccount(test *testing.T) {
	for _, payload := range []string{
		`{"type":"response.failed","response":{"error":{"code":"account_deactivated"}}}`,
		`{"type":"response.failed","response":{"error":{"type":"authentication_error"}}}`,
		`{"type":"error","error":{"code":"invalid_api_key"}}`,
		`{"type":"response.completed","response":{"status":"failed","status_details":{"error":{"code":"invalid_grant"}}}}`,
	} {
		test.Run(payload, func(test *testing.T) {
			store := auth.NewStore(nil, nil, nil)
			test.Cleanup(store.Stop)
			account := &auth.Account{DBID: 1, Status: auth.StatusReady, HealthTier: auth.HealthTierHealthy}
			store.AddAccount(account)
			handler := &Handler{store: store}
			response := &http.Response{StatusCode: http.StatusOK, Body: io.NopCloser(strings.NewReader(fmt.Sprintf("data: %s\n\n", payload)))}
			status, message := handler.readBatchTestStreamResult(context.Background(), account, response, "gpt-4o-mini")
			require.Equal(test, "failed", status)
			require.NotEmpty(test, message)
			require.Equal(test, "error", account.RuntimeStatus())
		})
	}
}

func TestBatchTestFailedResultsDoNotCountAsAbnormalAccounts(test *testing.T) {
	upstream := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		switch request.Header.Get("Authorization") {
		case "Bearer fixture-http":
			writer.WriteHeader(http.StatusInternalServerError)
			_, _ = io.WriteString(writer, `{"error":{"code":"server_is_overloaded"}}`)
		case "Bearer fixture-stream":
			writer.Header().Set("Content-Type", "text/event-stream")
			_, _ = io.WriteString(writer, "data: "+`{"type":"response.failed","response":{"error":{"code":"server_is_overloaded"}}}`+"\n\n")
		default:
			writer.WriteHeader(http.StatusUnauthorized)
			_, _ = io.WriteString(writer, `{"error":{"code":"invalid_api_key"}}`)
		}
	}))
	test.Cleanup(upstream.Close)
	store := auth.NewStore(nil, nil, nil)
	test.Cleanup(store.Stop)
	var accounts []*auth.Account
	for index, key := range []string{"fixture-http", "fixture-stream", "fixture-auth"} {
		account := &auth.Account{DBID: int64(index + 1), UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: upstream.URL, APIKey: key, Models: []string{"gpt-4o-mini"}, Status: auth.StatusReady, HealthTier: auth.HealthTierHealthy}
		store.AddAccount(account)
		accounts = append(accounts, account)
	}
	handler := &Handler{store: store}
	events := make(chan batchOperationEvent, len(accounts))
	counts := handler.runBatchTest(context.Background(), accounts, 0, handler.runSingleBatchTest, func(event batchOperationEvent) { events <- event })
	close(events)
	require.EqualValues(test, 2, counts.Failed)
	require.EqualValues(test, 1, counts.Banned)
	require.Zero(test, counts.Success)
	for event := range events {
		if event.AccountID != 3 {
			require.Equal(test, "failed", event.Status)
			require.Contains(test, event.Message, "server_is_overloaded")
			require.Equal(test, event.Message, event.Error)
		}
	}
	require.Equal(test, "active", accounts[0].RuntimeStatus())
	require.Equal(test, "active", accounts[1].RuntimeStatus())
	require.Equal(test, "unauthorized", accounts[2].RuntimeStatus())
}
