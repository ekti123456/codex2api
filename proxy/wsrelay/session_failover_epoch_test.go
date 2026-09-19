package wsrelay

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"strings"
	"sync/atomic"
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

func TestWebsocketSessionFailoverResetsWindowAndConnection(test *testing.T) {
	for _, native := range []bool{false, true} {
		test.Run(fmt.Sprintf("native_ingress=%v", native), func(t *testing.T) { runWebsocketToolFailover(t, native) })
	}
}

func runWebsocketToolFailover(test *testing.T, native bool, preserve ...bool) {
	keepInput := len(preserve) > 0 && preserve[0]
	runWebsocketToolFailoverScenario(test, native, keepInput, false)
}

func TestWebsocketSessionQuotaRetry(t *testing.T) {
	for _, native := range []bool{false, true} {
		for _, preserve := range []bool{false, true} {
			t.Run(fmt.Sprintf("native=%v/preserve=%v", native, preserve), func(t *testing.T) {
				runWebsocketToolFailoverScenario(t, native, preserve, true)
			})
		}
	}
}

func TestTurnStatePrefixedMetadataWebsocketFailover(t *testing.T) {
	for _, kind := range []string{"codex.response.metadata", "responsesapi.response.metadata"} {
		for _, native := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/native=%v", kind, native), func(t *testing.T) {
				runWebsocketToolFailoverScenario(t, native, false, false, kind)
			})
		}
	}
}

