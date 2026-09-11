package wsrelay

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strconv"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestWebsocketTerminalClassificationAndConnectionLimits(test *testing.T) {
	for _, scenario := range []struct {
		name, frame string
		broken      bool
	}{
		{"incomplete", `{"type":"response.incomplete","response":{"id":"resp_incomplete","status":"incomplete"}}`, false},
		{"error_limit", `{"type":"error","error":{"code":"websocket_connection_limit_reached"}}`, true},
		{"flat_limit", `{"type":"error","code":"websocket_connection_limit_reached"}`, true},
		{"failed_limit", `{"type":"response.failed","response":{"error":{"code":"websocket_connection_limit_reached"}}}`, true},
		{"overload", `{"type":"response.failed","response":{"error":{"code":"server_is_overloaded"}}}`, false},
		{"rate_limit", `{"type":"error","status":429,"error":{"code":"rate_limit_exceeded"}}`, false},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			response := &WsResponse{}
			require.True(test, isReadLeaseTerminal([]byte(scenario.frame)))
			require.ErrorIs(test, response.handleMessage([]byte(scenario.frame), func([]byte) bool { return true }), io.EOF)
			require.Equal(test, scenario.broken, response.connBroken)
		})
	}
	require.False(test, isConnLimitErrorFrame([]byte(`{"type":"response.output_text.delta","delta":"websocket_connection_limit_reached","code":"websocket_connection_limit_reached"}`)))
}

func TestWebsocketTerminalRecoveryKeepsCurrentMetadataAndAccount(test *testing.T) {
	for _, scenario := range []struct {
		name, first  string
		connections  int32
		continuation bool
	}{
		{"incomplete", `{"type":"response.incomplete","response":{"id":"resp_first","status":"incomplete","output":[],"usage":{"output_tokens":4}}}`, 1, true},
		{"connection_limit", `{"type":"response.failed","response":{"error":{"code":"websocket_connection_limit_reached"}}}`, 2, false},
		{"overload", `{"type":"error","status":500,"error":{"code":"server_is_overloaded"}}`, 1, false},
		{"rate_limit", `{"type":"error","status":429,"error":{"code":"rate_limit_exceeded"}}`, 1, false},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			previous := proxy.GetResinConfig()
			test.Cleanup(func() { proxy.SetResinConfig(previous) })
			var openings, turns atomic.Int32
			received := make(chan []byte, 2)
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				connection, err := (&websocket.Upgrader{}).Upgrade(writer, request, nil)
				if err != nil {
					return
				}
				openings.Add(1)
				defer connection.Close()
				for {
					_, payload, err := connection.ReadMessage()
					if err != nil {
						return
					}
					received <- payload
					result := []byte(`{"type":"response.completed","response":{"id":"resp_second","status":"completed"}}`)
					if turns.Add(1) == 1 {
						result = []byte(scenario.first)
					}
					if connection.WriteMessage(websocket.TextMessage, result) != nil {
						return
					}
				}
			}))
			test.Cleanup(server.Close)
			proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: server.URL, PlatformName: "terminal-regression"})
			manager := NewManager()
			test.Cleanup(manager.Stop)
			executor := NewExecutorWithManager(manager)
			account := &auth.Account{DBID: 17, AccessToken: "dummy", AccountID: "fixture", CodexFingerprintMode: auth.CodexFingerprintModeDevice, DynamicConcurrencyLimit: 1}
			var first *WsConnection
			for turn := 0; turn < 2; turn++ {
				current := strconv.Itoa(turn)
				canonical := `{"session_id":"root","thread_id":"thread-current","window_id":"thread-current:` + current + `","turn_id":"turn-` + current + `","request_kind":"turn","thread_source":"user"}`
				payload := map[string]any{"model": "gpt-6-astra", "input": []any{}, "client_metadata": map[string]any{"x-codex-turn-metadata": canonical, "thread_id": "stale", "x-codex-window-id": "stale:0", "x-codex-turn-state": "state-" + current}}
				if turn == 1 && scenario.continuation {
					payload["previous_response_id"] = "resp_first"
				}
				body, err := json.Marshal(payload)
				require.NoError(test, err)
				ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
				defer cancel()
				response, err := executor.ExecuteRequestViaWebsocket(ctx, account, body, "same-session", "", "same-key", nil, http.Header{}, "")
				require.NoError(test, err)
				if first == nil {
					first = response.conn
				}
				require.Equal(test, int64(17), response.conn.session.AccountID)
				if turn == 1 && scenario.connections == 1 {
					require.Same(test, first, response.conn)
				}
				readDone := make(chan error, 1)
				var actualResponse []byte
				go func() {
					readDone <- response.ReadStream(func(frame []byte) bool { actualResponse = append([]byte(nil), frame...); return true })
				}()
				select {
				case err := <-readDone:
					require.NoError(test, err)
				case <-ctx.Done():
					response.Close()
					<-readDone
					test.Fatal("terminal did not complete read stream")
				}
				if turn == 0 && scenario.continuation {
					require.JSONEq(test, scenario.first, string(actualResponse))
					mode, result := manager.continuationRequirement("resp_first", 17, "same-key", response.requestScope)
					require.Equal(test, "connection_local", mode)
					require.Equal(test, "known", result)
				}
				require.NoError(test, response.Close())
				if turn == 0 && scenario.connections == 2 {
					require.False(test, first.IsConnected())
				}
				actual := <-received
				require.Equal(test, "thread-current", gjson.GetBytes(actual, "client_metadata.thread_id").String())
				require.Equal(test, "thread-current:"+current, gjson.GetBytes(actual, "client_metadata.x-codex-window-id").String())
				require.Equal(test, "state-"+current, gjson.GetBytes(actual, "client_metadata.x-codex-turn-state").String())
			}
			require.Equal(test, scenario.connections, openings.Load())
		})
	}
}
