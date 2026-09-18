package proxy

import (
	"context"
	"encoding/json"
	"errors"
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestResponsePrivacyRecursiveFields(t *testing.T) {
	h, account, _, _ := responsePrivacySetup(t)
	c, _, _ := responsePrivacyRequest(t, h, 101, "turn", "")
	for _, field := range []string{"session_id", "thread_id", "conversation_id", "parent_thread_id", "turn_id", "root_turn_id", "account_id", "chatgpt_account_id", "organization_id", "project_id", "installation_id", "device_id", "window_id", "client_request_id", "trace_id", "user_id", "email", "sessionId", "CHATGPT-ACCOUNT-ID", "X-API-Key"} {
		for _, prefix := range []string{"", "response.", "metadata.nested.", "client_metadata.nested.", "debug.nested.", "metadata.content.", "debug.content.", "response.output.0.metadata."} {
			raw, err := sjson.SetBytes([]byte(`{"type":"response.metadata","response":{"id":"resp_private"}}`), prefix+field, "audit-private-value")
			require.NoError(t, err)
			out, err := maskResponsePayload(c.Request.Context(), account, raw, false)
			require.NoError(t, err)
			require.NotContains(t, string(out), "audit-private-value", prefix+field)
		}
	}
	for _, path := range []string{"headers", "metadata.headers", "response.headers", "response.metadata.headers", "metadata.nested.headers", "client_metadata.headers", "client_metadata.nested.headers", "response.client_metadata.headers", "Headers", "metadata.Headers"} {
		raw, _ := sjson.SetBytes([]byte(`{"type":"response.metadata"}`), path, map[string]any{"Authorization": "audit-credential", "Set-Cookie": "audit-cookie", "X-Codex-Turn-State": "audit-turn", "OpenAI-Model": map[string]string{"account_id": "audit-model-nested"}})
		out, err := maskResponsePayload(c.Request.Context(), account, raw, false)
		require.NoError(t, err)
		for _, secret := range []string{"audit-credential", "audit-cookie", "audit-turn", "audit-model-nested"} {
			require.NotContains(t, string(out), secret, path)
		}
	}
	for _, raw := range []string{
		`{"metadata":"{\"headers\":{\"Authorization\":\"audit-private\"},\"session_id\":\"audit-private\"}"}`,
		`{"metadata":[{"nested":{"Authorization":"audit-private"}},{"headers":{"Cookie":"audit-private"}}]}`,
		`{"metadata":{"session":{"id":"audit-private"},"account":{"id":"audit-private"}}}`,
		`{"metadata":{"session_id":"first","session_id":"audit-private"}}`,
		`{"metadata":{"headers":{"OpenAI-Model":"keep","OpenAI-Model":{"authorization":"audit-private"}}}}`,
		`{"metadata":"{\"authorization\":\"audit-private\" broken"}`,
	} {
		out, err := maskResponsePayload(c.Request.Context(), account, []byte(raw), true)
		require.NoError(t, err)
		require.NotContains(t, string(out), "audit-private")
	}
	deep := `{"account_id":"audit-private"}`
	for range 70 {
		deep = `{"nested":` + deep + `}`
	}
	out, err := maskResponsePayload(c.Request.Context(), account, []byte(`{"metadata":`+deep+`}`), true)
	require.NoError(t, err)
	require.NotContains(t, string(out), "audit-private")
	// Diagnostic references must not mint new authorized continuations.
	before := len(responseIdentityFrom(c.Request.Context()).issued)
	for _, path := range []string{"metadata.response_id", "response.metadata.previous_response_id", "client_metadata.response_id", "error.details.response_id", "debug.response_id"} {
		raw, _ := sjson.SetBytes([]byte(`{}`), path, "resp_unverified_private")
		out, err := maskResponsePayload(c.Request.Context(), account, raw, true)
		require.NoError(t, err)
		require.NotContains(t, string(out), "resp_unverified_private")
	}
	require.Len(t, responseIdentityFrom(c.Request.Context()).issued, before)
	out, err = maskResponsePayload(c.Request.Context(), account, []byte(`{"metadata":{"response_id":"resp_same_event"},"response":{"id":"resp_same_event"}}`), false)
	require.NoError(t, err)
	require.NotEmpty(t, gjson.GetBytes(out, "response.id").String())
	require.Equal(t, gjson.GetBytes(out, "response.id").String(), gjson.GetBytes(out, "metadata.response_id").String())
	for _, malformed := range []string{`{"type":"error","message":"audit-private"`, `"audit-private"`, `<html>audit-private</html>`} {
		out, err := maskResponsePayload(c.Request.Context(), account, []byte(malformed), false)
		require.Error(t, err)
		require.Empty(t, out)
	}
	out, err = maskResponsePayload(context.Background(), account, []byte(`{"id":"resp_unbound_private","x-codex-turn-state":"private-state"}`), true)
	require.NoError(t, err)
	require.NotContains(t, string(out), "private")
	// Actual business content must remain replayable, including opaque IDs.
	raw := []byte(`{"id":"resp_protocol","output":[{"id":"msg_keep","call_id":"call_keep","arguments":"{\"session_id\":\"business\"}","encrypted_content":"opaque"}],"text":{"format":{"schema":{"properties":{"account_id":{"type":"string"}}}}}}`)
	out, err = maskResponsePayload(c.Request.Context(), account, raw, true)
	require.NoError(t, err)
	require.JSONEq(t, gjson.GetBytes(raw, "output").Raw, gjson.GetBytes(out, "output").Raw)
	require.JSONEq(t, gjson.GetBytes(raw, "text").Raw, gjson.GetBytes(out, "text").Raw)
}

func TestResponsePrivacyPublicErrors(t *testing.T) {
	for _, raw := range []string{
		`{"type":"error","error":{"message":"account=audit-private","code":"audit-private","details":{"x-codex-turn-state":"audit-private"}}}`,
		`{"type":"response.failed","response":{"id":"resp_public","error":{"code":"context_length_exceeded","message":"audit-private","details":{"account_id":"audit-private"}}}}`,
		`{"type":"error","message":"audit-private","detail":"audit-private","request_id":"audit-private"}`,
		`{"metadata":{"nested":{"error":{"message":"audit-private","details":["audit-private"]}}}}`,
		`{"metadata":"{\"error\":{\"message\":\"audit-private\"}}"}`,
		`{"response":{"status_details":{"error":{"message":"audit-private"}}}}`,
		`{"output":[{"type":"message","metadata":{"error":{"message":"audit-private"}}}]}`,
	} {
		out := publicResponseErrorPayload(nil, []byte(raw))
		require.NotContains(t, string(out), "audit-private")
		require.JSONEq(t, string(out), string(publicResponseErrorPayload(nil, out)), "public projection must be idempotent")
	}
	for _, committed := range []bool{false, true} {
		c, r := newTurnStateTestContext(t)
		if committed {
			c.Writer.Header().Set("Content-Type", "text/event-stream")
			c.Writer.WriteHeaderNow()
		}
		body := []byte(`{"error":{"code":"context_length_exceeded","message":"audit-private","details":{"headers":{"Authorization":"audit-private"}}}}`)
		writeContinuousRetryLastFailure(c, continuousRetryProtocolResponses, continuousRetryFailure{status: 400, body: body})
		require.NotContains(t, r.Body.String(), "audit-private")
		require.Contains(t, r.Body.String(), "context_length_exceeded")
	}
	c, r := newTurnStateTestContext(t)
	ErrorToGinResponse(c, errors.New("websocket handshake failed: OpenAI-Organization=audit-private"))
	require.NotContains(t, r.Body.String(), "audit-private")
	require.Equal(t, http.StatusInternalServerError, r.Code)
	// Safety decisions are rebuilt from gateway state, never upstream details.
	body := []byte(`{"type":"error","error":{"code":"invalid_prompt","message":"Your prompt was flagged as potentially violating our usage policy audit-private","details":{"codex2api_safety":{"session_id":"audit-private"}}}}`)
	out := publicResponseErrorPayload(c, body)
	require.NotContains(t, string(out), "audit-private")
	require.Contains(t, string(out), upstreamPromptSafetyReason)
	// Preserve raw private material for server classification; the public
	// operation must not mutate its caller's buffer.
	require.Contains(t, string(body), "audit-private")
	var object map[string]any
	require.NoError(t, json.Unmarshal(out, &object))
	require.NotEmpty(t, object["error"])
}
