package proxy

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

type downstreamOfficialCase struct {
	Name              string
	Item              json.RawMessage
	ExpectedPreserved bool
}

var downstreamOfficialCases = []downstreamOfficialCase{
	{"message_phase", json.RawMessage(`{"id":"msg_audit","type":"message","role":"assistant","phase":"commentary","content":[{"type":"output_text","text":"{\"user\":\"business\"}","annotations":[]}]}`), true},
	{"reasoning", json.RawMessage(`{"id":"rs_audit","type":"reasoning","summary":[{"type":"summary_text","text":"{\"error\":\"business\"}"}],"encrypted_content":"gAAAA-audit"}`), true},
	{"function_call", json.RawMessage(`{"id":"fc_audit","type":"function_call","call_id":"call_audit","name":"lookup","namespace":"functions","encrypted_function_args":["opaque"],"arguments":"{\"account_id\":\"customer\",\"user\":\"business\"}"}`), true},
	{"custom_tool", json.RawMessage(`{"id":"ct_audit","type":"custom_tool_call","call_id":"call_custom","name":"apply_patch","input":"{\"session_id\":\"business\"}"}`), true},
	{"shell_action", json.RawMessage(`{"id":"sh_audit","type":"local_shell_call","call_id":"call_shell","status":"completed","action":{"type":"exec","command":["echo","hello"],"env":{"TOKEN":"business"},"user":"sandbox"}}`), true},
	{"tool_search", json.RawMessage(`{"id":"ts_audit","type":"tool_search_output","status":"completed","execution":"client","call_id":"call_search","tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"user":{"type":"string"},"session_id":{"type":"string"},"error":{"type":"object"}}}}]}`), true},
	{"mcp_tools", json.RawMessage(`{"id":"mt_audit","type":"mcp_list_tools","server_label":"docs","tools":[{"name":"lookup","input_schema":{"properties":{"user":{"type":"string"}}},"annotations":{"user":"business"}}]}`), true},
	{"tool_output", json.RawMessage(`{"id":"fo_audit","type":"function_call_output","call_id":"call_audit","output":[{"type":"input_text","text":"{\"session_id\":\"business\",\"error\":\"business\"}"}]}`), true},
	{"compaction", json.RawMessage(`{"id":"cmp_audit","type":"compaction","encrypted_content":"gAAAA-compaction-audit"}`), true},
	{"configuration_update", json.RawMessage(`{"type":"configuration_update","reasoning":{"effort":"high"}}`), true},
	{"agent_message", json.RawMessage(`{"id":"am_audit","type":"agent_message","author":"/root/a","recipient":"/root","content":[{"type":"input_text","text":"business"}]}`), true},
	{"program_code", json.RawMessage(`{"id":"pg_audit","type":"program","call_id":"call_program","code":"{ const count = 1; text(count); }","fingerprint":"opaque-replay-fingerprint"}`), true},
	{"program_result", json.RawMessage(`{"id":"po_audit","type":"program_output","call_id":"call_program","status":"completed","result":"{\"user\":\"business\",\"session_id\":\"business-session\",\"count\":1}"}`), true},
	{"mcp_execution_error", json.RawMessage(`{"id":"mc_audit","type":"mcp_call","call_id":"call_mcp","name":"lookup","server_label":"docs","arguments":"{}","error":{"type":"mcp_tool_execution_error","content":[{"type":"text","text":"Unknown column customer_id; use id"}]}}`), true},
	{"mcp_list_error", json.RawMessage(`{"id":"ml_audit","type":"mcp_list_tools","server_label":"docs","tools":[],"error":"Server requires setup before listing tools"}`), true},
	{"image_revised_prompt", json.RawMessage(`{"id":"ig_audit","type":"image_generation_call","status":"completed","result":"BASE64_IMAGE","revised_prompt":"{\"user\":\"a woman\",\"style\":\"watercolor\"}"}`), true},
	{"mcp_approval_reason", json.RawMessage(`{"id":"ap_audit","type":"mcp_approval_response","approval_request_id":"approval_audit","approve":false,"reason":"{\"user\":\"business reason\",\"reason\":\"wrong project\"}"}`), true},
	{"item_turn_metadata", json.RawMessage(`{"id":"msg_meta","type":"message","role":"assistant","content":[{"type":"output_text","text":"hello"}],"internal_chat_message_metadata_passthrough":{"turn_id":"turn-original","create_time":1.5,"content_item_kinds":["plain"]}}`), false},
}

