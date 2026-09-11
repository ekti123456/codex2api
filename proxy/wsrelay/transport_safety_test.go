package wsrelay

import (
	"context"
	"fmt"
	"io"
	"net"
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

type deliveryFailureConnection struct {
	net.Conn
	fail *atomic.Bool
}

func (connection *deliveryFailureConnection) Write(payload []byte) (int, error) {
	written, err := connection.Conn.Write(payload)
	if err == nil && connection.fail.CompareAndSwap(true, false) {
		return written, io.ErrUnexpectedEOF
	}
	return written, err
}

func TestAmbiguousWebsocketWriteIsNeverReplayed(test *testing.T) {
	var openings, frames atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := (&websocket.Upgrader{}).Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		openings.Add(1)
		defer connection.Close()
		for {
			if _, _, err := connection.ReadMessage(); err != nil {
				return
			}
			frames.Add(1)
		}
	}))
	test.Cleanup(server.Close)
	previous := proxy.GetResinConfig()
	proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: server.URL, PlatformName: "delivery-test"})
	test.Cleanup(func() { proxy.SetResinConfig(previous) })
	manager := NewManager()
	test.Cleanup(manager.Stop)
	var fail atomic.Bool
	manager.dialer.NetDialContext = func(ctx context.Context, network, address string) (net.Conn, error) {
		connection, err := (&net.Dialer{}).DialContext(ctx, network, address)
		if err != nil {
			return nil, err
		}
		return &deliveryFailureConnection{Conn: connection, fail: &fail}, nil
	}
	manager.afterConnectionStored = func(*WsConnection) { fail.Store(true) }
	executor := NewExecutorWithManager(manager)
	account := &auth.Account{DBID: 17, AccessToken: "fixture-token", DynamicConcurrencyLimit: 2}
	ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
	defer cancel()
	_, err := executor.ExecuteRequestViaWebsocket(ctx, account, []byte(`{"model":"test","input":"hello"}`), "session", "", "key", nil, nil, "")
	require.Error(test, err)
	require.True(test, proxy.TransportReplayBlocked(err))
	require.False(test, shouldRetryWebsocketSendError(err))
	require.Equal(test, int32(1), openings.Load())
	require.Eventually(test, func() bool { return frames.Load() == 1 }, time.Second, time.Millisecond)
	require.Zero(test, manager.ConnectionCount())
	closed := NewWsConnection(nil, NewSession(17, manager), "ws://fixture")
	closed.Close()
	err = executor.sendRequest(closed, []byte(`{}`), "request")
	require.Error(test, err)
	require.False(test, proxy.TransportReplayBlocked(err))
	require.True(test, shouldRetryWebsocketSendError(err))
}

func TestWebsocketExecutorSeparatesDownstreamConnectionsAndKeys(test *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		connection, err := (&websocket.Upgrader{}).Upgrade(writer, request, nil)
		if err != nil {
			return
		}
		defer connection.Close()
		for {
			if _, _, err := connection.ReadMessage(); err != nil {
				return
			}
			if connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.completed","response":{"status":"completed"}}`)) != nil {
				return
			}
		}
	}))
	test.Cleanup(server.Close)
	previous := proxy.GetResinConfig()
	proxy.SetResinConfig(&proxy.ResinConfig{BaseURL: server.URL, PlatformName: "isolation-test"})
	test.Cleanup(func() { proxy.SetResinConfig(previous) })
	manager := NewManager()
	test.Cleanup(manager.Stop)
	manager.probeFunc = func(*WsConnection) bool { return true }
	executor := NewExecutorWithManager(manager)
	account := &auth.Account{DBID: 17, AccessToken: "fixture-token", DynamicConcurrencyLimit: 16}
	firstCtx := proxy.WithDownstreamWebsocketConnection(context.Background())
	otherCtx := proxy.WithDownstreamWebsocketConnection(context.Background())
	request := func(parent context.Context, key string, stateless bool) *WsConnection {
		ctx, cancel := context.WithTimeout(parent, 3*time.Second)
		defer cancel()
		session, pool := "shared-session", ""
		if stateless {
			session, pool = "stateless-"+proxy.NewUpstreamSessionUUID(), "shared-pool"
		}
		response, err := executor.ExecuteRequestViaWebsocket(ctx, account, []byte(`{"model":"test","input":"hello"}`), session, "", key, nil, nil, pool)
		require.NoError(test, err)
		require.NoError(test, response.ReadStream(func([]byte) bool { return true }))
		require.NoError(test, response.Close())
		return response.conn
	}
	for _, stateless := range []bool{false, true} {
		first := request(firstCtx, "key-a", stateless)
		other := request(otherCtx, "key-a", stateless)
		require.NotSame(test, first, other)
		require.NotEqual(test, first.PoolKey, other.PoolKey)
		differentKey := request(firstCtx, "key-b", stateless)
		require.NotSame(test, first, differentKey)
		again := request(firstCtx, "key-a", stateless)
		if !stateless {
			require.Same(test, first, again)
		}
		require.NotContains(test, again.PoolKey, "key-a")
	}
}

