package wsrelay

import (
	"errors"
	"testing"
	"time"

	"github.com/codex2api/proxy"
	"github.com/gorilla/websocket"
	"github.com/stretchr/testify/require"
)

func TestPeerMessageTooBigBeforeResponseAllowsHTTPFallback(test *testing.T) {
	for _, scenario := range []struct {
		name            string
		responseStarted bool
		connectionLocal bool
	}{
		{name: "before_response"},
		{name: "after_response_started", responseStarted: true},
		{name: "connection_local_without_observer", connectionLocal: true},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			finished := make(chan struct{})
			manager, connection := newReadPumpTestConnection(test, func(socket *websocket.Conn) {
				if _, _, err := socket.ReadMessage(); err != nil {
					return
				}
				if scenario.responseStarted {
					if err := socket.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.created","response":{"id":"resp_started"}}`)); err != nil {
						return
					}
				}
				_ = socket.WriteControl(websocket.CloseMessage, websocket.FormatCloseMessage(websocket.CloseMessageTooBig, ""), time.Now().Add(time.Second))
				<-finished
			})
			test.Cleanup(func() { close(finished) })
			pending := connection.session.AddPendingRequest("read-pump-test")
			require.NoError(test, connection.BeginReadLease(pending.RequestID))
			require.NoError(test, connection.WriteMessage(websocket.TextMessage, []byte(`{"type":"response.create"}`)))
			response := &WsResponse{conn: connection, pendingReq: pending, sessionID: "read-pump-test", manager: manager, connectionLocal: scenario.connectionLocal}
			result := make(chan error, 1)
			go func() { result <- response.ReadStream(func([]byte) bool { return true }) }()
			select {
			case err := <-result:
				require.Error(test, err)
				var closeErr *websocket.CloseError
				require.True(test, errors.As(err, &closeErr))
				require.Equal(test, websocket.CloseMessageTooBig, closeErr.Code)
				require.Equal(test, scenario.responseStarted || scenario.connectionLocal, proxy.TransportReplayBlocked(err))
			case <-time.After(readPumpTestTimeout):
				test.Fatal("peer 1009 did not finish the stream")
			}
			require.NoError(test, response.Close())
			require.False(test, connection.IsConnected())
		})
	}
}

func TestLocalReadLimitIsNotReportedAsPeerRejection(test *testing.T) {
	err := normalizeReadPumpError(websocket.ErrReadLimit)
	require.Equal(test, "local_read_limit", readExitReason(err))
	var observer *proxy.TransportObserver
	require.True(test, proxy.TransportReplayBlocked(observer.WebsocketReadFailure(err, false, false)))
}