func TestDownstreamOfficialItems(t *testing.T) {
	h, account, _, _ := responsePrivacySetup(t)
	c, _, _ := responsePrivacyRequest(t, h, 101, "audit", "")
	for _, tc := range downstreamOfficialCases {
		for _, carrier := range []string{"json", "item_done", "completed", "sse"} {
			payload := []byte(`{"id":"resp_audit","output":[` + string(tc.Item) + `]}`)
			path := "output.0"
			if carrier == "item_done" {
				payload = []byte(`{"type":"response.output_item.done","item":` + string(tc.Item) + `}`)
				path = "item"
			}
			if carrier == "completed" || carrier == "sse" {
				payload = []byte(`{"type":"response.completed","response":` + string(payload) + `}`)
				path = "response.output.0"
			}
			var masked []byte
			var err error
			if carrier == "sse" {
				stream := &turnStateStream{ctx: c.Request.Context(), account: account}
				var frame []byte
				frame, err = stream.maskFrame(append(append([]byte("data: "), payload...), []byte("\n\n")...))
				masked = bytes.TrimSpace(bytes.TrimPrefix(frame, []byte("data: ")))
			} else {
				masked, err = maskResponsePayload(c.Request.Context(), account, payload, carrier == "json")
			}
			require.NoError(t, err, tc.Name)
			final := publicResponseErrorPayload(c, masked)
			var before, after any
			require.NoError(t, json.Unmarshal(tc.Item, &before))
			require.NoError(t, json.Unmarshal([]byte(gjson.GetBytes(final, path).Raw), &after))
			a, _ := json.Marshal(before)
			b, _ := json.Marshal(after)
			preserved := bytes.Equal(a, b)
			require.Equal(t, tc.ExpectedPreserved, preserved, tc.Name+"/"+carrier+": "+string(final))
		}
	}
	t.Logf("checked %d item/carrier combinations", len(downstreamOfficialCases)*4)
}

