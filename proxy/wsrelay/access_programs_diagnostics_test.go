package wsrelay

import (
	"context"
	"fmt"
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
)

func TestAccessProgramsNativeWebsocketDiagnostics(t *testing.T) {
	oldRuntime, oldResin, oldExecutor := proxy.CurrentRuntimeSettings(), proxy.GetResinConfig(), proxy.WebsocketExecuteFunc
	t.Cleanup(func() {
		proxy.ApplyRuntimeSettings(oldRuntime)
		proxy.SetResinConfig(oldResin)
		proxy.WebsocketExecuteFunc = oldExecutor
	})
	settings := proxy.DefaultRuntimeSettings()
	settings.CodexForceWebsocket = true
	proxy.ApplyRuntimeSettings(settings)
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "access-programs.db"))
	require.NoError(t, err)
	t.Cleanup(func() { require.NoError(t, db.Close()) })
	_, err = db.InsertAPIKeyWithOptions(t.Context(), database.APIKeyInput{Key: "test-user-key"})
	require.NoError(t, err)
	memory := cache.NewMemory(100)
	t.Cleanup(func() { require.NoError(t, memory.Close()) })
	store := auth.NewStore(nil, memory, nil)
	t.Cleanup(store.Stop)
	store.AddAccount(&auth.Account{DBID: 1695, AccountID: "661373c1-f1a9-4ca9-8682-a0594b30c36c", AccessToken: "test-token", Status: auth.StatusReady, Models: []string{"gpt-5.6-sol"}})
	handler := proxy.NewHandler(store, db, nil, nil)
	handler.SetRuntimeCache(memory)
	seen := make(chan []byte, 4)
	upstream := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, frame, err := conn.ReadMessage()
			if err != nil {
				return
			}
			seen <- frame
			if err := conn.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"id":"access-response","status":"completed","output":[],"usage":{"input_tokens":1,"output_tokens":1}}}`)); err != nil {
				return
			}
		}
	}))
	t.Cleanup(upstream.Close)
	proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: upstream.URL, PlatformName: "access-programs-test"})
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
	router := gin.New()
	router.GET("/v1/responses", handler.APIKeyAuthMiddleware(), handler.ResponsesWebSocket)
	frontend := httptest.NewServer(router)
	t.Cleanup(frontend.Close)
	conn, _, err := websocket.DefaultDialer.Dial("ws"+strings.TrimPrefix(frontend.URL, "http")+"/v1/responses", http.Header{"Authorization": {"Bearer test-user-key"}})
	require.NoError(t, err)
	defer conn.Close()
	root, turn := uuid.Must(uuid.NewV7()).String(), uuid.Must(uuid.NewV7()).String()
	for step, field := range []string{`,"access_programs":{"cyber":"daybreak_blue"}`, `,"access_programs":null`, "", `,"access_programs":{"cyber":"standard"}`} {
		body := []byte(fmt.Sprintf(`{"type":"response.create","model":"gpt-5.6-sol","input":"hello"%s,"client_metadata":{"session_id":%q,"thread_id":%q,"x-codex-turn-metadata":{"session_id":%q,"thread_id":%q,"turn_id":%q,"thread_source":"user","request_kind":"turn","window_number":0}}}`, field, root, root, root, root, turn))
		require.NoError(t, conn.SetReadDeadline(time.Now().Add(5*time.Second)))
		require.NoError(t, conn.WriteMessage(websocket.TextMessage, body))
		for {
			_, event, readErr := conn.ReadMessage()
			require.NoError(t, readErr)
			kind := gjson.GetBytes(event, "type").String()
			require.NotEqual(t, "error", kind, string(event))
			require.NotEqual(t, "response.failed", kind, string(event))
			if kind == "response.completed" {
				break
			}
		}
		var sent []byte
		select {
		case sent = <-seen:
		case <-time.After(time.Second):
			t.Fatal("upstream frame missing")
		}
		var diagnostic []byte
		require.Eventually(t, func() bool {
			db.FlushUsageLogs()
			logs, err := db.ListRecentUsageLogs(t.Context(), 10)
			if err != nil || len(logs) != step+1 {
				return false
			}
			detail, err := db.GetUsageRequestDiagnostics(t.Context(), logs[0].ID)
			if err != nil {
				return false
			}
			diagnostic = detail.Diagnostics
			return len(diagnostic) > 0
		}, 2*time.Second, 10*time.Millisecond)
		for direction, wire := range map[string][]byte{"inbound": body, "outbound": sent} {
			value := gjson.GetBytes(wire, "access_programs")
			snapshot := gjson.GetBytes(diagnostic, "access_programs."+direction)
			state := "absent"
			if value.Exists() {
				state = "present"
			}
			require.Equal(t, state, snapshot.Get("state").String(), "step=%d direction=%s diagnostic=%s", step, direction, diagnostic)
			require.Equal(t, value.Raw, snapshot.Get("value").Raw)
		}
		require.Equal(t, "websocket", gjson.GetBytes(diagnostic, "upstream.transport").String())
	}
}
