package proxy

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
)

func TestUsageTurnStateUsesActualHTTPOutboundWithoutResponseValue(t *testing.T) {
	h := newWindowAuthorizationHandler(t)
	account := &auth.Account{DBID: 71, AccountID: "a"}
	h.store.AddAccount(account)
	outbound := base64.URLEncoding.EncodeToString(make([]byte, 217))
	newResponse := base64.URLEncoding.EncodeToString(make([]byte, 128))
	for _, tc := range []struct {
		name, returned string
		length, bytes  int
	}{
		{"no_returned_value", "", 292, 217},
		{"response_value_wins", newResponse, len(newResponse), 128},
		{"non_base64_response_wins", "not-base64!", len("not-base64!"), 0},
	} {
		t.Run(tc.name, func(t *testing.T) {
			seen := make(chan string, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				_, _ = io.Copy(io.Discard, r.Body)
				seen <- r.Header.Get(codexTurnStateHeader)
				if tc.returned != "" {
					w.Header().Set(codexTurnStateHeader, tc.returned)
				}
				w.Header().Set("Content-Type", "application/json")
				_, _ = io.WriteString(w, `{"id":"response-test"}`)
			}))
			defer server.Close()
			c, _, _ := aliasRequest(t, h, 101, "turn", "", false)
			attachUpstreamTrace(c, h.store)
			beginUsageSelectionAttempt(c, 1)
			body := []byte(`{"input":[]}`)
			request, err := http.NewRequestWithContext(c.Request.Context(), http.MethodPost, server.URL+"/responses", strings.NewReader(string(body)))
			require.NoError(t, err)
			request.Header.Set(codexTurnStateHeader, outbound)
			response, err := doTracedUpstreamRequest(server.Client(), request, account, "", body)
			require.NoError(t, err)
			require.NoError(t, maskTurnStateResponse(c.Request.Context(), account, response))
			_, err = io.Copy(io.Discard, response.Body)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			require.Equal(t, outbound, <-seen)
			usage := &database.UsageLogInput{AccountID: account.ID(), StatusCode: 200}
			populateUpstreamTrace(c, usage)
			populateUsageRequestDiagnostics(c, usage)
			require.NotNil(t, usage.TurnStateLength)
			require.Equal(t, tc.length, *usage.TurnStateLength)
			if tc.bytes == 0 {
				require.Nil(t, usage.TurnStateDecodedBytes, "must not keep decoded bytes from the fallback value")
			} else {
				require.Equal(t, tc.bytes, *usage.TurnStateDecodedBytes)
			}
		})
	}
}

func TestUsageTurnStateOutboundFrameAndRetryIsolation(t *testing.T) {
	c := transportTestContext()
	beginUsageSelectionAttempt(c, 1)
	beginUpstreamTrace(c.Request.Context(), &auth.Account{DBID: 17}, "", true)
	firstObserver := UpstreamTransportObserver(c.Request.Context())
	real := base64.URLEncoding.EncodeToString(make([]byte, 217))
	body := []byte(fmt.Sprintf(`{"input":[],"client_metadata":{"x-codex-turn-state":%q}}`, real))
	firstObserver.ResponsesInput(body, nil, "")
	log := func(accountID int64) *database.UsageLogInput {
		usage := &database.UsageLogInput{AccountID: accountID}
		populateUpstreamTrace(c, usage)
		populateUsageRequestDiagnostics(c, usage)
		return usage
	}
	require.Nil(t, log(17).TurnStateLength, "a prepared but unsent frame does not count")
	firstObserver.Phase("after_payload")
	first := log(17)
	require.Equal(t, 292, *first.TurnStateLength)
	require.Equal(t, 217, *first.TurnStateDecodedBytes)
	// Identity snapshots may be trimmed without losing the measured length.
	firstObserver.updateOutboundIdentity(func(identity *outboundIdentityDiagnostic) {
		identity.Body = nil
		identity.Truncated = true
	})
	require.Equal(t, 292, *log(17).TurnStateLength)
	beginUsageSelectionAttempt(c, 2)
	beginUpstreamTrace(c.Request.Context(), &auth.Account{DBID: 18}, "", true)
	secondObserver := UpstreamTransportObserver(c.Request.Context())
	secondObserver.OutboundWebsocketHandshake(CaptureOutboundIdentityHeaders(http.Header{codexTurnStateHeader: {real}}))
	secondObserver.ResponsesInput([]byte(`{"input":[]}`), nil, "") // Account switch cleared state.
	secondObserver.Phase("after_payload")
	firstObserver.ResponsesInput(body, nil, "") // Late first-attempt callback must be ignored.
	second := log(18)
	require.Zero(t, *second.TurnStateLength)
	require.Nil(t, second.TurnStateDecodedBytes)
	require.Equal(t, 292, *first.TurnStateLength, "saved first-attempt values must stay immutable")
	require.Nil(t, log(17).TurnStateLength, "do not borrow the current account's snapshot for another account")
}

