package wsrelay

import (
	"context"
	"fmt"
	"io"
	"net/http"
	"net/http/httptest"
	"path/filepath"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/proxy"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestOutboundPrivacyFinalWireConsistencyAndReuse(t *testing.T) {
	t.Setenv("CODEX_OUTBOUND_SESSION_MODE", "account")
	t.Setenv("CODEX_REQUEST_COMPRESSION", "off")
	t.Setenv("CODEX_TELEMETRY_ENABLED", "false")
	previousResin, previousRuntime, previousExecutor := proxy.GetResinConfig(), proxy.CurrentRuntimeSettings(), proxy.WebsocketExecuteFunc
	t.Cleanup(func() {
		proxy.SetResinConfig(previousResin)
		proxy.ApplyRuntimeSettings(previousRuntime)
		proxy.WebsocketExecuteFunc = previousExecutor
	})
	proxy.ApplyRuntimeSettings(proxy.DefaultRuntimeSettings())
	type capture struct {
		headers http.Header
		body    []byte
	}
	captures := make(chan capture, 8)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if !websocket.IsWebSocketUpgrade(r) {
			body, _ := io.ReadAll(r.Body)
			captures <- capture{r.Header.Clone(), body}
			w.Header().Set("Content-Type", "application/json")
			_, _ = w.Write([]byte(`{"id":"wire-test","output":[]}`))
			return
		}
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		for {
			_, body, err := conn.ReadMessage()
			if err != nil {
				return
			}
			captures <- capture{r.Header.Clone(), body}
			event := []byte(`{"type":"response.completed","response":{"id":"wire-test","status":"completed","output":[]}}`)
			if stream := gjson.GetBytes(body, "stream_id"); stream.Exists() {
				event, _ = sjson.SetBytes(event, "stream_id", stream.String())
			}
			if conn.WriteMessage(websocket.TextMessage, event) != nil {
				return
			}
		}
	}))
	t.Cleanup(server.Close)
	proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: server.URL, PlatformName: "outbound-privacy"})
	manager := NewManager()
	t.Cleanup(manager.Stop)
	executor := NewExecutorWithManager(manager)
	var connections []*WsConnection
	proxy.WebsocketExecuteFunc = func(ctx context.Context, account *auth.Account, body []byte, session, proxyURL, key string, config *proxy.DeviceProfileConfig, headers http.Header, pool string) (*http.Response, error) {
		res, err := executor.ExecuteRequestViaWebsocket(ctx, account, body, session, proxyURL, key, config, headers, pool)
		if err != nil {
			return nil, err
		}
		connections = append(connections, res.conn)
		return websocketResponseToHTTP(ctx, res, http.StatusOK, nil), nil
	}
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "wire.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(proxy.WithCodexIdentityStore(context.Background(), db), 15*time.Second)
	defer cancel()
	account := &auth.Account{DBID: 1695, AccountID: "661373c1-f1a9-4ca9-8682-a0594b30c36c", AccessToken: "account-token", CodexInstallationID: "account-installation", CodexFingerprintMode: auth.CodexFingerprintModeOff, CustomHeaders: map[string]string{"Session-Id": "wrong-custom-session", "Thread-Id": "wrong-custom-thread", "X-Codex-Installation-Id": "account-installation", "X-Oai-Attestation": "account-attestation"}}
	const root = "01a09302-49f4-7b53-b545-91ef29610317"
	const turn = "01a095b5-86a3-7ec2-af42-0bb1111ef330"
	var firstSession, firstRequest, firstTurn string
	var firstStream string
	for i, transport := range []string{"http", "ws", "ws", "compact"} {
		originalTurn := turn
		if i == 2 {
			originalTurn = "01a095b6-86a3-7ec2-af42-0bb1111ef331"
		}
		headers := http.Header{"Session-Id": {root}, "Thread-Id": {root}, "X-Client-Request-Id": {"private-request"}, "X-Oai-Attestation": {"private-attestation"}, "User-Agent": {"codex_cli_rs/0.155.0 (private-platform)"}, "Originator": {"codex_cli_rs"}}
		body := []byte(fmt.Sprintf(`{"model":"gpt-6-astra","input":[{"role":"user","content":"keep C:/task/path"}],"extra_body":{"x-codex-turn-state":"private-extra-state"},"client_metadata":{"session_id":"stale-flat","x-client-request-id":"private-request","x-codex-turn-metadata":{"session_id":"%s","thread_id":"%s","turn_id":"%s","root_turn_id":"%s","parent_turn_id":"%s","window_id":"%s:%d","window_number":%d,"installation_id":"private-installation","client_request_id":"private-request","nested":{"session_id":"private-nested"},"encoded":"{\"request_id\":\"private-encoded\"}"}}}`, root, root, originalTurn, turn, turn, root, i, i))
		var res *http.Response
		body, _ = sjson.SetRawBytes(body, "access_programs", []byte(`{"cyber":"daybreak_blue"}`))
		body, _ = sjson.SetRawBytes(body, "client_metadata.x-codex-turn-metadata.tool_namespaces_info", []byte(`{"functions":{"name":"functions","functions":{"exec":{"name":"exec","direct":true,"code_mode_name":null,"deferred":false,"source":{"kind":"harness"}}}}}`))
		body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.history_ingest_requested", true)
		body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.auto_review_enabled", false)
		body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.agent_name", "/root/private-agent")
		if transport == "ws" {
			body, _ = sjson.SetBytes(body, "generate", false)
			body, _ = sjson.SetBytes(body, "stream_id", "private-client-stream")
		}
		if transport == "compact" {
			res, err = proxy.ExecuteCompactRequest(ctx, account, body, "cache", "", "test-key", nil, headers)
		} else {
			res, err = proxy.ExecuteRequest(ctx, account, body, "cache", "", "test-key", nil, headers, transport == "ws")
		}
		require.NoError(t, err)
		received, readErr := io.ReadAll(res.Body)
		err = readErr
		require.NoError(t, err)
		require.NoError(t, res.Body.Close())
		var sent capture
		select {
		case sent = <-captures:
		case <-ctx.Done():
			t.Fatal("missing wire capture")
		}
		if transport == "ws" {
			require.Equal(t, gjson.False, gjson.GetBytes(sent.body, "generate").Type, "prewarm must not become generation")
			stream := gjson.GetBytes(sent.body, "stream_id").String()
			require.NotEmpty(t, stream)
			require.NotEqual(t, "private-client-stream", stream)
			if firstStream == "" {
				firstStream = stream
			} else {
				require.Equal(t, firstStream, stream, "a new turn on the reused connection must keep the lane mapping")
			}
			require.NotContains(t, string(received), stream)
			require.Contains(t, string(received), "private-client-stream")
		}
		require.NotContains(t, string(sent.body), "private-agent")
		require.NotContains(t, sent.headers.Get("X-Codex-Turn-Metadata"), "tool_namespaces_info")
		if transport != "compact" {
			require.Equal(t, "daybreak_blue", gjson.GetBytes(sent.body, "access_programs.cyber").String())
			canonical := gjson.GetBytes(sent.body, "client_metadata.x-codex-turn-metadata")
			require.Equal(t, gjson.String, canonical.Type)
			canonical = gjson.Parse(canonical.String())
			require.Equal(t, "exec", canonical.Get("tool_namespaces_info.functions.functions.exec.name").String())
			require.True(t, canonical.Get("history_ingest_requested").Bool())
			require.Equal(t, gjson.False, canonical.Get("auto_review_enabled").Type)
		} else {
			require.False(t, gjson.GetBytes(sent.body, "access_programs").Exists())
		}
		require.Equal(t, "Bearer account-token", sent.headers.Get("Authorization"))
		require.Equal(t, "account-attestation", sent.headers.Get("X-Oai-Attestation"))
		for _, forbidden := range []string{"private-platform", "private-attestation", "private-installation", "private-nested", "private-encoded", "private-extra-state", "private-request", "stale-flat", "wrong-custom"} {
			require.NotContains(t, string(sent.body), forbidden)
			require.NotContains(t, fmt.Sprint(sent.headers), forbidden)
		}
		headerMeta := gjson.Parse(sent.headers.Get("X-Codex-Turn-Metadata"))
		meta := gjson.GetBytes(sent.body, "client_metadata.x-codex-turn-metadata")
		if meta.Type == gjson.String {
			meta = gjson.Parse(meta.String())
		}
		if transport == "compact" {
			require.False(t, gjson.GetBytes(sent.body, "client_metadata").Exists())
			meta = headerMeta
		}
		session, request, mappedTurn := meta.Get("session_id").String(), meta.Get("client_request_id").String(), meta.Get("turn_id").String()
		require.NotEqual(t, root, session)
		require.NotEmpty(t, request)
		require.NotEqual(t, originalTurn, mappedTurn)
		require.Equal(t, "account-installation", meta.Get("installation_id").String())
		require.Equal(t, session, sent.headers.Get("Session-Id"))
		require.Equal(t, session, sent.headers.Get("Thread-Id"))
		if i == 0 {
			firstSession, firstRequest, firstTurn = session, request, mappedTurn
		}
		require.Equal(t, firstSession, session)
		require.Equal(t, firstRequest, request)
		require.Equal(t, firstTurn, meta.Get("parent_turn_id").String())
		require.Equal(t, firstTurn, meta.Get("root_turn_id").String())
		if i == 2 {
			require.NotEqual(t, firstTurn, mappedTurn)
		} else {
			require.Equal(t, firstTurn, mappedTurn)
		}
		if transport == "ws" {
			require.Empty(t, sent.headers.Get("X-Codex-Window-Id"))
			require.Empty(t, sent.headers.Get("X-Client-Request-Id"))
			for _, field := range []string{"turn_id", "root_turn_id", "parent_turn_id", "window_id", "window_number", "client_request_id"} {
				require.False(t, headerMeta.Get(field).Exists(), field)
			}
		} else {
			require.Equal(t, request, sent.headers.Get("X-Client-Request-Id"))
			require.Equal(t, meta.Get("window_id").String(), sent.headers.Get("X-Codex-Window-Id"))
		}
		if transport != "compact" {
			for _, field := range []string{"session_id", "thread_id", "turn_id", "root_turn_id", "parent_turn_id", "window_id", "window_number", "installation_id", "client_request_id"} {
				if flat := gjson.GetBytes(sent.body, "client_metadata."+field); flat.Exists() {
					require.Equal(t, meta.Get(field).Raw, flat.Raw, field)
				}
			}
			require.Equal(t, request, gjson.GetBytes(sent.body, "client_metadata.x-client-request-id").String())
			require.Equal(t, meta.Get("window_id").String(), gjson.GetBytes(sent.body, "client_metadata.x-codex-window-id").String())
		}
		require.Equal(t, "keep C:/task/path", gjson.GetBytes(sent.body, "input.0.content").String())
	}
	require.Len(t, connections, 2)
	require.Same(t, connections[0], connections[1])
}
