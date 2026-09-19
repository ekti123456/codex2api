package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestPreservedInputReplayRetainsAuthenticatedHistoryAndRawDelta(t *testing.T) {
	resetResponseCacheStateForTest(testResponseCacheConfig())
	h, owner, target, root := responsePrivacySetup(t)
	record, _, err := h.db.SwitchSessionContinuityAccount(t.Context(), database.SessionAccountFailover{RootKey: hashRiskIdentity(root), ExpectedAccountID: owner.ID(), AccountID: target.ID(), ResetOutboundWindow: true, WindowThreadID: continuityTestThread, LossyContextRestart: true, PreserveRestartInput: true})
	require.NoError(t, err)
	h.store.UnbindSessionAffinity(root, owner.ID())
	h.store.BindSessionAffinity(root, target, "")
	producer, _, _ := responsePrivacyRequest(t, h, 101, "producer", "")
	h.attachSessionOutboundEpoch(producer, hashRiskIdentity(root), record)
	alias, err := responseIdentityFrom(producer.Request.Context()).issue(producer.Request.Context(), target, "resp_replay_current")
	require.NoError(t, err)
	cacheOwner := responseCacheOwnerForRequest(producer, requestAPIKeyID(producer))
	setResponseCache(cacheOwner, alias, []json.RawMessage{
		json.RawMessage(`{"role":"user","content":"original context"}`),
		json.RawMessage(`{"type":"function_call","call_id":"call_replay","name":"lookup","arguments":"{}"}`),
	})
	request, body, _ := responsePrivacyRequest(t, h, 101, "consumer", alias)
	body, err = sjson.SetRawBytes(body, "input", []byte(`[{"type":"function_call_output","call_id":"call_replay","output":{"number":9007199254740993,"session_id":"business"}},{"type":"reasoning","id":"opaque-id","encrypted_content":"opaque-cipher"}]`))
	require.NoError(t, err)
	request.Set(preservedInputSnapshotKey, body)
	h.attachSessionOutboundEpoch(request, hashRiskIdentity(root), record)
	require.NoError(t, validateResponseIdentityIngress(request, body))
	prepared := prepareResponsesBodyForOwnerDetailed(body, cacheOwner)
	require.Equal(t, responseCacheLookupHit, prepared.CacheLookup.Kind)
	capturePreservedInputReplay(request, prepared)
	ctx, outgoing, err := PreparePreservedInputTransport(request.Request.Context(), prepared.Body)
	require.NoError(t, err)
	require.Equal(t, "original context", gjson.GetBytes(outgoing, "input.0.content").String())
	require.Equal(t, "function_call", gjson.GetBytes(outgoing, "input.1.type").String())
	require.JSONEq(t, gjson.GetBytes(body, "input.0").Raw, gjson.GetBytes(outgoing, "input.2").Raw)
	require.JSONEq(t, gjson.GetBytes(body, "input.1").Raw, gjson.GetBytes(outgoing, "input.3").Raw)
	require.Equal(t, "9007199254740993", gjson.GetBytes(outgoing, "input.2.output.number").Raw)
	_, _, _, err = cleanSessionRestartContext(nil, outgoing, nil, true)
	require.NoError(t, err, "expanded history must retain the matching call")
	require.NoError(t, ValidatePreservedSessionInput(ctx, outgoing))
	_, repeated, err := PreparePreservedInputTransport(ctx, outgoing)
	require.NoError(t, err)
	require.Equal(t, outgoing, repeated, "HTTP to WS executor handoff must not prepend twice")
	mutated, err := sjson.DeleteBytes(outgoing, "input.0")
	require.NoError(t, err)
	require.Error(t, ValidatePreservedSessionInput(ctx, mutated))
	// A later generation cannot authorize this request's cached history.
	staleEpoch := *outboundEpochFromContext(request.Request.Context())
	staleEpoch.record.FailoverCount++
	stale := context.WithValue(request.Request.Context(), sessionOutboundEpochContextKey{}, &staleEpoch)
	_, _, err = PreparePreservedInputTransport(stale, prepared.Body)
	require.Error(t, err)
	changedEpoch := *outboundEpochFromContext(request.Request.Context())
	changedEpoch.preservedInput = []byte(`[{"role":"user","content":"different snapshot"}]`)
	changed := context.WithValue(request.Request.Context(), sessionOutboundEpochContextKey{}, &changedEpoch)
	_, _, err = PreparePreservedInputTransport(changed, prepared.Body)
	require.Error(t, err, "snapshot mismatch must not silently discard cached ancestry")
}