func TestContinuationLossSurvivesBindingCleanupWithOwnerChecks(test *testing.T) {
	manager := NewManager()
	test.Cleanup(manager.Stop)
	connection := addConnectedConn(test, manager, 17, "lane")
	connection.diagnosticID = "lost-connection"
	connection.onClosed = manager.recordConnectionLoss
	manager.BindResponseConn("response-1", connection, "lane", 17, "key", "scope")
	connection.noteExit("heartbeat_pong_timeout", nil)
	manager.DiscardConnection(connection)
	require.Empty(test, manager.respConnBindings)
	mode, reason := manager.continuationRequirement("response-1", 17, "key", "scope")
	require.Equal(test, "connection_local", mode)
	require.Equal(test, "lost:heartbeat_pong_timeout", reason)
	for _, owner := range []struct {
		account    int64
		key, scope string
	}{{18, "key", "scope"}, {17, "other", "scope"}, {17, "key", "other"}} {
		_, reason = manager.continuationRequirement("response-1", owner.account, owner.key, owner.scope)
		require.Equal(test, "owner_mismatch", reason)
	}
	account := &auth.Account{DBID: 17}
	_, _, _, local, err := acquireContinuation(manager, nil, "response-1", account, "key", "scope", "ws://fixture", nil, "")
	require.Error(test, err)
	require.True(test, local)
	require.False(test, proxy.IsRetryableError(err))
	manager.markResponsePersisted("response-1", connection)
	mode, _ = manager.continuationRequirement("response-1", 17, "key", "scope")
	require.Equal(test, "persisted", mode)
	loss := manager.continuationLosses["response-1"]
	loss.expiresAt = time.Now().Add(-time.Second)
	manager.continuationLosses["response-1"] = loss
	mode, reason = manager.continuationRequirement("response-1", 17, "key", "scope")
	require.Equal(test, "external_unknown", mode)
	require.Equal(test, "not_recorded", reason)
}

func TestContinuationLossCapacityAndExpiration(test *testing.T) {
	manager := NewManager()
	test.Cleanup(manager.Stop)
	connection := addConnectedConn(test, manager, 17, "lane")
	manager.BindResponseConn("expired", connection, "lane", 17, "key", "scope")
	binding := manager.respConnBindings["expired"]
	binding.expiresAt = time.Now().Add(-time.Second)
	manager.respConnBindings["expired"] = binding
	_, reason := manager.continuationRequirement("expired", 17, "key", "scope")
	require.Equal(test, "binding_expired", reason)
	require.NotContains(test, manager.respConnBindings, "expired")
	_, reason = manager.continuationRequirement("expired", 17, "key", "scope")
	require.Equal(test, "lost:binding_expired", reason)
	manager.respConnMu.Lock()
	for index := 0; index <= responseConnBindingMaxEntries; index++ {
		manager.rememberContinuationLossLocked(fmt.Sprint(index), binding, "capacity-test")
	}
	manager.respConnMu.Unlock()
	require.Len(test, manager.continuationLosses, responseConnBindingMaxEntries)
}

func TestConnectionLocalContinuationCannotCrossDownstreamConnection(test *testing.T) {
	manager := NewManager()
	test.Cleanup(manager.Stop)
	manager.probeFunc = func(*WsConnection) bool { return true }
	connection := addConnectedConn(test, manager, 17, "lane")
	connection.downstreamConnectionID = "downstream-a"
	manager.BindResponseConn("response-1", connection, "lane", 17, "key", "scope")
	account := &auth.Account{DBID: 17}
	_, _, _, local, err := acquireContinuation(manager, nil, "response-1", account, "key", "scope", connection.URL, nil, "", "downstream-b")
	require.Error(test, err)
	require.True(test, local)
	require.True(test, connection.IsConnected())
	manager.markResponsePersisted("response-1", connection)
	fresh, _, _, local, err := acquireContinuation(manager, nil, "response-1", account, "key", "scope", connection.URL, nil, "", "downstream-b")
	require.NoError(test, err)
	require.False(test, local)
	require.Nil(test, fresh)
}

func TestConnectionLifecycleLogsFirstCloseReason(test *testing.T) {
	manager := NewManager()
	test.Cleanup(manager.Stop)
	connection := addConnectedConn(test, manager, 17, "private-lane")
	connection.diagnosticID = proxy.NewUpstreamSessionUUID()
	connection.createdAt = time.Now().Add(-time.Minute).UnixNano()
	connection.noteExit("upstream_close", &websocket.CloseError{Code: 1001, Text: "secret-peer-text"})
	connection.noteExit("pool_discarded", nil)
	connection.Close()
	connection.Close()
	payload := []byte(fmt.Sprintf(`{"upstream":{"connection_id":%q,"account_id":17}}`, connection.diagnosticID))
	result := proxy.EnrichWebsocketLifecycle(payload)
	require.Equal(test, "upstream_close", gjson.GetBytes(result, "upstream.connection_lifecycle.exit_reason").String())
	require.Equal(test, int64(1001), gjson.GetBytes(result, "upstream.connection_lifecycle.close_code").Int())
	require.NotContains(test, string(result), "secret-peer-text")
	require.NotContains(test, string(result), "private-lane")
	require.Equal(test, "upstream_eof", readExitReason(io.EOF))
	require.Equal(test, "unexpected_business_frame", readExitReason(errReadPumpIdleFrame))
	require.GreaterOrEqual(test, connection.lifecycle("closed").AgeMillis, int64(60000))
}
