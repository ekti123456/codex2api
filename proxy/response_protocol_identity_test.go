package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestHistoryTurnIdentityRoundTripAndIsolation(t *testing.T) {
	t.Setenv("CODEX_OUTBOUND_SESSION_MODE", "account")
	h, account, otherAccount, _ := responsePrivacySetup(t)
	c, _, _ := responsePrivacyRequest(t, h, 101, "first", "")
	ctx := WithCodexIdentityStore(c.Request.Context(), h.db)
	headers, body := accountIdentityFixture(t, false, true)
	turn1 := gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata.turn_id").String()
	first := NewCodexTransportFingerprint(account, headers, body, "", ctx)
	require.NoError(t, first.ClaimSessionIdentity(ctx, account, "history-test"))
	firstOut := first.ApplyBody(body)
	upstream1 := gjson.GetBytes(firstOut, "client_metadata.x-codex-turn-metadata.turn_id").String()
	require.NotEqual(t, turn1, upstream1)
	turn2 := uuid.Must(uuid.NewV7()).String()
	body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.turn_id", turn2)
	body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.root_turn_id", turn2)
	var history []any
	for i := 0; i < 40; i++ {
		id := fmt.Sprint(i)
		if i == 0 {
			id = turn1
		}
		if i == 1 {
			id = turn2
		}
		history = append(history, map[string]any{"type": "message", "role": "assistant", "content": []any{}, "internal_chat_message_metadata_passthrough": map[string]any{"turn_id": id}})
	}
	body, _ = sjson.SetBytes(body, "input", history)
	body, _ = sjson.SetBytes(body, "input.-1", map[string]any{"type": "function_call_output", "call_id": "call_x", "output": "{\"turn_id\":\"business\"}"})
	// A fresh request context reconstructs the same mapping from persistence.
	next, _, _ := responsePrivacyRequest(t, h, 101, "second", "")
	nextCtx := WithCodexIdentityStore(next.Request.Context(), h.db)
	second := NewCodexTransportFingerprint(account, headers, body, "", nextCtx)
	require.NoError(t, second.ClaimSessionIdentity(nextCtx, account, "history-test"))
	out := second.ApplyBody(body)
	upstream2 := gjson.GetBytes(out, "client_metadata.x-codex-turn-metadata.turn_id").String()
	require.NotEqual(t, upstream1, upstream2)
	require.Equal(t, upstream1, gjson.GetBytes(out, "input.0.internal_chat_message_metadata_passthrough.turn_id").String())
	require.Equal(t, upstream2, gjson.GetBytes(out, "input.1.internal_chat_message_metadata_passthrough.turn_id").String())
	require.Equal(t, upstream2, gjson.Get(second.DownstreamHeaders().Get(codexTurnMetadataHeader), "turn_id").String())
	require.Equal(t, gjson.GetBytes(body, "input.40.output").String(), gjson.GetBytes(out, "input.40.output").String())
	require.JSONEq(t, string(out), string(second.ApplyBody(out)))
	response := []byte(`{"id":"resp_history","output":` + gjson.GetBytes(out, "input").Raw + `}`)
	masked, err := maskResponsePayload(nextCtx, account, response, true)
	require.NoError(t, err)
	for i := 0; i < 40; i++ {
		require.Equal(t, gjson.GetBytes(body, fmt.Sprintf("input.%d.internal_chat_message_metadata_passthrough.turn_id", i)).String(), gjson.GetBytes(masked, fmt.Sprintf("output.%d.internal_chat_message_metadata_passthrough.turn_id", i)).String())
	}
	// A later response can refer to old history even if this request did not
	// upload that item. Reverse lookup never guesses the current turn.
	walker := responsePrivacyWalker{ctx: nextCtx, account: account}
	metadata := json.RawMessage(gjson.GetBytes(masked, "output.0.internal_chat_message_metadata_passthrough").Raw)
	again, err := walker.itemMetadata(metadata)
	require.NoError(t, err)
	require.JSONEq(t, string(metadata), string(again))
	later, _, _ := responsePrivacyRequest(t, h, 101, "later", "")
	masked, err = maskResponsePayload(later.Request.Context(), account, response, true)
	require.NoError(t, err)
	require.Equal(t, turn1, gjson.GetBytes(masked, "output.0.internal_chat_message_metadata_passthrough.turn_id").String())
	foreign, _, _ := responsePrivacyRequest(t, h, 102, "foreign", "")
	for _, check := range []struct {
		ctx   context.Context
		other bool
	}{{foreign.Request.Context(), false}, {later.Request.Context(), true}, {context.WithValue(later.Request.Context(), sessionOutboundEpochContextKey{}, &sessionOutboundEpoch{record: database.SessionContinuityRecord{FailoverCount: 2}}), false}} {
		selected := account
		if check.other {
			selected = otherAccount
		}
		masked, err = maskResponsePayload(check.ctx, selected, response, true)
		require.NoError(t, err)
		require.False(t, gjson.GetBytes(masked, "output.0.internal_chat_message_metadata_passthrough.turn_id").Exists())
		require.NotContains(t, string(masked), upstream1)
	}
}