func TestDownstreamOfficialFinalWire(t *testing.T) {
	for _, transport := range []string{"http_sse", "http_json", "compact_json", "downstream_websocket"} {
		t.Run(transport, func(t *testing.T) {
			h, owner, _, _ := failoverTestSetup(t, true)
			t.Setenv("CODEX_REQUEST_COMPRESSION", "off")
			old := GetResinConfig()
			t.Cleanup(func() { SetResinConfig(old) })
			items := make([]json.RawMessage, 0, len(downstreamOfficialCases))
			for _, tc := range downstreamOfficialCases {
				items = append(items, tc.Item)
			}
			original := map[string]any{"id": "resp_wire_audit", "object": "response", "status": "completed", "model": "gpt-5.5", "output": items, "usage": map[string]any{"input_tokens": 42, "output_tokens": 7, "input_tokens_details": map[string]int{"cached_tokens": 32}, "output_tokens_details": map[string]int{"reasoning_tokens": 3}}}
			seenHistory := make(chan string, 1)
			upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				request, _ := io.ReadAll(r.Body)
				historyTurn := gjson.GetBytes(request, "input.0.internal_chat_message_metadata_passthrough.turn_id").String()
				select {
				case seenHistory <- historyTurn:
				default:
				}
				replyItems := append([]json.RawMessage(nil), items...)
				replyItems[len(replyItems)-1], _ = sjson.SetBytes(replyItems[len(replyItems)-1], "internal_chat_message_metadata_passthrough.turn_id", historyTurn)
				reply := make(map[string]any)
				for key, value := range original {
					reply[key] = value
				}
				reply["output"] = replyItems
				w.Header().Set("OpenAI-Model", "gpt-5.5")
				w.Header().Set("X-Reasoning-Included", "true")
				w.Header().Set("X-Models-Etag", "audit-model-etag")
				w.Header().Set("X-Codex-Turn-State", "audit-upstream-turn-state")
				if transport == "compact_json" {
					w.Header().Set("Content-Type", "application/json")
					_ = json.NewEncoder(w).Encode(reply)
					return
				}
				w.Header().Set("Content-Type", "text/event-stream")
				for _, item := range replyItems {
					event, _ := json.Marshal(map[string]any{"type": "response.output_item.done", "item": item})
					_, _ = io.WriteString(w, "data: "+string(event)+"\n\n")
				}
				event, _ := json.Marshal(map[string]any{"type": "response.completed", "response": reply})
				_, _ = io.WriteString(w, "data: "+string(event)+"\n\n")
			}))
			t.Cleanup(upstream.Close)
			SetResinConfig(&ResinConfig{BaseURL: upstream.URL, PlatformName: "downstream-schema-audit"})
			root := uuid.Must(uuid.NewV7()).String()
			_, body := failoverTestRequest(t, h)
			body = bytes.ReplaceAll(body, []byte(continuityTestThread), []byte(root))
			body, _ = sjson.SetBytes(body, "stream", transport != "http_json" && transport != "compact_json")
			body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.turn_id", uuid.Must(uuid.NewV7()).String())
			body, _ = sjson.SetRawBytes(body, "input", []byte(`[{"type":"message","role":"user","content":"test","internal_chat_message_metadata_passthrough":{"turn_id":"turn-original"}}]`))
			probe, _ := newTurnStateTestContext(t)
			probe.Set(contextAPIKeyID, int64(101))
			probe.Request.Header.Set("Authorization", "Bearer test-user-key")
			identity := h.resolveRequestSessionIdentityForContext(probe, body)
			key := capacityAwareSessionAffinityKey(identity, 101)
			_, err := h.db.CommitSessionContinuity(t.Context(), hashRiskIdentity(key), database.SessionContinuityRecord{AccountID: owner.ID(), ThreadID: root, NumberKnown: true, LastSeen: time.Now()})
			require.NoError(t, err)
			h.store.BindSessionAffinity(key, owner, "")
			var output string
			headers := make(http.Header)
			if transport == "downstream_websocket" {
				engine := gin.New()
				engine.GET("/v1/responses", func(c *gin.Context) { c.Set(contextAPIKeyID, int64(101)); h.ResponsesWebSocket(c) })
				server := httptest.NewServer(engine)
				t.Cleanup(server.Close)
				conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(server.URL, "http")+"/v1/responses", http.Header{"Authorization": {"Bearer test-user-key"}})
				require.NoError(t, err)
				defer conn.Close()
				body, _ = sjson.SetBytes(body, "type", "response.create")
				require.NoError(t, conn.WriteMessage(websocket.TextMessage, body))
				require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
				for {
					_, data, err := conn.ReadMessage()
					require.NoError(t, err)
					output += string(data) + "\n"
					require.NotEqual(t, "error", gjson.GetBytes(data, "type").String(), string(data))
					if gjson.GetBytes(data, "type").String() == "response.completed" {
						break
					}
				}
			} else {
				w := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(w)
				c.Set(contextAPIKeyID, int64(101))
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
				c.Request.Header.Set("Authorization", "Bearer test-user-key")
				if transport == "compact_json" {
					c.Request.URL.Path = "/v1/responses/compact"
					h.ResponsesCompact(c)
				} else {
					h.Responses(c)
				}
				require.Equal(t, 200, w.Code, w.Body.String())
				output = w.Body.String()
				headers = w.Header().Clone()
			}
			var response gjson.Result
			if transport == "http_json" || transport == "compact_json" {
				response = gjson.Parse(output)
			} else {
				for _, line := range strings.Split(output, "\n") {
					data := strings.TrimPrefix(line, "data: ")
					if gjson.Get(data, "type").String() == "response.completed" {
						response = gjson.Get(data, "response")
					}
				}
			}
			require.True(t, response.IsObject(), output)
			historyTurn := <-seenHistory
			require.NotEmpty(t, historyTurn)
			require.NotEqual(t, "turn-original", historyTurn)
			for i, tc := range downstreamOfficialCases {
				actual := response.Get("output").Array()[i]
				if transport == "http_json" && tc.Name == "image_revised_prompt" { // existing image collector adds the decoded byte count
					require.EqualValues(t, 9, actual.Get("bytes").Int())
					withoutBytes, err := sjson.Delete(actual.Raw, "bytes")
					require.NoError(t, err)
					actual = gjson.Parse(withoutBytes)
				}
				if tc.ExpectedPreserved || tc.Name == "item_turn_metadata" {
					require.JSONEq(t, string(tc.Item), actual.Raw, transport+"/"+tc.Name)
				}
				if tc.Name == "program_code" {
					require.True(t, actual.Get("code").Exists())
				}
				if tc.Name == "mcp_list_error" {
					require.Equal(t, gjson.String, actual.Get("error").Type)
				}
			}
			if transport != "downstream_websocket" {
				require.Equal(t, "true", headers.Get("X-Reasoning-Included"))
				require.Equal(t, "gpt-5.5", headers.Get("OpenAI-Model"))
				require.NotEmpty(t, headers.Get("X-Models-Etag"))
				require.NotEqual(t, "audit-model-etag", headers.Get("X-Models-Etag"))
			}
			require.EqualValues(t, 42, response.Get("usage.input_tokens").Int())
			require.Equal(t, "gpt-5.5", response.Get("model").String())
		})
	}
}