func TestMigratedConversationHandlesUseScopedProtocolMapping(t *testing.T) {
	h, owner, target, root := responsePrivacySetup(t)
	record, _, err := h.db.SwitchSessionContinuityAccount(t.Context(), database.SessionAccountFailover{RootKey: hashRiskIdentity(root), ExpectedAccountID: owner.ID(), AccountID: target.ID(), ResetOutboundWindow: true, WindowThreadID: continuityTestThread, LossyContextRestart: true, PreserveRestartInput: true})
	require.NoError(t, err)
	c, _, _ := responsePrivacyRequest(t, h, 101, "conversation", "")
	h.attachSessionOutboundEpoch(c, hashRiskIdentity(root), record)
	ctx := c.Request.Context()
	db, binding := protocolIdentityBinding(ctx, target)
	require.NotNil(t, db)
	alias := db.CodexConversationAlias(binding, "conv_current_original")
	require.NoError(t, db.PutCodexProtocolPair(ctx, binding, "conversation", database.CodexProtocolPair{Public: alias, Upstream: "conv_current_original"}))
	for _, handle := range []any{alias, map[string]string{"id": alias}, "conv_current_original", map[string]string{"id": "conv_current_original"}} {
		for _, preserve := range []bool{false, true} {
			body, err := json.Marshal(map[string]any{"conversation": handle, "input": []any{map[string]any{"type": "function_call_output", "call_id": "stored_call", "output": "result"}}})
			require.NoError(t, err)
			if !preserve {
				body, err = sjson.SetBytes(body, "input", "continue")
				require.NoError(t, err)
			}
			known, cancel := outboundEpochFromContext(ctx).restartContextVerifier(ctx)
			cleaned, _, report, err := cleanSessionRestartContext(nil, body, known, preserve)
			cancel()
			require.NoError(t, err)
			require.Zero(t, report.Removed["conversation"])
			outgoing, err := prepareConversationOutbound(ctx, target, cleaned)
			require.NoError(t, err)
			value := gjson.GetBytes(outgoing, "conversation")
			if value.IsObject() {
				value = value.Get("id")
			}
			require.Equal(t, "conv_current_original", value.String())
			if preserve {
				require.JSONEq(t, gjson.GetBytes(body, "input").Raw, gjson.GetBytes(outgoing, "input").Raw)
			}
		}
	}
	for _, handle := range []string{
		`{"id":"` + alias + `","id":"conv_unregistered"}`,
		`{"id":"conv_unregistered","id":"` + alias + `"}`,
	} {
		body := []byte(`{"conversation":` + handle + `,"input":"continue"}`)
		_, err := prepareConversationOutbound(ctx, target, body)
		require.Error(t, err, "never verify one duplicate ID and send another")
		known, cancel := outboundEpochFromContext(ctx).restartContextVerifier(ctx)
		_, _, _, err = cleanSessionRestartContext(nil, body, known, true)
		cancel()
		require.Error(t, err)
	}
	for _, mutation := range []string{"user", "root", "account", "generation", "unknown"} {
		t.Run(mutation, func(t *testing.T) {
			badCtx, badRecord, value := ctx, record, alias
			switch mutation {
			case "user":
				other, _, _ := responsePrivacyRequest(t, h, 102, "conversation", "")
				h.attachSessionOutboundEpoch(other, hashRiskIdentity(root), record)
				badCtx = other.Request.Context()
			case "root":
				session := responseIdentityFrom(ctx)
				badCtx = context.WithValue(ctx, protocolIdentityKey{}, &responseIdentitySession{handler: session.handler, scope: session.scope, rootKey: "other-root"})
			case "account":
				badRecord.AccountID = owner.ID()
			case "generation":
				badRecord.FailoverCount++
			case "unknown":
				value = "conv_unregistered"
			}
			require.False(t, trustedConversationIdentity(badCtx, badRecord, value))
			_, _, _, err := cleanSessionRestartContext(http.Header{}, []byte(`{"conversation":{"id":"`+value+`"},"input":"continue"}`), func(kind, token string) bool {
				return kind == "conversation" && trustedConversationIdentity(badCtx, badRecord, token)
			}, true)
			require.Error(t, err)
		})
	}
}

func TestPlainContinuationMissingHistoryHTTPAndCompact(t *testing.T) {
	for _, compact := range []bool{false, true} {
		for _, relay := range []bool{false, true} {
			name := "responses"
			if compact {
				name = "compact"
			}
			if relay {
				name += "/relay"
			} else {
				name += "/native"
			}
			t.Run(name, func(t *testing.T) {
				resetResponseCacheStateForTest(testResponseCacheConfig())
				var upstreamBody []byte
				upstream := newContinuationRelayUpstream(t, compact, &upstreamBody)
				store := newContinuationCodexStore()
				if relay {
					store.Stop()
					store = newContinuationRelayStore(upstream.URL)
				}
				t.Cleanup(store.Stop)
				h := NewHandler(store, nil, nil, nil)
				handle := h.Responses
				if compact {
					handle = h.ResponsesCompact
				}
				raw := []byte(`{"model":"gpt-5.5","stream":true,"previous_response_id":"resp_plain_missing","input":[{"role":"user","content":"continue"}]}`)
				response := invokeResponsesHandler(t, handle, raw)
				if relay {
					require.Equal(t, http.StatusOK, response.Code, response.Body.String())
					require.Equal(t, "resp_plain_missing", gjson.GetBytes(upstreamBody, "previous_response_id").String())
				} else {
					require.Equal(t, http.StatusConflict, response.Code, response.Body.String())
					require.Equal(t, string(api.ErrCodeResponseContextUnavailable), gjson.Get(response.Body.String(), "error.code").String())
					require.Empty(t, upstreamBody)
					require.NotContains(t, response.Body.String(), "resp_plain_missing")
				}
			})
		}
	}
}
