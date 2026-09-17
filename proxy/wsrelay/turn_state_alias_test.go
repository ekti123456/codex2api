package wsrelay

import (
	"net/http"
	"testing"

	"github.com/stretchr/testify/require"
)

func TestTurnStateHandshakeIsNotReplayedOnPooledConnection(t *testing.T) {
	conn := &WsConnection{}
	headers := http.Header{"X-Codex-Turn-State": []string{"first-turn"}, "Other": []string{"keep"}}
	require.Equal(t, "first-turn", turnScopedHandshakeHeaders(conn, headers).Get("X-Codex-Turn-State"))
	for range 3 {
		current := turnScopedHandshakeHeaders(conn, headers)
		require.Empty(t, current.Get("X-Codex-Turn-State"))
		require.Equal(t, "keep", current.Get("Other"))
	}
	require.Equal(t, "first-turn", headers.Get("X-Codex-Turn-State"), "must not mutate the stored handshake")
	require.Equal(t, "first-turn", turnScopedHandshakeHeaders(&WsConnection{}, headers).Get("X-Codex-Turn-State"))
}