func TestConversationAliasRoundTripAndGeneration(t *testing.T) {
	h, account, other, _ := responsePrivacySetup(t)
	c, _, _ := responsePrivacyRequest(t, h, 101, "first", "")
	response := []byte(`{"id":"resp_conversation","conversation":{"id":"conv_official_resource","account_id":"secret"},"output":[]}`)
	masked, err := maskResponsePayload(c.Request.Context(), account, response, true)
	require.NoError(t, err)
	alias := gjson.GetBytes(masked, "conversation.id").String()
	require.True(t, h.db.IsManagedCodexConversationAlias(alias))
	again, err := (responsePrivacyWalker{ctx: c.Request.Context(), account: account}).conversation(json.RawMessage(gjson.GetBytes(masked, "conversation").Raw))
	require.NoError(t, err)
	require.Equal(t, alias, gjson.GetBytes(again, "id").String())
	require.NotContains(t, string(masked), "conv_official_resource")
	require.NotContains(t, string(masked), "secret")
	for _, object := range []bool{false, true} {
		var handle any = alias
		if object {
			handle = map[string]string{"id": alias}
		}
		body, _ := json.Marshal(map[string]any{"conversation": handle, "input": []any{}})
		next, _, _ := responsePrivacyRequest(t, h, 101, "next", "")
		out, err := prepareConversationOutbound(next.Request.Context(), account, body)
		require.NoError(t, err)
		require.Contains(t, string(out), "conv_official_resource")
		require.NotContains(t, string(out), alias)
		_, err = prepareConversationOutbound(next.Request.Context(), other, body)
		require.Error(t, err)
		wrong, _, _ := responsePrivacyRequest(t, h, 102, "wrong", "")
		_, err = prepareConversationOutbound(wrong.Request.Context(), account, body)
		require.Error(t, err)
		changed := context.WithValue(next.Request.Context(), sessionOutboundEpochContextKey{}, &sessionOutboundEpoch{record: database.SessionContinuityRecord{FailoverCount: 2}})
		_, err = prepareConversationOutbound(changed, account, body)
		require.Error(t, err)
	}
}

