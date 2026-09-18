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
	appconfig "github.com/codex2api/config"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestWebsocketContinuityOffRestartsIdentityAndConnection(t *testing.T) {
	oldRuntime, oldResin, oldExecutor := proxy.CurrentRuntimeSettings(), proxy.GetResinConfig(), proxy.WebsocketExecuteFunc
	t.Cleanup(func() {
		proxy.ApplyRuntimeSettings(oldRuntime)
		proxy.SetResinConfig(oldResin)
		proxy.WebsocketExecuteFunc = oldExecutor
	})
	t.Setenv("CODEX_REQUEST_COMPRESSION", "off")
	t.Setenv("CODEX_OUTBOUND_SESSION_MODE", "legacy")
	settings := proxy.DefaultRuntimeSettings()
	settings.CodexForceWebsocket = true
	settings.CodexSessionFailoverEnabled = false
	proxy.ApplyRuntimeSettings(settings)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "restart.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	memory := cache.NewMemory(100)
	t.Cleanup(func() { require.NoError(t, memory.Close()) })
	store := auth.NewStore(nil, memory, nil)
	t.Cleanup(store.Stop)
	account := &auth.Account{DBID: 1695, AccountID: "661373c1-f1a9-4ca9-8682-a0594b30c36c", AccessToken: "first-token", Status: auth.StatusReady, Models: []string{"gpt-5.6-sol"}, CodexFingerprintMode: auth.CodexFingerprintModeDevice}
	store.AddAccount(account)
	config := store.GetPromptFilterConfig()
	config.Advanced.Risk.SessionContinuityMode = "off"
	store.SetPromptFilterConfig(config)
	handler := proxy.NewHandler(store, db, &appconfig.Config{AllowAnonymousV1: true}, nil)
	handler.SetRuntimeCache(memory)
	router := gin.New()
	router.POST("/v1/responses", handler.APIKeyAuthMiddleware(), handler.Responses)
	type capture struct {
		headers    http.Header
		body       []byte
		connection int32
	}
	seen := make(chan capture, 1)
	var connections atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		connection, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
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
			seen <- capture{r.Header.Clone(), body, index}
			if err := connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"restart-response","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`)); err != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: server.URL, PlatformName: "continuity-ws"})
	manager := NewManager()
	t.Cleanup(manager.Stop)
	executor := NewExecutorWithManager(manager)
	proxy.WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, body []byte, session, proxyURL, key string, config *proxy.DeviceProfileConfig, headers http.Header, pool string) (*http.Response, error) {
		response, err := executor.ExecuteRequestViaWebsocket(ctx, account, body, session, proxyURL, key, config, headers, pool)
		if err != nil {
			return nil, err
		}
		return websocketResponseToHTTP(ctx, response, http.StatusOK, nil), nil
	}
	root := proxy.NewUpstreamSessionUUID()
	var previous capture
	previousResponse := ""
	for i, step := range []struct{ incoming, outgoing int }{{47, 0}, {47, 0}, {48, 1}, {55, 0}, {56, 1}} {
		body := []byte(fmt.Sprintf(`{"model":"gpt-5.6-sol","stream":true,"input":"full plaintext context","client_metadata":{"session_id":"%s","thread_id":"%s","x-codex-turn-metadata":{"session_id":"%s","thread_id":"%s","thread_source":"user","request_kind":"turn","window_id":"%s:%d","window_number":%d}}}`, root, root, root, root, root, step.incoming, step.incoming))
		// The first request starts a fresh session. Later requests carry an alias
		// actually issued to this owner, so admission reaches restart cleanup.
		if previousResponse != "" {
			body, err = sjson.SetBytes(body, "previous_response_id", previousResponse)
			require.NoError(t, err)
		}
		body = addSessionWireTools(t, body)
		recorder := httptest.NewRecorder()
		request := httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
		request.Header.Set("Authorization", "Bearer test-user-key")
		request.Header.Set("X-Codex-Turn-State", "old-state")
		ctx, cancel := context.WithTimeout(request.Context(), 5*time.Second)
		request = request.WithContext(ctx)
		router.ServeHTTP(recorder, request)
		cancel()
		require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
		previousResponse = ""
		for _, line := range strings.Split(recorder.Body.String(), "\n") {
			if payload, ok := strings.CutPrefix(line, "data: "); ok && gjson.Get(payload, "type").String() == "response.completed" {
				previousResponse = gjson.Get(payload, "response.id").String()
			}
		}
		require.True(t, db.IsManagedCodexResponseID(previousResponse), recorder.Body.String())
		db.FlushUsageLogs()
		logs, err := db.ListRecentUsageLogs(t.Context(), 1)
		require.NoError(t, err)
		require.Len(t, logs, 1)
		require.Equal(t, fmt.Sprint(step.incoming), logs[0].WindowNumberOriginal)
		detail, err := db.GetUsageRequestDiagnostics(t.Context(), logs[0].ID)
		require.NoError(t, err)
		require.Equal(t, fmt.Sprint(step.outgoing), logs[0].WindowNumberOutbound, string(detail.Diagnostics))
		require.Contains(t, string(detail.Diagnostics), `"tool_preservation":"preserved"`)
		require.Contains(t, string(detail.Diagnostics), `"additional_items":1`)
		select {
		case sent := <-seen:
			assertSessionWireTools(t, sent.body)
			meta := gjson.Parse(gjson.GetBytes(sent.body, "client_metadata.x-codex-turn-metadata").String())
			require.EqualValues(t, step.outgoing, meta.Get("window_number").Uint())
			require.NotEqual(t, root, sent.headers.Get("Session-Id"))
			require.Equal(t, sent.headers.Get("Session-Id"), meta.Get("session_id").String())
			require.Equal(t, fmt.Sprintf("%s:%d", sent.headers.Get("Thread-Id"), step.outgoing), meta.Get("window_id").String())
			require.False(t, gjson.GetBytes(sent.body, "previous_response_id").Exists())
			require.Empty(t, sent.headers.Get("X-Codex-Turn-State"))
			require.Equal(t, account.AccountID, sent.headers.Get("Chatgpt-Account-Id"))
			if i == 3 {
				require.NotEqual(t, previous.connection, sent.connection)
				require.NotEqual(t, previous.headers.Get("Session-Id"), sent.headers.Get("Session-Id"))
			} else if i > 0 {
				require.Equal(t, previous.connection, sent.connection)
				require.Equal(t, previous.headers.Get("Session-Id"), sent.headers.Get("Session-Id"))
			}
			previous = sent
		case <-time.After(time.Second):
			t.Fatal("upstream frame missing")
		}
	}
	require.EqualValues(t, 2, connections.Load())
}
