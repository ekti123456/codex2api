package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func responsePrivacyRequest(t *testing.T, h *Handler, user int64, turn, previous string) (*gin.Context, []byte, requestSessionIdentity) {
	c, body, identity := aliasRequest(t, h, user, turn, "", false)
	h.bindResponseIdentity(c, identity)
	if previous != "" {
		var err error
		body, err = sjson.SetBytes(body, "previous_response_id", previous)
		require.NoError(t, err)
	}
	return c, body, identity
}

func responsePrivacySetup(t *testing.T) (*Handler, *auth.Account, *auth.Account, string) {
	h, owner, target, _ := failoverTestSetup(t, true)
	c, _, identity := responsePrivacyRequest(t, h, 101, "turn-1", "")
	root := sessionAffinityKey(identity.affinityID, requestAPIKeyID(c))
	h.store.BindSessionAffinity(root, owner, "")
	_, err := h.db.CommitSessionContinuity(t.Context(), hashRiskIdentity(root), database.SessionContinuityRecord{AccountID: owner.ID(), LastSeen: time.Now()})
	require.NoError(t, err)
	return h, owner, target, root
}

func TestResponsePrivacyAliasLifecycleAndValidation(t *testing.T) {
	h, owner, target, root := responsePrivacySetup(t)
	c, _, _ := responsePrivacyRequest(t, h, 101, "turn-1", "")
	alias, err := responseIdentityFrom(c.Request.Context()).issue(c.Request.Context(), owner, "resp_original")
	require.NoError(t, err)
	// Response handles outlive the issuing turn, unlike turn-state tokens.
	next, body, _ := responsePrivacyRequest(t, h, 101, "turn-2", alias)
	require.NoError(t, validateResponseIdentityIngress(next, body))
	for i := 0; i < 2; i++ {
		body, err = prepareResponseIdentityOutbound(next.Request.Context(), owner, body)
		require.NoError(t, err)
		require.Equal(t, "resp_original", gjson.GetBytes(body, "previous_response_id").String())
	}
	_, err = prepareResponseIdentityOutbound(next.Request.Context(), target, body)
	require.Error(t, err)
	for _, id := range []string{"resp_unknown", "resp_" + strings.Repeat("f", 64), "123iqa", " " + alias, alias + " "} {
		rejected, payload, _ := responsePrivacyRequest(t, h, 101, "turn-2", id)
		require.Error(t, validateResponseIdentityIngress(rejected, payload))
	}
	other, payload, _ := responsePrivacyRequest(t, h, 102, "turn-2", alias)
	require.Error(t, validateResponseIdentityIngress(other, payload))
	other, payload, identity := responsePrivacyRequest(t, h, 101, "turn-2", alias)
	identity.affinityID = "another-root"
	h.bindResponseIdentity(other, identity)
	require.Error(t, validateResponseIdentityIngress(other, payload))
	// A -> B -> A never revives an earlier-generation handle.
	_, _, err = h.db.SwitchSessionContinuityAccount(t.Context(), database.SessionAccountFailover{RootKey: hashRiskIdentity(root), ExpectedAccountID: owner.ID(), AccountID: target.ID()})
	require.NoError(t, err)
	_, _, err = h.db.SwitchSessionContinuityAccount(t.Context(), database.SessionAccountFailover{RootKey: hashRiskIdentity(root), ExpectedAccountID: target.ID(), ExpectedGeneration: 1, AccountID: owner.ID()})
	require.NoError(t, err)
	stale, payload, _ := responsePrivacyRequest(t, h, 101, "turn-3", alias)
	require.Error(t, validateResponseIdentityIngress(stale, payload))
}

