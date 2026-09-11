package wsrelay

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestWebsocketAnalyticsSnapshotAndProfile(test *testing.T) {
	headers := http.Header{}
	headers.Set("X-Codex-Turn-Metadata", `{"session_id":"session","analytics_enabled":false,"large":"`+strings.Repeat("x", 9000)+`"}`)
	prepareCodexHandshakeSnapshot(headers)
	require.Equal(test, gjson.False, gjson.Get(headers.Get("X-Codex-Turn-Metadata"), "analytics_enabled").Type)
	profile := websocketConnectionProfile(headers)
	headers.Set("X-Codex-Turn-Metadata", `{"session_id":"session","analytics_enabled":false,"window_number":3}`)
	require.Equal(test, profile, websocketConnectionProfile(headers))
	headers.Set("X-Codex-Turn-Metadata", `{"session_id":"session","analytics_enabled":true,"window_number":3}`)
	require.NotEqual(test, profile, websocketConnectionProfile(headers))
}

func TestWebsocketAnalyticsFinalTransmissionAndReuse(test *testing.T) {
	test.Setenv("CODEX_OUTBOUND_SESSION_MODE", "preserve")
	test.Setenv("CODEX_TELEMETRY_ENABLED", "true")
	previousRuntime, previousResin := proxy.CurrentRuntimeSettings(), proxy.GetResinConfig()
	test.Cleanup(func() {
		proxy.ApplyRuntimeSettings(previousRuntime)
		proxy.SetResinConfig(previousResin)
	})
	type capture struct {
		headers http.Header
		body    []byte
	}
	received := make(chan capture, 1)
	var connections atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := (&websocket.Upgrader{}).Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		connections.Add(1)
		defer connection.Close()
		for {
			_, body, err := connection.ReadMessage()
			if err != nil {
				return
			}
			received <- capture{request.Header.Clone(), body}
			if err := connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"status":"completed","output":[]}}`)); err != nil {
				return
			}
		}
	}))
	test.Cleanup(server.Close)
	proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: server.URL, PlatformName: "analytics-metadata"})
	manager := NewManager()
	test.Cleanup(manager.Stop)
	executor := NewExecutorWithManager(manager)
	account := &auth.Account{DBID: 4011, AccessToken: "test-token", CodexFingerprintMode: auth.CodexFingerprintModeDevice,
		CustomHeaders: map[string]string{"X-Codex-Turn-Metadata": `{"analytics_enabled":true,"session_id":"wrong"}`}}
	for _, enabled := range []bool{false, false, true, false} {
		proxy.UpdateRuntimeSettings(func(settings proxy.RuntimeSettings) proxy.RuntimeSettings {
			settings.CodexTelemetryEnabled = enabled
			return settings
		})
		ctx, cancel := context.WithTimeout(context.Background(), 5*time.Second)
		body := []byte(`{"model":"test","input":[],"client_metadata":{"x-codex-turn-metadata":{"session_id":"session","thread_id":"child","turn_id":"turn","request_kind":"turn"}}}`)
		upstream, err := executor.ExecuteRequestViaWebsocket(ctx, account, body, "pool-session", "", "key", nil, nil, "")
		require.NoError(test, err)
		response := websocketResponseToHTTP(ctx, upstream, http.StatusOK, nil)
		_, err = io.ReadAll(response.Body)
		require.NoError(test, err)
		require.NoError(test, response.Body.Close())
		cancel()
		sent := <-received
		metadata := gjson.Parse(gjson.GetBytes(sent.body, "client_metadata.x-codex-turn-metadata").String())
		require.Contains(test, []gjson.Type{gjson.False, gjson.True}, metadata.Get("analytics_enabled").Type)
		require.Equal(test, enabled, metadata.Get("analytics_enabled").Bool())
		require.Equal(test, metadata.Get("analytics_enabled").Raw, gjson.Get(sent.headers.Get("X-Codex-Turn-Metadata"), "analytics_enabled").Raw)
		require.Equal(test, "session", sent.headers.Get("Session-Id"))
		require.Equal(test, "session", metadata.Get("session_id").String())
		require.Equal(test, "child", sent.headers.Get("Thread-Id"))
	}
	require.EqualValues(test, 3, connections.Load())
}
