package wsrelay

import (
	"net/http"
	"testing"

	"github.com/codex2api/proxy"
	"github.com/stretchr/testify/require"
)

func TestTurnStateHandshakeClearsNestedMetadata(t *testing.T) {
	for _, clear := range []func(http.Header){prepareCodexHandshakeSnapshot, stripCodexFrameScopedHandshakeHeaders, proxy.ClearCodexTurnStateHeaders} {
		headers := http.Header{"X-Codex-Turn-State": {"private-state"}, "x-codex-turn-state": {"private-state"}, "X-Codex-Turn-Metadata": {`{"turn_id":"keep","nested":{"X-Codex-Turn-State":"private-state"}}`}}
		clear(headers)
		require.NotContains(t, headers.Get("X-Codex-Turn-Metadata"), "private-state")
		require.Empty(t, headers.Get("X-Codex-Turn-State"))
		require.Empty(t, headers["x-codex-turn-state"])
	}
}

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