func TestResponsePrivacyLegacyCacheAndPostFailover(t *testing.T) {
	h, owner, target, root := responsePrivacySetup(t)
	c, body, _ := responsePrivacyRequest(t, h, 101, "turn", "resp_legacy")
	s := responseIdentityFrom(c.Request.Context())
	h.recordResponseAccountAffinity(s.owner, "resp_legacy", owner.ID(), root, "gpt-5.5", "")
	require.NoError(t, validateResponseIdentityIngress(c, body))
	actual, err := prepareResponseIdentityOutbound(c.Request.Context(), owner, body)
	require.NoError(t, err)
	require.JSONEq(t, string(body), string(actual))
	h.recordResponseAccountAffinity(s.owner, "resp_wrong_account", target.ID(), root, "gpt-5.5", "")
	next, payload, _ := responsePrivacyRequest(t, h, 101, "turn", "resp_wrong_account")
	require.Error(t, validateResponseIdentityIngress(next, payload))
	record, _, err := h.db.SwitchSessionContinuityAccount(t.Context(), database.SessionAccountFailover{RootKey: hashRiskIdentity(root), ExpectedAccountID: owner.ID(), AccountID: target.ID(), ResetOutboundWindow: true, WindowThreadID: continuityTestThread, LossyContextRestart: true, PreserveRestartInput: true})
	require.NoError(t, err)
	h.store.UnbindSessionAffinity(root, owner.ID())
	h.store.BindSessionAffinity(root, target, "")
	producer, _, _ := responsePrivacyRequest(t, h, 101, "turn-new", "")
	h.attachSessionOutboundEpoch(producer, hashRiskIdentity(root), record)
	alias, err := responseIdentityFrom(producer.Request.Context()).issue(producer.Request.Context(), target, "resp_after_switch")
	require.NoError(t, err)
	next, payload, _ = responsePrivacyRequest(t, h, 101, "turn-later", alias)
	require.NoError(t, validateResponseIdentityIngress(next, payload))
	h.attachSessionOutboundEpoch(next, hashRiskIdentity(root), record)
	for i := 0; i < 2; i++ {
		payload, _, err = PrepareSessionRestartOutbound(next.Request.Context(), target, payload, nil)
		require.NoError(t, err)
		require.Equal(t, "resp_after_switch", gjson.GetBytes(payload, "previous_response_id").String())
	}
	// A cache outage must not be interpreted as authorization for a raw old ID.
	unknown, unknownBody, _ := responsePrivacyRequest(t, h, 101, "turn", "resp_untracked_legacy")
	require.Error(t, validateResponseIdentityIngress(unknown, unknownBody))
}

