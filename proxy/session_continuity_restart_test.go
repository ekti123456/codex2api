package proxy

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync"
	"testing"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestContinuityOffRestartsOutboundIdentityAndNumbers(t *testing.T) {
	for _, unbound := range []bool{false, true} {
		t.Run(fmt.Sprint(unbound), func(t *testing.T) {
			t.Setenv("CODEX_OUTBOUND_SESSION_MODE", "legacy")
			handler, owner, _, key := failoverTestSetup(t, false)
			if unbound {
				key = "unbound-restart::api-key:101"
			}
			config := handler.store.GetPromptFilterConfig()
			config.Advanced.Risk.SessionContinuityMode = "off"
			handler.store.SetPromptFilterConfig(config)
			previousResin := GetResinConfig()
			t.Cleanup(func() { SetResinConfig(previousResin) })
			type capture struct {
				headers http.Header
				body    []byte
			}
			seen := make(chan capture, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				seen <- capture{r.Header.Clone(), readUpstreamRequestBody(r)}
				w.Header().Set("Content-Type", "application/json")
				_, _ = w.Write([]byte(`{"id":"response-fixture"}`))
			}))
			defer server.Close()
			SetResinConfig(&ResinConfig{BaseURL: server.URL, PlatformName: "continuity-restart"})
			var lastSession string
			for i, step := range []struct{ inbound, outbound, generation uint64 }{{47, 0, 1}, {47, 0, 1}, {48, 1, 1}, {55, 0, 2}, {55, 0, 2}, {56, 1, 2}} {
				request, body := outboundEpochTestRequest(t, handler, step.inbound)
				body, _ = sjson.SetBytes(body, "previous_response_id", "old-response")
				request.Request.Header.Set("X-Codex-Turn-State", "old-turn-state")
				original := bytes.Clone(body)
				require.Nil(t, handler.configureSessionModelAffinity(request, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
				if i == 0 || i == 3 {
					routing, routingHeaders := sessionRestartRoutingContext(request, body)
					require.False(t, gjson.GetBytes(routing, "previous_response_id").Exists())
					require.Empty(t, routingHeaders.Get("X-Codex-Turn-State"))
				}
				require.Nil(t, handler.commitSessionContinuity(request, owner))
				if i == 0 || i == 3 {
					diagnostic := usageRequestDiagnosticState(request)
					require.Equal(t, "restarted", diagnostic.Continuity.Action)
					require.Equal(t, step.generation, diagnostic.AccountFailover.Generation)
					require.Equal(t, "after_restart", diagnostic.AccountFailover.Phase)
				}
				var response *http.Response
				var err error
				if i%2 == 0 {
					response, err = ExecuteRequest(request.Request.Context(), owner, body, "cache", "", "test-user-key", nil, request.Request.Header, false)
				} else {
					response, err = ExecuteCompactRequest(request.Request.Context(), owner, body, "cache", "", "test-user-key", nil, request.Request.Header)
				}
				require.NoError(t, err)
				require.NoError(t, response.Body.Close())
				sent := <-seen
				metadata := diagnosticMetadataObject(gjson.GetBytes(sent.body, "client_metadata.x-codex-turn-metadata"))
				session := sent.headers.Get("Session-Id")
				require.NotEmpty(t, session)
				require.NotEqual(t, continuityTestThread, session)
				require.Equal(t, step.outbound, metadata.Get("window_number").Uint())
				require.Equal(t, fmt.Sprintf("%s:%d", session, step.outbound), metadata.Get("window_id").String())
				require.Equal(t, session, metadata.Get("session_id").String())
				require.Equal(t, metadata.Get("window_id").String(), sent.headers.Get("X-Codex-Window-Id"))
				require.False(t, gjson.GetBytes(sent.body, "previous_response_id").Exists())
				require.Empty(t, sent.headers.Get("X-Codex-Turn-State"))
				if i == 3 {
					require.NotEqual(t, lastSession, session)
				} else if i > 0 {
					require.Equal(t, lastSession, session)
				}
				lastSession = session
				require.Equal(t, original, body)
				record, found, err := handler.db.ReadSessionContinuity(context.Background(), hashRiskIdentity(key))
				require.NoError(t, err)
				require.True(t, found)
				require.Equal(t, owner.ID(), record.AccountID)
				require.Equal(t, step.inbound, record.Number)
				require.Equal(t, step.generation, record.FailoverCount)
				// Next request must restore identity and numbering from persistence.
				handler.continuityRecords = nil
			}
		})
	}
}

func TestContinuityOffRestartEmptyContextDoesNotCommit(t *testing.T) {
	handler, owner, _, key := failoverTestSetup(t, false)
	config := handler.store.GetPromptFilterConfig()
	config.Advanced.Risk.SessionContinuityMode = "off"
	handler.store.SetPromptFilterConfig(config)
	request, body := outboundEpochTestRequest(t, handler, 47)
	body, _ = sjson.SetRawBytes(body, "input", []byte(`[{"type":"reasoning","encrypted_content":"old-cipher"}]`))
	failure := handler.prepareSessionContinuity(request, requestSessionIdentity{stableIdentity: true}, key, body)
	require.NotNil(t, failure)
	require.Equal(t, "codex_session_restart_context_required", string(failure.Code))
	require.Equal(t, http.StatusBadRequest, api.HTTPStatusCode(failure.Code))
	record, found, err := handler.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, owner.ID(), record.AccountID)
	require.Zero(t, record.FailoverCount)
}

func TestContinuityRestartConcurrentCopiesAndStaleOwner(t *testing.T) {
	handler, owner, target, key := failoverTestSetup(t, false)
	root := hashRiskIdentity(key)
	expected, _, err := handler.db.ReadSessionContinuity(t.Context(), root)
	require.NoError(t, err)
	next := expected
	next.Number = 47
	next.LastFailoverReason = "continuity_window_gap"
	var results [8]database.SessionContinuityRecord
	var failures [8]error
	var workers sync.WaitGroup
	for i := range results {
		workers.Go(func() {
			results[i], failures[i] = handler.db.RestartSessionContinuity(t.Context(), root, expected, next)
		})
	}
	workers.Wait()
	for i := range results {
		require.NoError(t, failures[i])
		require.Equal(t, expected.FailoverCount+1, results[i].FailoverCount)
	}
	first := results[0]
	numbers, err := handler.db.ResolveSessionOutboundWindowNumbers(t.Context(), root, owner.ID(), first.FailoverCount, map[string]database.SessionOutboundWindowInput{continuityTestThread: {Number: 47, ContextID: "ctx-47"}})
	require.NoError(t, err)
	require.Zero(t, numbers[continuityTestThread])
	_, _, err = handler.db.SwitchSessionContinuityAccount(t.Context(), database.SessionAccountFailover{RootKey: root, ExpectedAccountID: owner.ID(), AccountID: target.ID(), ExpectedGeneration: first.FailoverCount})
	require.NoError(t, err)
	_, err = handler.db.RestartSessionContinuity(t.Context(), root, expected, next)
	require.ErrorIs(t, err, database.ErrSessionOwnerConflict)
}