func TestToolErrorPrivacyAndProtocolErrorShape(t *testing.T) {
	h, account, _, _ := responsePrivacySetup(t)
	c, _, _ := responsePrivacyRequest(t, h, 101, "errors", "")
	payload := []byte(`{"id":"resp_tools","output":[{"type":"mcp_call","error":{"type":"mcp_tool_execution_error","account_id":"hidden","content":[{"type":"text","text":"Unknown column customer_id; use id. Bearer owner-token"}]}}],"moderation":{"input":{"type":"error","code":"moderation_unavailable","message":"Temporarily unavailable"}}}`)
	masked, err := maskResponsePayload(c.Request.Context(), account, payload, true)
	require.NoError(t, err)
	final := publicResponseErrorPayload(c, masked)
	require.Equal(t, "mcp_tool_execution_error", gjson.GetBytes(final, "output.0.error.type").String())
	require.Contains(t, gjson.GetBytes(final, "output.0.error.content.0.text").String(), "Unknown column customer_id; use id")
	require.NotContains(t, string(final), "owner-token")
	require.NotContains(t, string(final), "hidden")
	require.JSONEq(t, gjson.GetBytes(payload, "moderation").Raw, gjson.GetBytes(final, "moderation").Raw)
	final = publicResponseErrorPayload(c, []byte(`{"type":"error","stream_id":"client","status":400,"sequence_number":3,"error":{"code":"previous_response_not_found","type":"invalid_request_error","message":"secret","param":"previous_response_id"},"account_id":"hidden"}`))
	for path, want := range map[string]string{"stream_id": "client", "status": "400", "sequence_number": "3", "error.type": "invalid_request_error", "error.param": "previous_response_id"} {
		require.Equal(t, want, gjson.GetBytes(final, path).String())
	}
	require.NotContains(t, string(final), "secret")
	require.NotContains(t, string(final), "hidden")
	trace := transportTestContext()
	beginUpstreamTrace(trace.Request.Context(), account, "", true)
	UpstreamTransportObserver(trace.Request.Context()).updateOutboundIdentity(func(identity *outboundIdentityDiagnostic) {
		identity.AccountMapping = &codexAccountIdentityDiagnostic{Changes: []codexAccountIdentityChange{{Outbound: "upstream-ws-session-private"}}}
	})
	toolError, err := (responsePrivacyWalker{ctx: trace.Request.Context(), account: account}).toolError([]byte(`{"type":"mcp_tool_execution_error","content":{"text":"Unknown column; session upstream-ws-session-private","number":9007199254740993}}`))
	require.NoError(t, err)
	require.NotContains(t, string(toolError), "upstream-ws-session-private")
	require.Contains(t, string(toolError), "Unknown column")
	require.Equal(t, "9007199254740993", gjson.GetBytes(toolError, "content.number").Raw)
}

func TestDownstreamFlatErrorAndShellCommandEvents(t *testing.T) {
	h, account, _, _ := responsePrivacySetup(t)
	c, _, _ := responsePrivacyRequest(t, h, 101, "events", "")
	for _, kind := range []string{"response.shell_call_command.added", "response.shell_call_command.done"} {
		payload, _ := json.Marshal(map[string]any{"type": kind, "command": "{ echo business; }", "command_index": 0, "output_index": 1, "sequence_number": 2})
		masked, err := maskResponsePayload(c.Request.Context(), account, payload, false)
		require.NoError(t, err)
		require.JSONEq(t, string(payload), string(publicResponseErrorPayload(c, masked)))
	}
	flat := []byte(`{"type":"error","sequence_number":7,"code":"invalid_parameter","message":"secret trace","param":"input[0].type"}`)
	masked, err := maskResponsePayload(c.Request.Context(), account, flat, false)
	require.NoError(t, err)
	final := publicResponseErrorPayload(c, masked)
	require.False(t, gjson.GetBytes(final, "error").Exists())
	require.Equal(t, "error", gjson.GetBytes(final, "type").String())
	require.Equal(t, "invalid_parameter", gjson.GetBytes(final, "code").String())
	require.Equal(t, "input[0].type", gjson.GetBytes(final, "param").String())
	require.EqualValues(t, 7, gjson.GetBytes(final, "sequence_number").Int())
	require.NotContains(t, string(final), "secret trace")
}

func TestHistoryTurnDuplicateMetadataUsesOneValue(t *testing.T) {
	t.Setenv("CODEX_OUTBOUND_SESSION_MODE", "account")
	h, account, _, _ := responsePrivacySetup(t)
	c, _, _ := responsePrivacyRequest(t, h, 101, "duplicates", "")
	ctx := WithCodexIdentityStore(c.Request.Context(), h.db)
	headers, body := accountIdentityFixture(t, false, true)
	body, _ = sjson.SetRawBytes(body, "input", []byte(`[{"type":"message","content":[],"internal_chat_message_metadata_passthrough":{"turn_id":"discarded"},"internal_chat_message_metadata_passthrough":{"turn_id":"discarded-too","turn_id":"chosen"}}]`))
	fingerprint := NewCodexTransportFingerprint(account, headers, body, "", ctx)
	require.NoError(t, fingerprint.ClaimSessionIdentity(ctx, account, "duplicates"))
	out := fingerprint.ApplyBody(body)
	require.NotContains(t, string(out), "discarded")
	require.NotContains(t, string(out), "chosen")
	require.Equal(t, 1, strings.Count(gjson.GetBytes(out, "input.0").Raw, `"turn_id"`))
	metadata, err := (responsePrivacyWalker{ctx: ctx, account: account}).itemMetadata(json.RawMessage(gjson.GetBytes(out, "input.0.internal_chat_message_metadata_passthrough").Raw))
	require.NoError(t, err)
	require.Equal(t, "chosen", gjson.GetBytes(metadata, "turn_id").String())
}