func TestResponsePrivacyAllEnvelopesAndHeaders(t *testing.T) {
	h, owner, target, _ := responsePrivacySetup(t)
	c, _, _ := responsePrivacyRequest(t, h, 101, "turn", "")
	ctx := c.Request.Context()
	for _, path := range []string{"headers", "metadata.headers", "response.headers", "response.metadata.headers"} {
		data := []byte(`{"type":"response.created","response":{"id":"resp_real","output":[],"model":"actual-model","usage":{"input_tokens":12}},"metadata":{"openai_chatgpt_moderation_metadata":{"keep":true}}}`)
		headers := map[string]any{"X-Codex-Turn-State": "upstream-state", "OpenAI-Model": "actual-model", "x-request-id": "trace-secret", "OpenAI-Organization": "org-secret", "chatgpt-account-id": "account-secret", "Set-Cookie": "session-secret", "X-Models-Etag": "catalog-secret", "unknown": "secret"}
		data, err := sjson.SetBytes(data, path, headers)
		require.NoError(t, err)
		out, err := maskResponsePayload(ctx, owner, data, false)
		require.NoError(t, err)
		for _, secret := range []string{"resp_real", "upstream-state", "trace-secret", "org-secret", "account-secret", "session-secret", "catalog-secret"} {
			require.NotContains(t, string(out), secret)
		}
		require.Equal(t, "actual-model", gjson.GetBytes(out, "response.model").String())
		require.Equal(t, int64(12), gjson.GetBytes(out, "response.usage.input_tokens").Int())
		require.True(t, gjson.GetBytes(out, "metadata.openai_chatgpt_moderation_metadata.keep").Bool())
		require.True(t, h.db.IsManagedCodexResponseID(gjson.GetBytes(out, "response.id").String()))
	}
	s := responseIdentityFrom(ctx)
	alias, err := s.issue(ctx, owner, "resp_real")
	require.NoError(t, err)
	for _, kind := range []string{"response.created", "response.in_progress", "response.completed", "response.done", "response.failed"} {
		data, _ := json.Marshal(map[string]any{"type": kind, "response_id": "resp_real", "response": map[string]any{"id": "resp_real", "previous_response_id": "resp_previous", "output": []any{map[string]any{"id": "msg_keep", "call_id": "call_keep", "type": "function_call", "arguments": "{\"id\":\"resp_real\"}"}}}})
		out, err := maskResponsePayload(ctx, owner, data, false)
		require.NoError(t, err)
		require.Equal(t, alias, gjson.GetBytes(out, "response.id").String())
		require.Equal(t, alias, gjson.GetBytes(out, "response_id").String())
		require.Equal(t, "msg_keep", gjson.GetBytes(out, "response.output.0.id").String())
		require.Equal(t, "call_keep", gjson.GetBytes(out, "response.output.0.call_id").String())
		require.Contains(t, gjson.GetBytes(out, "response.output.0.arguments").String(), "resp_real")
	}
	response := &http.Response{Header: http.Header{"Content-Type": []string{"application/json"}}, Body: io.NopCloser(strings.NewReader(`{"id":"resp_real","object":"response.compaction","output":[{"type":"compaction","encrypted_content":"keep-opaque"}],"headers":{"x-codex-turn-state":"real-json-state","account-id":"secret"}}`))}
	require.NoError(t, maskTurnStateResponse(ctx, owner, response))
	out, err := io.ReadAll(response.Body)
	require.NoError(t, err)
	require.Equal(t, alias, gjson.GetBytes(out, "id").String())
	require.Contains(t, string(out), "keep-opaque")
	require.NotContains(t, string(out), "secret")
	require.NotContains(t, string(out), "real-json-state")
	frame := []byte("event: response.completed\r\ndata: {\"type\":\"response.completed\",\r\ndata: \"response\":{\"id\":\"resp_real\"}}\r\n\r\n")
	stream := &turnStateStream{ctx: ctx, account: owner, state: turnStateSessionFrom(ctx)}
	masked, err := stream.maskFrame(frame)
	require.NoError(t, err)
	require.Contains(t, string(masked), alias)
	require.NotContains(t, string(masked), "resp_real")
	plain := []byte("data: {\"type\":\"response.output_text.delta\",\"delta\":\"resp_real\"}\n\n")
	masked, err = stream.maskFrame(plain)
	require.NoError(t, err)
	require.True(t, bytes.Equal(plain, masked))
	errFrame := []byte(`{"type":"error","error":{"code":"previous_response_not_found","message":"Previous response resp_real was not found"}}`)
	masked, err = maskResponsePayload(ctx, owner, errFrame, false)
	require.NoError(t, err)
	require.Contains(t, string(masked), alias)
	require.NotContains(t, string(masked), "resp_real")
	issuedBefore := len(s.issued)
	unknownError := []byte(`{"type":"error","error":{"message":"Unknown response resp_unverified_reference"}}`)
	masked, err = maskResponsePayload(ctx, owner, unknownError, false)
	require.NoError(t, err)
	require.NotContains(t, string(masked), "resp_unverified_reference")
	require.Contains(t, string(masked), "[response]")
	require.Len(t, s.issued, issuedBefore, "an echoed error ID must not become an authorized continuation")
	foreignReference, err := s.publicErrorReference(ctx, target, "resp_real")
	require.NoError(t, err)
	require.Equal(t, "[response]", foreignReference, "an earlier attempt's ID must not be authorized on another account")
	diagnostics, _ := json.Marshal(responseIdentityDiagnostic(ctx))
	require.Contains(t, string(diagnostics), "resp_real")
	require.Contains(t, string(diagnostics), alias)
}

