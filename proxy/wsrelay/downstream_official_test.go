package wsrelay

import (
	"context"
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

func TestDownstreamStreamBusinessBoundary(t *testing.T) {
	for _, item := range []string{
		`{"id":"prog_out","type":"program_output","call_id":"call_program","status":"completed","result":"{\"stream_id\":\"business_record\",\"count\":1}"}`,
		`{"id":"ci_audit","type":"code_interpreter_call","code":"print('ok')","outputs":[{"type":"logs","logs":"{\"stream_id\":\"business_record\"}"}]}`,
		`{"id":"fs_audit","type":"file_search_call","status":"completed","queries":["x"],"results":[{"file_id":"file_audit","text":"business","attributes":{"stream_id":"business_record"}}]}`,
	} {
		payload := []byte(`{"type":"response.output_item.done","stream_id":"out_lane","item":` + item + `}`)
		response := &WsResponse{upstreamStreamID: "out_lane", clientStreamID: "client_lane"}
		delivered := false
		err := response.handleMessage(payload, func([]byte) bool { delivered = true; return true })
		require.NoError(t, err)
		require.True(t, delivered)
		require.False(t, proxy.TransportReplayBlocked(err))
	}
}

func TestDownstreamActualWSBusinessStreamField(t *testing.T) {
	for _, named := range []bool{true, false} {
		name := "default"
		if named {
			name = "named"
		}
		t.Run(name, func(t *testing.T) { downstreamActualWSBusinessStreamField(t, named, false) })
		t.Run(name+"_error", func(t *testing.T) { downstreamActualWSBusinessStreamField(t, named, true) })
	}
}

func downstreamActualWSBusinessStreamField(t *testing.T, named, failure bool) {
	t.Setenv("CODEX_OUTBOUND_SESSION_MODE", "account")
	t.Setenv("CODEX_TELEMETRY_ENABLED", "false")
	old := proxy.GetResinConfig()
	t.Cleanup(func() { proxy.SetResinConfig(old) })
	wire := make(chan []byte, 1)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		conn, err := (&websocket.Upgrader{}).Upgrade(w, r, nil)
		if err != nil {
			return
		}
		defer conn.Close()
		_, body, err := conn.ReadMessage()
		if err != nil {
			return
		}
		wire <- body
		if failure {
			event := []byte(`{"type":"error","status":400,"sequence_number":19,"error":{"code":"invalid_parameter","type":"invalid_request_error","message":"invalid input","param":"input[0].type"}}`)
			if named {
				event, _ = sjson.SetBytes(event, "stream_id", gjson.GetBytes(body, "stream_id").String())
			}
			_ = conn.WriteMessage(websocket.TextMessage, event)
			_, _, _ = conn.ReadMessage()
			return
		}
		event := []byte(`{"type":"response.output_item.done","item":{"id":"ci_audit","type":"code_interpreter_call","code":"print('ok')","outputs":[{"type":"logs","logs":"{\"stream_id\":\"business_record\"}"}]}}`)
		if gjson.GetBytes(body, "stream_id").Exists() {
			event, _ = sjson.SetBytes(event, "stream_id", gjson.GetBytes(body, "stream_id").String())
		}
		_ = conn.WriteMessage(websocket.TextMessage, event)
		done := []byte(`{"type":"response.completed","response":{"id":"resp_audit","status":"completed","output":[]}}`)
		if gjson.GetBytes(body, "stream_id").Exists() {
			done, _ = sjson.SetBytes(done, "stream_id", gjson.GetBytes(body, "stream_id").String())
		}
		_ = conn.WriteMessage(websocket.TextMessage, done)
		_, _, _ = conn.ReadMessage()
	}))
	t.Cleanup(server.Close)
	proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: server.URL, PlatformName: "ws-business-audit"})
	db, err := database.New("sqlite", filepath.Join(t.TempDir(), "audit.db"))
	require.NoError(t, err)
	t.Cleanup(func() { _ = db.Close() })
	ctx, cancel := context.WithTimeout(proxy.WithCodexIdentityStore(context.Background(), db), 10*time.Second)
	defer cancel()
	manager := NewManager()
	t.Cleanup(manager.Stop)
	executor := NewExecutorWithManager(manager)
	account := &auth.Account{DBID: 1695, AccountID: "661373c1-f1a9-4ca9-8682-a0594b30c36c", AccessToken: "audit-account-token"}
	root := proxy.NewUpstreamSessionUUID()
	body := []byte(`{"model":"gpt-6-astra","input":[]}`)
	if named {
		body, _ = sjson.SetBytes(body, "stream_id", "client_lane")
	}
	response, err := executor.ExecuteRequestViaWebsocket(ctx, account, body, root, "", "audit-key", nil, http.Header{"Session-Id": {root}, "Thread-Id": {root}}, "")
	require.NoError(t, err)
	defer response.Close()
	delivered := 0
	var last []byte
	err = response.ReadStream(func(data []byte) bool {
		delivered++
		last = data
		if named {
			require.Equal(t, "client_lane", gjson.GetBytes(data, "stream_id").String())
		}
		if gjson.GetBytes(data, "type").String() == "response.output_item.done" {
			require.Equal(t, `{"stream_id":"business_record"}`, gjson.GetBytes(data, "item.outputs.0.logs").String())
		}
		return true
	})
	require.NoError(t, err)
	if failure {
		require.Equal(t, 1, delivered)
		require.Equal(t, "response.failed", gjson.GetBytes(last, "type").String())
		require.EqualValues(t, 400, gjson.GetBytes(last, "status").Int())
		require.EqualValues(t, 19, gjson.GetBytes(last, "sequence_number").Int())
		require.Equal(t, "input[0].type", gjson.GetBytes(last, "response.error.param").String())
	} else {
		require.Equal(t, 2, delivered)
	}
	require.False(t, response.connBroken)
	sent := <-wire
	if named {
		require.NotEqual(t, "client_lane", gjson.GetBytes(sent, "stream_id").String())
	}
}
