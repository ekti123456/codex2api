package wsrelay

import (
	"bytes"
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestWebsocketOutboundSessionFailureReuseAndHTTP(test *testing.T) {
	test.Setenv("CODEX_OUTBOUND_SESSION_MODE", "aligned")
	test.Setenv("CODEX_SESSION_HEADER_MODE", "native")
	test.Setenv("CODEX_REQUEST_COMPRESSION", "off")
	previousResin, previousRuntime, previousExecutor := proxy.GetResinConfig(), proxy.CurrentRuntimeSettings(), proxy.WebsocketExecuteFunc
	test.Cleanup(func() {
		proxy.SetResinConfig(previousResin)
		proxy.ApplyRuntimeSettings(previousRuntime)
		proxy.WebsocketExecuteFunc = previousExecutor
	})
	proxy.ApplyRuntimeSettings(proxy.DefaultRuntimeSettings())
	type capture struct {
		headers http.Header
		body    []byte
	}
	received := make(chan capture, 1)
	var connections atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		if !websocket.IsWebSocketUpgrade(request) {
			body, _ := io.ReadAll(request.Body)
			received <- capture{request.Header.Clone(), body}
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"id":"response-http"}`))
			return
		}
		connection, err := (&websocket.Upgrader{}).Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		connections.Add(1)
		defer connection.Close()
		for attempt := 0; ; attempt++ {
			_, body, err := connection.ReadMessage()
			if err != nil {
				return
			}
			received <- capture{request.Header.Clone(), body}
			terminal := `{"type":"response.completed","response":{"id":"response-ws","status":"completed","output":[]}}`
			if attempt == 0 {
				terminal = `{"type":"response.failed","response":{"status":"failed","error":{"code":"server_is_overloaded","type":"service_unavailable_error","message":"overloaded"}}}`
			}
			if err := connection.WriteMessage(websocket.TextMessage, []byte(terminal)); err != nil {
				return
			}
		}
	}))
	test.Cleanup(server.Close)
	proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: server.URL, PlatformName: "outbound-session"})
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
	account := &auth.Account{DBID: 3701, AccountID: "fixture", AccessToken: "private-token", CodexFingerprintMode: auth.CodexFingerprintModeDevice, DynamicConcurrencyLimit: 1,
		CustomHeaders: map[string]string{"Session-Id": "wrong-custom-session", "Session_id": "wrong-legacy-session"}}
	headers := http.Header{"Session-Id": {"client-session"}, "Originator": {"codex-tui"}}
	originalHeaders := headers.Clone()
	upstream := proxy.IsolateCodexSessionID(1, "client-session")
	for attempt := 0; attempt < 3; attempt++ {
		body := []byte(fmt.Sprintf(`{"model":"gpt-6-astra","input":[],"client_metadata":{"session_id":"client-session","thread_id":"client-thread","x-codex-turn-metadata":{"session_id":"client-session","thread_id":"client-thread","window_id":"client-thread:%d","window_number":%d,"turn_id":"turn-%d","request_kind":"turn","thread_source":"user"}}}`, attempt, attempt, attempt))
		originalBody := bytes.Clone(body)
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		defer cancel()
		response, err := proxy.ExecuteRequest(ctx, account, body, upstream, "", "same-key", nil, headers, attempt < 2)
		require.NoError(test, err)
		output, err := io.ReadAll(response.Body)
		require.NoError(test, err)
		require.NoError(test, response.Body.Close())
		if attempt == 0 {
			require.Contains(test, string(output), "server_is_overloaded")
		}
		var sent capture
		select {
		case sent = <-received:
		case <-ctx.Done():
			test.Fatal("upstream did not receive request")
		}
		require.Equal(test, "client-session", sent.headers.Get("Session-Id"))
		require.Equal(test, "client-thread", sent.headers.Get("Thread-Id"))
		require.Equal(test, "client-thread", sent.headers.Get("X-Client-Request-Id"))
		require.Empty(test, sent.headers.Get("Session_id"))
		require.Equal(test, "client-session", gjson.GetBytes(sent.body, "client_metadata.session_id").String())
		metadata := gjson.GetBytes(sent.body, "client_metadata.x-codex-turn-metadata").String()
		require.Equal(test, "client-session", gjson.Get(metadata, "session_id").String())
		if attempt < 2 {
			require.Equal(test, "client-thread:0", sent.headers.Get("X-Codex-Window-Id"))
			require.Equal(test, "client-session", gjson.Get(sent.headers.Get("X-Codex-Turn-Metadata"), "session_id").String())
		}
		require.Equal(test, "client-thread", gjson.Get(metadata, "thread_id").String())
		require.Equal(test, int64(attempt), gjson.Get(metadata, "window_number").Int())
		require.Equal(test, upstream, gjson.GetBytes(sent.body, "prompt_cache_key").String())
		require.Equal(test, originalBody, body)
		require.Equal(test, originalHeaders, headers)
	}
	require.EqualValues(test, 1, connections.Load())
}