func runWebsocketToolFailoverScenario(test *testing.T, native, keepInput, quota bool, metadataTypes ...string) {
	metadataType := "response.metadata"
	if len(metadataTypes) > 0 {
		metadataType = metadataTypes[0]
	}
	oldRuntime, oldResin, oldExecutor := proxy.CurrentRuntimeSettings(), proxy.GetResinConfig(), proxy.WebsocketExecuteFunc
	test.Cleanup(func() {
		proxy.ApplyRuntimeSettings(oldRuntime)
		proxy.SetResinConfig(oldResin)
		proxy.WebsocketExecuteFunc = oldExecutor
	})
	test.Setenv("CODEX_REQUEST_COMPRESSION", "off")
	settings := proxy.DefaultRuntimeSettings()
	settings.CodexSessionFailoverEnabled, settings.CodexForceWebsocket = true, true
	settings.CodexSessionFailoverPreserveInput = keepInput
	settings.CodexWSSilentRetry, settings.CodexWSSilentRetries = true, 1
	proxy.ApplyRuntimeSettings(settings)
	dbPath := filepath.Join(test.TempDir(), "epoch.db")
	db, err := database.New("sqlite", dbPath)
	require.NoError(test, err)
	test.Cleanup(func() { require.NoError(test, db.Close()) })
	memory := cache.NewMemory(100)
	test.Cleanup(func() { require.NoError(test, memory.Close()) })
	store := auth.NewStore(nil, memory, nil)
	store.SetMaxRetries(0)
	store.SetMaxRateLimitRetries(1)
	store.SetRetryIntervalMS(1)
	store.SetTransportRetryPolicy("sticky")
	test.Cleanup(store.Stop)
	first := &auth.Account{DBID: 1695, AccountID: "661373c1-f1a9-4ca9-8682-a0594b30c36c", AccessToken: "first-token", Status: auth.StatusReady, Models: []string{"gpt-5.6-sol"}, CodexFingerprintMode: auth.CodexFingerprintModeDevice}
	second := &auth.Account{DBID: 1696, AccountID: "761373c1-f1a9-4ca9-8682-a0594b30c36c", AccessToken: "second-token", Status: auth.StatusReady, Models: []string{"gpt-5.6-sol"}, CodexFingerprintMode: auth.CodexFingerprintModeDevice, Disabled: 1}
	store.AddAccount(first)
	store.AddAccount(second)
	handler := proxy.NewHandler(store, db, nil, nil)
	handler.SetRuntimeCache(memory)
	type capture struct {
		headers    http.Header
		body       []byte
		connection int32
	}
	seen := make(chan capture, 8)
	var connections atomic.Int32
	var quotaActive atomic.Bool
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := (&websocket.Upgrader{}).Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		index := connections.Add(1)
		for {
			_, body, err := connection.ReadMessage()
			if err != nil {
				return
			}
			seen <- capture{request.Header.Clone(), body, index}
			require.NotContains(test, string(body), "unverified-nested-state")
			require.NotContains(test, request.Header.Get("X-Codex-Turn-Metadata"), "unverified-nested-state")
			if quotaActive.Load() && request.Header.Get("Authorization") == "Bearer first-token" {
				// Lifecycle/metadata frames are not visible answer content and must
				// not accidentally prevent a safe pre-content quota retry.
				if err := connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"quota-failed-attempt"}}`)); err != nil {
					return
				}
				if err := connection.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":%q,"headers":{"x-codex-turn-state":"real-turn-state-1"}}`, metadataType))); err != nil {
					return
				}
				failure := fmt.Sprintf(`{"type":"response.failed","response":{"error":{"type":"usage_limit_reached","code":"usage_limit_reached","message":"quota exhausted","resets_at":%d,"plan_type":"plus"}}}`, time.Now().Add(time.Hour).Unix())
				if err := connection.WriteMessage(websocket.TextMessage, []byte(failure)); err != nil {
					return
				}
				continue
			}
			if err := connection.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":%q,"headers":{"x-codex-turn-state":"real-turn-state-%d"},"client_metadata":{"nested":{"X-Codex-Turn-State":"real-turn-state-%d"}}}`, metadataType, index, index))); err != nil {
				return
			}
			if err := connection.WriteMessage(websocket.TextMessage, []byte(fmt.Sprintf(`{"type":"response.completed","response":{"id":"epoch-response","status":"completed","output":[{"type":"reasoning","id":"epoch-reasoning","encrypted_content":"gAAAAepoch-state-%d"},{"type":"function_call","call_id":"tool-call","name":"exec_command","arguments":"{}"}],"usage":{"input_tokens":1,"output_tokens":1}}}`, index))); err != nil {
				return
			}
		}
	}))
	test.Cleanup(server.Close)
	proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: server.URL, PlatformName: "epoch-ws"})
	manager := NewManager()
	test.Cleanup(manager.Stop)
	executor := NewExecutorWithManager(manager)
	proxy.WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, body []byte, session, proxyURL, key string, config *proxy.DeviceProfileConfig, headers http.Header, pool string) (*http.Response, error) {
		response, err := executor.ExecuteRequestViaWebsocket(ctx, account, body, session, proxyURL, key, config, headers, pool)
		if err != nil {
			return nil, err
		}
		return websocketResponseToHTTP(ctx, response, http.StatusOK, nil), nil
	}
	var downstream *websocket.Conn
	if native {
		engine := gin.New()
		engine.GET("/v1/responses", handler.ResponsesWebSocket)
		frontend := httptest.NewServer(engine)
		test.Cleanup(frontend.Close)
		downstream, _, err = websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(frontend.URL, "http")+"/v1/responses", http.Header{"Authorization": {"Bearer test-user-key"}})
		require.NoError(test, err)
		test.Cleanup(func() { _ = downstream.Close() })
	}
	rootID, err := uuid.NewV7()
	require.NoError(test, err)
	root := rootID.String()
	var captures []capture
	const originalTurn = "01a095b5-86a3-7ec2-af42-0bb1111ef330"
	var mappedTurns []string
	var clientAlias string
	steps := 5
	if quota {
		steps = 4
	}
	for number := 0; number < steps; number++ {
		if number == 2 {
			if quota {
				quotaActive.Store(true)
			} else {
				atomic.StoreInt32(&first.Disabled, 1)
			}
			atomic.StoreInt32(&second.Disabled, 0)
		}
		if number == 3 && !native {
			require.NoError(test, db.Close())
			db, err = database.New("sqlite", dbPath)
			require.NoError(test, err)
			handler = proxy.NewHandler(store, db, nil, nil)
			handler.SetRuntimeCache(memory)
		}
		if number == 4 {
			atomic.StoreInt32(&first.Disabled, 0)
			atomic.StoreInt32(&second.Disabled, 1)
		}
		body := []byte(fmt.Sprintf(`{"model":"gpt-5.6-sol","stream":true,"input":"full plaintext context","client_metadata":{"session_id":"%s","thread_id":"%s","x-codex-turn-metadata":{"session_id":"%s","thread_id":"%s","thread_source":"user","request_kind":"turn","window_id":"%s:%d","window_number":%d}}}`, root, root, root, root, root, number, number))
		body, _ = sjson.SetBytes(body, "client_metadata.X-Codex-Turn-State", "unverified-nested-state")
		body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.nested.x-codex-turn-state", "unverified-nested-state")
		for _, field := range []string{"turn_id", "root_turn_id"} {
			body, err = sjson.SetBytes(body, "client_metadata."+field, originalTurn)
			require.NoError(test, err)
			body, err = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata."+field, originalTurn)
			require.NoError(test, err)
		}
		if number == 1 || number == 3 {
			body, err = sjson.SetRawBytes(body, "input", []byte(fmt.Sprintf(`[{"type":"reasoning","id":"epoch-reasoning","encrypted_content":"gAAAAepoch-state-%d"},{"role":"user","content":"continue"}]`, number/2+1)))
			require.NoError(test, err)
		}
		if number == 2 || number == 4 {
			body, err = sjson.SetRawBytes(body, "input", []byte(fmt.Sprintf(`[{"type":"reasoning","encrypted_content":"gAAAAepoch-state-%d"},{"type":"compaction","encrypted_content":"gAAAAold-compaction"},{"role":"user","content":[{"type":"input_file","file_id":"old-file"},{"type":"input_text","text":"current task"}]}]`, number/2)))
			require.NoError(test, err)
		}
		body = addSessionWireTools(test, body)
		if clientAlias != "" && native {
			body, err = sjson.SetBytes(body, "client_metadata.x-codex-turn-state", clientAlias)
			require.NoError(test, err)
		}
		previousAlias := clientAlias
		if native {
			body, err = sjson.SetBytes(body, "type", "response.create")
			require.NoError(test, err)
			require.NoError(test, downstream.SetReadDeadline(time.Now().Add(5*time.Second)))
			require.NoError(test, downstream.WriteMessage(websocket.TextMessage, body))
			for {
				_, event, readErr := downstream.ReadMessage()
				require.NoError(test, readErr)
				kind := gjson.GetBytes(event, "type").String()
				require.NotEqual(test, "error", kind, string(event))
				require.NotEqual(test, "response.failed", kind, string(event))
				require.NotContains(test, string(event), "real-turn-state-")
				if kind == metadataType {
					clientAlias = gjson.GetBytes(event, "headers.x-codex-turn-state").String()
				}
				if kind == "response.completed" {
					require.Contains(test, string(event), "exec_command")
					break
				}
			}
		} else {
			recorder := httptest.NewRecorder()
			request, _ := gin.CreateTestContext(recorder)
			request.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
			request.Request.Header.Set("Authorization", "Bearer test-user-key")
			request.Request.Header.Set("X-Codex-Turn-Metadata", gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata").Raw)
			if clientAlias != "" {
				request.Request.Header.Set("X-Codex-Turn-State", clientAlias)
			}
			ctx, cancel := context.WithTimeout(request.Request.Context(), 5*time.Second)
			request.Request = request.Request.WithContext(ctx)
			handler.Responses(request)
			cancel()
			require.Equal(test, http.StatusOK, recorder.Code, recorder.Body.String())
			require.Contains(test, recorder.Body.String(), "response.completed")
			require.Contains(test, recorder.Body.String(), "exec_command")
			require.NotContains(test, recorder.Body.String(), "real-turn-state-")
			// The Codex HTTP client saves the actual response header, not SSE
			// metadata. Result() snapshots headers at the first downstream write.
			clientAlias = recorder.Result().Header.Get("X-Codex-Turn-State")
		}
		require.True(test, database.ValidCodexTurnStateAlias(clientAlias))
		if number == 1 || number == 3 {
			require.Equal(test, previousAlias, clientAlias)
		} else {
			require.NotEqual(test, previousAlias, clientAlias)
		}
		select {
		case sent := <-seen:
			if quota && number == 2 {
				require.Equal(test, "Bearer first-token", sent.headers.Get("Authorization"))
				require.Equal(test, "real-turn-state-1", gjson.GetBytes(sent.body, "client_metadata.x-codex-turn-state").String())
				select {
				case sent = <-seen:
					require.Equal(test, "Bearer second-token", sent.headers.Get("Authorization"))
				case <-time.After(time.Second):
					test.Fatal("quota retry did not reach the new account")
				}
			}
			captures = append(captures, sent)
			assertSessionWireTools(test, sent.body)
			state := gjson.GetBytes(sent.body, "client_metadata.x-codex-turn-state").String()
			if number == 1 || number == 3 {
				require.Equal(test, fmt.Sprintf("real-turn-state-%d", sent.connection), state)
			} else {
				require.Empty(test, state)
			}
			require.False(test, db.IsManagedCodexTurnStateAlias(state))
			if keepInput && number >= 2 {
				require.JSONEq(test, gjson.GetBytes(body, "input").Raw, gjson.GetBytes(sent.body, "input").Raw)
			}
			if !keepInput && (number == 2 || number == 4) {
				require.NotContains(test, string(sent.body), "gAAAAepoch-state-")
				require.NotContains(test, string(sent.body), "gAAAAold-compaction")
				require.NotContains(test, string(sent.body), "old-file")
				require.Contains(test, string(sent.body), "current task")
			}
			if number == 3 {
				require.Contains(test, string(sent.body), "gAAAAepoch-state-2")
			}
			meta := gjson.Parse(gjson.GetBytes(sent.body, "client_metadata.x-codex-turn-metadata").String())
			require.EqualValues(test, number%2, meta.Get("window_number").Uint())
			require.Equal(test, sent.headers.Get("Thread-Id")+fmt.Sprintf(":%d", number%2), meta.Get("window_id").String())
			require.Equal(test, sent.headers.Get("Session-Id"), meta.Get("session_id").String())
			mappedTurn := meta.Get("turn_id").String()
			require.NotEqual(test, originalTurn, mappedTurn)
			require.Equal(test, mappedTurn, meta.Get("root_turn_id").String())
			require.Equal(test, mappedTurn, gjson.GetBytes(sent.body, "client_metadata.turn_id").String())
			require.False(test, gjson.Get(sent.headers.Get("X-Codex-Turn-Metadata"), "turn_id").Exists(), "pooled handshake must not retain a per-turn snapshot")
			mappedTurns = append(mappedTurns, mappedTurn)
			require.False(test, gjson.GetBytes(sent.body, "account_mapping").Exists())
			require.Equal(test, originalTurn, gjson.GetBytes(body, "client_metadata.turn_id").String())
		case <-time.After(time.Second):
			test.Fatal("upstream frame missing")
		}
	}
	require.Equal(test, captures[0].connection, captures[1].connection)
	require.Equal(test, captures[2].connection, captures[3].connection)
	require.NotEqual(test, captures[0].connection, captures[2].connection)
	if quota {
		require.EqualValues(test, 2, connections.Load())
		require.Equal(test, mappedTurns[0], mappedTurns[1])
		require.Equal(test, mappedTurns[2], mappedTurns[3])
		require.NotEqual(test, mappedTurns[0], mappedTurns[2])
		return
	}
	require.NotEqual(test, captures[0].connection, captures[4].connection)
	require.NotEqual(test, captures[0].headers.Get("Session-Id"), captures[4].headers.Get("Session-Id"))
	require.EqualValues(test, 3, connections.Load())
	require.Equal(test, mappedTurns[0], mappedTurns[1])
	require.Equal(test, mappedTurns[2], mappedTurns[3])
	require.NotEqual(test, mappedTurns[0], mappedTurns[2])
	require.NotEqual(test, mappedTurns[0], mappedTurns[4])
}

func TestSessionPreserveInputWebsocketIngress(t *testing.T) {
	for _, native := range []bool{false, true} {
		t.Run(fmt.Sprintf("native=%v", native), func(t *testing.T) { runWebsocketToolFailover(t, native, true) })
	}
}