func TestDownstreamWebSocketValidationErrorUsesCurrentLane(t *testing.T) {
	h, _, _, _ := responsePrivacySetup(t)
	engine := gin.New()
	engine.GET("/v1/responses", func(c *gin.Context) {
		c.Set(contextAPIKeyID, int64(101))
		conn, err := (&websocket.Upgrader{}).Upgrade(c.Writer, c.Request, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for i := 0; i < 3; i++ {
			_, body, err := conn.ReadMessage()
			if err != nil {
				return
			}
			_ = h.forwardResponsesWebSocketTurn(c, conn, body, "", nil)
		}
	})
	server := httptest.NewServer(engine)
	defer server.Close()
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", nil)
	require.NoError(t, err)
	defer conn.Close()
	require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
	for _, lane := range []string{"lane_one", "lane_two", ""} {
		payload := map[string]any{"type": "response.create"}
		if lane != "" {
			payload["stream_id"] = lane
		}
		require.NoError(t, conn.WriteJSON(payload))
		_, response, err := conn.ReadMessage()
		require.NoError(t, err)
		require.Equal(t, "error", gjson.GetBytes(response, "type").String())
		require.Equal(t, lane, gjson.GetBytes(response, "stream_id").String())
		if lane == "" {
			require.False(t, gjson.GetBytes(response, "stream_id").Exists())
		}
	}
}

func TestFunctionalHeadersWinningAttemptAndManifest(t *testing.T) {
	h, account, _, _ := responsePrivacySetup(t)
	c, _, _ := responsePrivacyRequest(t, h, 101, "headers", "")
	headers := http.Header{"X-Reasoning-Included": {""}, "Openai-Model": {"gpt-6-astra"}, "X-Models-Etag": {"official-v1"}, "X-Request-Id": {"private-trace"}}
	relayCodexTurnStateResponseHeader(c, "", account, headers)
	_, present := c.Writer.Header()["X-Reasoning-Included"]
	require.True(t, present)
	require.Equal(t, "gpt-6-astra", c.Writer.Header().Get("OpenAI-Model"))
	require.Empty(t, c.Writer.Header().Get("X-Request-Id"))
	initial := c.Writer.Header().Get("X-Models-Etag")
	require.NotEmpty(t, initial)
	require.NotContains(t, initial, "official")
	w := httptest.NewRecorder()
	catalog, _ := gin.CreateTestContext(w)
	catalog.Set(contextAPIKeyID, int64(101))
	catalog.Request = httptest.NewRequest("GET", "/models", nil)
	h.writeCodexManifest(catalog, []byte(`{"models":[{"slug":"visible"}]}`), "")
	relayCodexTurnStateResponseHeader(c, "", account, headers)
	require.Equal(t, w.Header().Get("ETag"), c.Writer.Header().Get("X-Models-Etag"))
	headers.Set("X-Models-Etag", "official-v2")
	relayCodexTurnStateResponseHeader(c, "", account, headers)
	require.NotEqual(t, w.Header().Get("ETag"), c.Writer.Header().Get("X-Models-Etag"))
	relayCodexTurnStateResponseHeader(c, "", account, nil)
	for _, field := range []string{"X-Reasoning-Included", "OpenAI-Model", "X-Models-Etag"} {
		_, ok := c.Writer.Header()[http.CanonicalHeaderKey(field)]
		require.False(t, ok, strings.Join(c.Writer.Header().Values(field), ","))
	}
	row := &database.APIKeyRow{ID: 101, AllowedGroupIDs: []int64{3}}
	c.Set(contextAPIKeyRow, row)
	scope := codexManifestSignalScope(c)
	row.QuotaUsed = 2
	row.TotalUsed = 4
	require.Equal(t, scope, codexManifestSignalScope(c))
	row.Limits.ModelAllow = []string{"different"}
	require.NotEqual(t, scope, codexManifestSignalScope(c))
}
