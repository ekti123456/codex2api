package wsrelay

// Temporary local-only disclosure probes; no real provider or credentials.
import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
	"github.com/google/uuid"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestResponsePrivacyRealWSUpstream(t *testing.T) {
	for _, scenario := range []string{"sse", "native_ws", "http_handshake_error", "ws_handshake_hidden", "ws_handshake_visible"} {
		t.Run(scenario, func(t *testing.T) {
			oldRuntime, oldResin, oldExecutor := proxy.CurrentRuntimeSettings(), proxy.GetResinConfig(), proxy.WebsocketExecuteFunc
			t.Cleanup(func() {
				proxy.ApplyRuntimeSettings(oldRuntime)
				proxy.SetResinConfig(oldResin)
				proxy.WebsocketExecuteFunc = oldExecutor
			})
			settings := proxy.DefaultRuntimeSettings()
			settings.CodexSessionFailoverEnabled, settings.CodexForceWebsocket = true, true
			settings.CodexWSSilentRetry, settings.CodexWSSilentRetries = false, 0
			settings.CodexWSHideErrors = scenario != "ws_handshake_visible"
			proxy.ApplyRuntimeSettings(settings)
			t.Setenv("CODEX_REQUEST_COMPRESSION", "off")
			db, err := database.New("sqlite", filepath.Join(t.TempDir(), "audit.db"))
			require.NoError(t, err)
			t.Cleanup(func() { require.NoError(t, db.Close()) })
			memory := cache.NewMemory(100)
			t.Cleanup(func() { require.NoError(t, memory.Close()) })
			store := auth.NewStore(nil, memory, nil)
			store.SetMaxRetries(0)
			store.SetMaxRateLimitRetries(0)
			store.SetRetryIntervalMS(1)
			t.Cleanup(store.Stop)
			store.AddAccount(&auth.Account{DBID: 1695, AccountID: "661373c1-f1a9-4ca9-8682-a0594b30c36c", AccessToken: "audit-token", Status: auth.StatusReady, Models: []string{"gpt-5.6-sol"}, CodexFingerprintMode: auth.CodexFingerprintModeDevice})
			handler := proxy.NewHandler(store, db, nil, nil)
			handler.SetRuntimeCache(memory)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				if strings.Contains(scenario, "handshake") {
					w.Header().Set("Content-Type", "application/json")
					w.Header().Set("OpenAI-Organization", "audit-handshake-org")
					w.Header().Set("X-Request-ID", "audit-handshake-trace")
					w.WriteHeader(503)
					_, _ = io.WriteString(w, `{"error":{"message":"session=audit-handshake-session response=resp_handshake_original","details":{"x-codex-turn-state":"audit-handshake-turn"}}}`)
					return
				}
				conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
				if err != nil {
					return
				}
				defer conn.Close()
				if _, _, err := conn.ReadMessage(); err != nil {
					return
				}
				_ = conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"resp_ws_protected","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1},"session_id":"audit-ws-session","metadata":{"nested":{"headers":{"Authorization":"audit-ws-bearer","X-Codex-Turn-State":"audit-ws-protected-turn"},"response_id":"resp_ws_nested_original"}}}}`))
				_, _, _ = conn.ReadMessage()
			}))
			t.Cleanup(server.Close)
			proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: server.URL, PlatformName: "privacy-audit-local-ws"})
			manager := NewManager()
			t.Cleanup(manager.Stop)
			executor := NewExecutorWithManager(manager)
			proxy.WebsocketExecuteFunc = func(ctx context.Context, a *auth.Account, body []byte, session, proxyURL, key string, config *proxy.DeviceProfileConfig, headers http.Header, pool string) (*http.Response, error) {
				response, err := executor.ExecuteRequestViaWebsocket(ctx, a, body, session, proxyURL, key, config, headers, pool)
				if err != nil {
					return nil, err
				}
				return websocketResponseToHTTP(ctx, response, http.StatusOK, nil), nil
			}
			root := uuid.Must(uuid.NewV7()).String()
			body := []byte(fmt.Sprintf(`{"model":"gpt-5.6-sol","stream":true,"input":"audit","client_metadata":{"session_id":"%s","thread_id":"%s","x-codex-turn-metadata":{"session_id":"%s","thread_id":"%s","thread_source":"user","request_kind":"turn","window_id":"%s:0","window_number":0}}}`, root, root, root, root, root))
			body = addSessionWireTools(t, body)
			native := scenario == "native_ws" || strings.HasPrefix(scenario, "ws_handshake")
			var output string
			if native {
				engine := gin.New()
				engine.GET("/v1/responses", handler.ResponsesWebSocket)
				frontend := httptest.NewServer(engine)
				t.Cleanup(frontend.Close)
				conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(frontend.URL, "http")+"/v1/responses", http.Header{"Authorization": {"Bearer test-user-key"}})
				require.NoError(t, err)
				defer conn.Close()
				body, _ = sjson.SetBytes(body, "type", "response.create")
				require.NoError(t, conn.SetReadDeadline(time.Now().Add(8*time.Second)))
				require.NoError(t, conn.WriteMessage(websocket.TextMessage, body))
				for {
					_, data, err := conn.ReadMessage()
					require.NoError(t, err)
					output += string(data)
					kind := gjson.GetBytes(data, "type").String()
					if kind == "error" || kind == "response.completed" || kind == "response.failed" {
						break
					}
				}
			} else {
				r := httptest.NewRecorder()
				c, _ := gin.CreateTestContext(r)
				c.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
				c.Request.Header.Set("Authorization", "Bearer test-user-key")
				ctx, cancel := context.WithTimeout(c.Request.Context(), 8*time.Second)
				defer cancel()
				c.Request = c.Request.WithContext(ctx)
				handler.Responses(c)
				output = r.Body.String()
			}
			if strings.Contains(scenario, "handshake") {
				require.Contains(t, output, `"error"`)
				for _, marker := range []string{"audit-handshake-org", "audit-handshake-trace", "audit-handshake-session", "resp_handshake_original", "audit-handshake-turn"} {
					require.NotContains(t, output, marker)
				}
				t.Log("Handshake downstream result confirmed: " + scenario)
			} else {
				require.Contains(t, output, "response.completed")
				for _, marker := range []string{"audit-ws-session", "audit-ws-bearer", "resp_ws_nested_original"} {
					require.NotContains(t, output, marker)
				}
				for _, marker := range []string{"resp_ws_protected", "audit-ws-protected-turn"} {
					require.NotContains(t, output, marker)
				}
				t.Log("Protected real WS upstream to " + scenario + " masks identity, nested credentials, IDs and Turn-State")
			}
		})
	}
}
