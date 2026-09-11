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
)

func TestConnectionProfileIncludesHandshakeNotTurnMetadata(test *testing.T) {
	headers := http.Header{"User-Agent": {"codex-tui/1"}, "Authorization": {"Bearer secret"}, "Originator": {"codex-tui"}, "Version": {"1"}}
	original := websocketConnectionProfile(headers)
	headers.Set("X-Codex-Turn-Metadata", `{"turn_id":"new-turn"}`)
	headers.Set("X-Codex-Window-Id", "thread:2")
	require.Equal(test, original, websocketConnectionProfile(headers))
	for _, name := range []string{"User-Agent", "Originator", "Version", "Authorization", "Cookie", "OpenAI-Beta", "X-Custom-Header"} {
		changed := headers.Clone()
		changed.Set(name, "changed")
		require.NotEqual(test, original, websocketConnectionProfile(changed), name)
	}
	require.NotContains(test, original, "secret")
}

func TestConnectionProfileChangeRotatesIdleConnection(test *testing.T) {
	for _, reusable := range []bool{false, true} {
		var openings atomic.Int32
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
			}
		}))
		test.Cleanup(server.Close)
		manager := NewManager()
		test.Cleanup(manager.Stop)
		manager.probeFunc = func(*WsConnection) bool { return true }
		account := &auth.Account{DBID: 17, Status: auth.StatusReady}
		headers := http.Header{"User-Agent": {"codex-tui/1"}}
		wsURL := "ws" + strings.TrimPrefix(server.URL, "http")
		acquire := func() (*WsConnection, *PendingRequest) {
			ctx, cancel := context.WithTimeout(context.Background(), 3*time.Second)
			defer cancel()
			if reusable {
				connection, pending, _, err := manager.AcquireReusableConnection(ctx, account, wsURL, "lane", "fallback", 1, headers, "")
				require.NoError(test, err)
				return connection, pending
			}
			connection, pending, err := manager.AcquireConnection(ctx, account, wsURL, "lane", headers, "")
			require.NoError(test, err)
			return connection, pending
		}
		first, pending := acquire()
		releaseUnsentConnection(manager, first, pending)
		same, pending := acquire()
		require.Same(test, first, same)
		releaseUnsentConnection(manager, same, pending)
		headers.Set("User-Agent", "codex-tui/2")
		next, pending := acquire()
		require.NotSame(test, first, next)
		require.Equal(test, int32(2), openings.Load())
		require.False(test, first.IsConnected())
		releaseUnsentConnection(manager, next, pending)
		manager.Stop()
		server.Close()
	}
}

func TestCompletedContinuationBoundBeforeDelivery(test *testing.T) {
	for _, store := range []string{"true", "false", `"true"`, "null"} {
		test.Run(store, func(test *testing.T) {
			manager := NewManager()
			test.Cleanup(manager.Stop)
			connection := addConnectedConn(test, manager, 17, "lane")
			response := &WsResponse{manager: manager, conn: connection, sessionID: "lane", apiKey: "key", requestScope: "scope"}
			payload := []byte(`{"type":"response.completed","response":{"id":"response-1","store":` + store + `}}`)
			err := response.handleMessage(payload, func([]byte) bool {
				mode, result := manager.continuationRequirement("response-1", 17, "key", "scope")
				require.Equal(test, "known", result)
				if store == "true" {
					require.Equal(test, "persisted", mode)
				} else {
					require.Equal(test, "connection_local", mode)
				}
				return true
			})
			require.ErrorIs(test, err, io.EOF)
		})
	}
}

func TestContinuationDoesNotFallBackWhenConnectionLocal(test *testing.T) {
	for _, scenario := range []string{"ready", "lost", "busy", "profile", "proxy", "account", "api_key", "scope", "persisted", "unknown"} {
		test.Run(scenario, func(test *testing.T) {
			manager := NewManager()
			test.Cleanup(manager.Stop)
			manager.probeFunc = func(*WsConnection) bool { return true }
			connection := addConnectedConn(test, manager, 17, "lane")
			headers := http.Header{"User-Agent": {"codex-tui/1"}}
			connection.handshakeProfile = websocketConnectionProfile(headers)
			manager.BindResponseConn("response-1", connection, "lane", 17, "key", "scope")
			account := &auth.Account{DBID: 17, Status: auth.StatusReady}
			apiKey, scope, proxyURL := "key", "scope", ""
			responseID := "response-1"
			switch scenario {
			case "lost", "persisted":
				if scenario == "persisted" {
					manager.markResponsePersisted(responseID, connection)
				}
				manager.DiscardConnection(connection)
			case "busy":
				connection.session.AddPendingRequest("busy")
			case "profile":
				headers.Set("User-Agent", "codex-tui/2")
			case "proxy":
				proxyURL = "http://proxy.invalid:3128"
			case "account":
				account.DBID = 18
			case "api_key":
				apiKey = "different"
			case "scope":
				scope = "different"
			case "unknown":
				responseID = "unknown"
			}
			acquired, pending, _, local, err := acquireContinuation(manager, nil, responseID, account, apiKey, scope, connection.URL, headers, proxyURL)
			if scenario == "ready" {
				require.NoError(test, err)
				require.Same(test, connection, acquired)
				require.True(test, local)
				releaseUnsentConnection(manager, acquired, pending)
			} else if scenario == "persisted" || scenario == "unknown" {
				require.NoError(test, err)
				require.False(test, local)
				require.Nil(test, acquired)
			} else {
				require.Error(test, err)
				require.False(test, proxy.IsRetryableError(err))
				require.Equal(test, http.StatusBadRequest, proxy.StatusCodeFromError(err))
				require.Nil(test, acquired)
			}
		})
	}
}