func TestUsageTurnStateCapturesRealUpstreamOnly(t *testing.T) {
	h := newWindowAuthorizationHandler(t)
	account := &auth.Account{DBID: 71, AccountID: "a"}
	h.store.AddAccount(account)
	real := base64.URLEncoding.EncodeToString(make([]byte, 217))
	for _, carrier := range []string{"header", "metadata", "codex.metadata", "array", "absent", "invalid_base64", "persistence_failed"} {
		t.Run(carrier, func(t *testing.T) {
			c, _, _ := aliasRequest(t, h, 101, "turn", "unrelated-client-token", false)
			beginUsageSelectionAttempt(c, 1)
			var before database.UsageLogInput
			populateUsageRequestDiagnostics(c, &before)
			require.Nil(t, before.TurnStateLength, "ingress must not count as upstream observation")
			value := real
			if carrier == "invalid_base64" {
				value = "not-base64!"
			}
			response := &http.Response{Header: make(http.Header), Body: io.NopCloser(strings.NewReader(""))}
			switch carrier {
			case "metadata", "codex.metadata", "array":
				eventType, headerValue := "response.metadata", fmt.Sprintf("%q", value)
				if carrier == "codex.metadata" {
					eventType = "codex.response.metadata"
				}
				if carrier == "array" {
					headerValue = "[" + headerValue + "]"
				}
				response.Header.Set("Content-Type", "text/event-stream")
				response.Body = io.NopCloser(strings.NewReader(fmt.Sprintf("data: {\"type\":%q,\"headers\":{\"X-Codex-Turn-State\":%s}}\n\n", eventType, headerValue)))
			case "absent":
			default:
				response.Header.Set(codexTurnStateHeader, value)
			}
			ctx := c.Request.Context()
			if carrier == "persistence_failed" {
				var cancel context.CancelFunc
				ctx, cancel = context.WithCancel(ctx)
				cancel()
			}
			err := maskTurnStateResponse(ctx, account, response)
			if carrier == "persistence_failed" {
				require.Error(t, err)
			} else {
				require.NoError(t, err)
				_, err = io.ReadAll(response.Body)
				require.NoError(t, err)
			}
			var usage database.UsageLogInput
			populateUsageRequestDiagnostics(c, &usage)
			require.NotNil(t, usage.TurnStateLength)
			switch carrier {
			case "absent":
				require.Equal(t, 0, *usage.TurnStateLength)
				require.Nil(t, usage.TurnStateDecodedBytes)
			case "invalid_base64":
				require.Equal(t, len(value), *usage.TurnStateLength)
				require.Nil(t, usage.TurnStateDecodedBytes)
			default:
				require.Equal(t, 292, *usage.TurnStateLength)
				require.Equal(t, 217, *usage.TurnStateDecodedBytes)
			}
		})
	}
}

func TestUsageTurnStateAttemptIsolationAndFirstValue(t *testing.T) {
	c, _ := newTurnStateTestContext(t)
	beginUsageSelectionAttempt(c, 1)
	first := c.Request.Context()
	observeUsageTurnState(first, "")
	observeUsageTurnState(first, "first-value!")
	observeUsageTurnState(first, "later-value-with-different-length!")
	var previous database.UsageLogInput
	populateUsageTurnState(c, &previous)
	require.Equal(t, len("first-value!"), *previous.TurnStateLength)
	beginUsageSelectionAttempt(c, 2)
	observeUsageTurnState(first, "late-event-from-old-account")
	var next database.UsageLogInput
	populateUsageTurnState(c, &next)
	require.Nil(t, next.TurnStateLength)
	observeUsageTurnState(c.Request.Context(), "")
	populateUsageTurnState(c, &next)
	require.Equal(t, 0, *next.TurnStateLength)
	require.Equal(t, len("first-value!"), *previous.TurnStateLength)
}
