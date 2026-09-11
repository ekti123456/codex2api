package wsrelay

import (
	"errors"
	"io"
	"net"
	"time"

	"github.com/codex2api/proxy"
	"github.com/gorilla/websocket"
)

func (connection *WsConnection) noteExit(reason string, err error) {
	if connection == nil {
		return
	}
	connection.lifecycleMu.Lock()
	defer connection.lifecycleMu.Unlock()
	if connection.exitReason != "" {
		return
	}
	connection.exitReason = reason
	var closeErr *websocket.CloseError
	if errors.As(err, &closeErr) {
		connection.exitCloseCode = closeErr.Code
	}
}

func (connection *WsConnection) lifecycle(state string) proxy.WebsocketConnectionLifecycle {
	connection.lifecycleMu.Lock()
	reason, code := connection.exitReason, connection.exitCloseCode
	connection.lifecycleMu.Unlock()
	now := time.Now().UTC()
	event := proxy.WebsocketConnectionLifecycle{ConnectionID: connection.diagnosticID, State: state,
		PoolKeyHash: proxy.TransportPoolKeyHash(connection.PoolKey), ObservedAt: now,
		ExitReason: reason, CloseCode: code}
	if connection.createdAt > 0 {
		event.OpenedAt = time.Unix(0, connection.createdAt).UTC()
		event.AgeMillis = max(now.Sub(event.OpenedAt).Milliseconds(), 0)
	}
	if last := connection.lastUsed.Load(); last > 0 {
		event.IdleMillis = max(now.Sub(time.Unix(0, last)).Milliseconds(), 0)
	}
	if connection.session != nil {
		event.AccountID, event.Pending = connection.session.AccountID, connection.session.PendingCount()
	}
	return event
}

func readExitReason(err error) string {
	var closeErr *websocket.CloseError
	var networkErr net.Error
	switch {
	case errors.Is(err, websocket.ErrReadLimit):
		return "local_read_limit"
	case errors.Is(err, errReadPumpIdleFrame), errors.Is(err, errReadPumpUncommitted):
		return "unexpected_business_frame"
	case errors.Is(err, errReadPumpQueueOverflow):
		return "receive_queue_overflow"
	case errors.As(err, &closeErr):
		return "upstream_close"
	case errors.As(err, &networkErr) && networkErr.Timeout():
		return "read_timeout"
	case errors.Is(err, io.EOF), errors.Is(err, io.ErrUnexpectedEOF):
		return "upstream_eof"
	default:
		return "read_failure"
	}
}