func TestResponsePrivacyStoreFailureAndRelayFrameReset(t *testing.T) {
	h, owner, _, _ := responsePrivacySetup(t)
	c, _, identity := responsePrivacyRequest(t, h, 101, "turn", "")
	ctx := c.Request.Context()
	c.Set(apiRelaySessionExemptContextKey, true)
	h.bindResponseIdentity(c, identity)
	require.Nil(t, responseIdentityFrom(c.Request.Context()))
	originalDB := h.db
	h.db = nil
	defer func() { h.db = originalDB }()
	_, err := maskResponsePayload(ctx, owner, []byte(`{"type":"response.created","response":{"id":"resp_unrecordable"}}`), false)
	require.Error(t, err)
	_, err = prepareResponseIdentityOutbound(context.WithValue(ctx, responseIdentityKey{}, responseIdentityFrom(ctx)), owner, []byte(`{"previous_response_id":"resp_injected_by_rule"}`))
	require.Error(t, err)
}

func TestResponsePrivacyLazyReadsAndMissingContentType(t *testing.T) {
	h, owner, _, _ := responsePrivacySetup(t)
	c, _, _ := responsePrivacyRequest(t, h, 101, "turn", "")
	for _, contentType := range []string{"application/json", "", "text/plain"} {
		t.Run(contentType, func(t *testing.T) {
			reader, writer := io.Pipe()
			defer reader.Close()
			defer writer.Close()
			response := &http.Response{Header: http.Header{"Content-Type": []string{contentType}}, Body: reader}
			timer := time.AfterFunc(time.Second, func() { _ = writer.CloseWithError(context.DeadlineExceeded) })
			defer timer.Stop()
			// The executor must return headers before reading any response data.
			require.NoError(t, maskTurnStateResponse(c.Request.Context(), owner, response))
			if contentType == "application/json" {
				go func() {
					_, _ = writer.Write([]byte(`{"id":"resp_lazy","headers":{"account-id":"secret"}}`))
					_ = writer.Close()
				}()
				out, err := io.ReadAll(response.Body)
				require.NoError(t, err)
				require.True(t, h.db.IsManagedCodexResponseID(gjson.GetBytes(out, "id").String()))
				require.NotContains(t, string(out), "secret")
			} else {
				// First event must reach the caller while the upstream stream is
				// still open, even if an intermediary dropped the SSE content type.
				go func() {
					_, _ = writer.Write([]byte("\n\ndata: {\"type\":\"response.created\",\"response\":{\"id\":\"resp_lazy\",\"headers\":{\"account-id\":\"secret\"}}}\n\n"))
				}()
				buffer := make([]byte, 2048)
				n, err := response.Body.Read(buffer)
				require.NoError(t, err)
				require.Contains(t, string(buffer[:n]), "resp_")
				require.NotContains(t, string(buffer[:n]), "resp_lazy")
				require.NotContains(t, string(buffer[:n]), "secret")
			}
		})
	}
}

func TestResponsePrivacyForeignKeyCannotDowngradeAliasToLegacy(t *testing.T) {
	h, owner, _, root := responsePrivacySetup(t)
	c, _, _ := responsePrivacyRequest(t, h, 101, "turn", "")
	s := responseIdentityFrom(c.Request.Context())
	alias, err := s.issue(c.Request.Context(), owner, "resp_shared_cache_original")
	require.NoError(t, err)
	h.recordResponseAccountAffinity(s.owner, alias, owner.ID(), root, "gpt-5.5", "")
	// File-backed test DBs clone a schema template, including its secret.
	// An in-memory DB runs fresh migrations and really has a different key.
	otherDB, err := database.New("sqlite", ":memory:")
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, otherDB.Close()) })
	require.False(t, otherDB.IsManagedCodexResponseID(alias), "separate keys must not authenticate the same alias")
	originalDB := h.db
	h.db = otherDB
	defer func() { h.db = originalDB }()
	consumer, body, _ := responsePrivacyRequest(t, h, 101, "turn-2", alias)
	require.Error(t, validateResponseIdentityIngress(consumer, body))
	events := responseIdentityDiagnostic(consumer.Request.Context())
	require.Equal(t, "rejected_unverifiable_alias", events[0].Action)
}
