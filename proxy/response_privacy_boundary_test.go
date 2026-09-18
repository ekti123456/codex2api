package proxy

// Regression tests reproduced from the downstream disclosure audit.
import (
	"bytes"
	"encoding/json"
	"io"
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

func TestResponsePrivacyBoundsRawHTTPErrorBeforeMasking(t *testing.T) {
	for _, prefix := range []string{"<html>", `{"error":{"message":"`} {
		t.Run(prefix, func(t *testing.T) {
			upstream := strings.NewReader(prefix + strings.Repeat("x", 4*upstreamErrorBodyReadMaxBytes))
			originalSize := upstream.Len()
			body := &responsePrivacyBody{statusCode: http.StatusBadRequest, body: io.NopCloser(upstream)}
			output, err := io.ReadAll(body)
			require.Error(t, err)
			require.Empty(t, output, "oversized private data must not escape the masking reader")
			// Include bufio's small prefetch allowance, but never consume the
			// entire oversized response before enforcing the error-body limit.
			require.LessOrEqual(t, originalSize-upstream.Len(), upstreamErrorBodyReadMaxBytes+4096)
		})
	}
}

func TestResponsePrivacyDownstreamBoundaries(t *testing.T) {
	for _, transport := range []string{"http_sse", "http_json", "compact_json", "downstream_websocket", "http_error_json", "http_error_plain", "http_error_deactivated"} {
		t.Run(transport, func(t *testing.T) {
			h, owner, _, _ := failoverTestSetup(t, true)
			t.Setenv("CODEX_REQUEST_COMPRESSION", "off")
			oldResin := GetResinConfig()
			t.Cleanup(func() { SetResinConfig(oldResin) })
			businessOutput := json.RawMessage(`[{"type":"reasoning","id":"rs_business","summary":[{"type":"summary_text","text":"{\"account_id\":\"business-account\",\"error\":{\"message\":\"business-error\"}}"}],"encrypted_content":"business-opaque"},{"type":"function_call","id":"fc_business","call_id":"call_business","name":"lookup","arguments":"{\"session_id\":\"business-session\"}"}]`)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				w.Header().Set("X-Request-ID", "audit-http-header-secret")
				w.Header().Set("Set-Cookie", "audit-http-cookie-secret")
				w.Header().Set(codexTurnStateHeader, "audit-protected-turn")
				if strings.HasPrefix(transport, "http_error") {
					w.Header().Set("Content-Type", "application/json")
					if transport == "http_error_plain" {
						w.Header().Set("Content-Type", "text/plain")
						w.WriteHeader(400)
						_, _ = io.WriteString(w, "bad request: account=audit-plain-error-secret")
					} else if transport == "http_error_deactivated" {
						w.WriteHeader(402)
						_, _ = io.WriteString(w, `{"error":{"code":"deactivated_workspace","message":"workspace disabled","details":{"account_id":"audit-nested-error-secret","x-codex-turn-state":"audit-error-raw-turn","response_id":"resp_audit_error_raw"}}}`)
					} else {
						w.WriteHeader(400)
						_, _ = io.WriteString(w, `{"error":{"message":"account=audit-json-error-secret session=audit-session-secret response=resp_should_be_hidden"}}`)
					}
					return
				}
				response := map[string]any{
					"id": "resp_audit_protected", "object": "response", "status": "completed", "model": "gpt-5.5", "output": businessOutput,
					"usage":      map[string]int{"input_tokens": 1, "output_tokens": 1},
					"session_id": "audit-original-session", "thread_id": "audit-original-thread", "account_id": "audit-original-account",
					"metadata":        map[string]any{"nested": map[string]any{"headers": map[string]string{"Authorization": "audit-original-bearer", "Set-Cookie": "audit-original-cookie", "X-Codex-Turn-State": "audit-protected-turn"}, "response_id": "resp_audit_nested_original"}},
					"client_metadata": `{"headers":{"Authorization":"audit-encoded-bearer"},"installation_id":"audit-original-installation"}`,
					"headers":         map[string]string{"X-Request-ID": "audit-protected-header", "X-Codex-Turn-State": "audit-protected-turn"},
				}
				if transport == "compact_json" {
					response["object"] = "response.compaction"
					w.Header().Set("Content-Type", "application/json")
					require.NoError(t, json.NewEncoder(w).Encode(response))
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				payload, _ := json.Marshal(map[string]any{"type": "response.completed", "response": response})
				_, _ = io.WriteString(w, "id: audit-sse-id-secret\n: audit-sse-comment-secret\ndata: "+string(payload)+"\n\n")
			}))
			t.Cleanup(upstream.Close)
			SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "privacy-audit-local"})
			root := uuid.Must(uuid.NewV7()).String()
			_, body := failoverTestRequest(t, h)
			body = bytes.ReplaceAll(body, []byte(continuityTestThread), []byte(root))
			body, _ = sjson.SetBytes(body, "stream", transport != "http_json" && transport != "compact_json")
			body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.turn_id", uuid.Must(uuid.NewV7()).String())
			probe, _ := newTurnStateTestContext(t)
			probe.Set(contextAPIKeyID, int64(101))
			probe.Request.Header.Set("Authorization", "Bearer test-user-key")
			identity := h.resolveRequestSessionIdentityForContext(probe, body)
			key := capacityAwareSessionAffinityKey(identity, 101)
			_, err := h.db.CommitSessionContinuity(t.Context(), hashRiskIdentity(key), database.SessionContinuityRecord{AccountID: owner.ID(), ThreadID: root, NumberKnown: true, LastSeen: time.Now()})
			require.NoError(t, err)
			h.store.BindSessionAffinity(key, owner, "")
			var output string
			if transport == "downstream_websocket" {
				engine := gin.New()
				engine.GET("/v1/responses", func(c *gin.Context) { c.Set(contextAPIKeyID, int64(101)); h.ResponsesWebSocket(c) })
				server := httptest.NewServer(engine)
				t.Cleanup(server.Close)
				conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", http.Header{"Authorization": []string{"Bearer test-user-key"}})
				require.NoError(t, err)
				defer conn.Close()
				body, _ = sjson.SetBytes(body, "type", "response.create")
				require.NoError(t, conn.WriteMessage(websocket.TextMessage, body))
				require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
				for {
					_, data, err := conn.ReadMessage()
					require.NoError(t, err)
					output += string(data)
					require.NotEqual(t, "error", gjson.GetBytes(data, "type").String(), "%s", data)
					if gjson.GetBytes(data, "type").String() == "response.completed" {
						break
					}
				}
			} else {
				r := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(r)
				c.Set(contextAPIKeyID, int64(101))
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
				c.Request.Header.Set("Authorization", "Bearer test-user-key")
				if transport == "compact_json" {
					c.Request.URL.Path = "/v1/responses/compact"
					h.ResponsesCompact(c)
				} else {
					h.Responses(c)
				}
				if !strings.HasPrefix(transport, "http_error") {
					require.Equal(t, 200, r.Code, "%s", r.Body.String())
				}
				output = r.Body.String()
				require.NotContains(t, r.Header().Get("X-Request-ID"), "audit-http-header-secret")
				require.Empty(t, r.Header().Get("Set-Cookie"))
			}
			if strings.HasPrefix(transport, "http_error") {
				marker := map[string]string{"http_error_json": "audit-json-error-secret", "http_error_plain": "audit-plain-error-secret", "http_error_deactivated": "audit-nested-error-secret"}[transport]
				if transport == "http_error_plain" {
					require.NotContains(t, output, marker)
					t.Log("CONTROL: non-JSON upstream HTTP error text suppressed by final handler")
					return
				}
				require.NotContains(t, output, marker)
				require.NotContains(t, output, "resp_should_be_hidden")
				if transport == "http_error_deactivated" {
					require.NotContains(t, output, "audit-error-raw-turn")
					require.NotContains(t, output, "resp_audit_error_raw")
				}
				t.Log("Upstream error details are private: " + transport)
				return
			}
			for _, marker := range []string{"audit-original-session", "audit-original-thread", "audit-original-account", "audit-original-bearer", "audit-original-cookie", "audit-encoded-bearer", "audit-original-installation", "resp_audit_nested_original"} {
				require.NotContains(t, output, marker, transport)
			}
			for _, marker := range []string{"audit-protected-turn", "audit-protected-header", "resp_audit_protected", "audit-sse-id-secret", "audit-sse-comment-secret"} {
				require.NotContains(t, output, marker, transport)
			}
			t.Log("Identity fields, nested credentials, response IDs and Turn-State are protected")
			var completed gjson.Result
			if transport == "http_json" || transport == "compact_json" {
				completed = gjson.Parse(output)
			} else {
				for _, line := range strings.Split(output, "\n") {
					data := strings.TrimPrefix(line, "data: ")
					if gjson.Get(data, "type").String() == "response.completed" {
						completed = gjson.Get(data, "response")
					}
				}
			}
			require.JSONEq(t, string(businessOutput), completed.Get("output").Raw)
			require.Equal(t, "gpt-5.5", completed.Get("model").String())
			require.EqualValues(t, 1, completed.Get("usage.input_tokens").Int())
			require.EqualValues(t, 1, completed.Get("usage.output_tokens").Int())
		})
	}
}
