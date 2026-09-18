package proxy

import (
	"context"
	"encoding/base64"
	"fmt"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
)

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
