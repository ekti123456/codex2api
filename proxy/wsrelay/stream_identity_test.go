package wsrelay

import (
	"io"
	"testing"

	"github.com/codex2api/proxy"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestStreamIdentityProtocolEchoAndBusinessContent(t *testing.T) {
	response := &WsResponse{clientStreamID: "client", upstreamStreamID: "outbound"}
	var seen []byte
	err := response.handleMessage([]byte(`{"type":"response.completed","stream_id":"outbound","response":{"id":"resp_one","stream_id":"outbound","metadata":{"nested":"{\"stream_id\":\"outbound\"}"},"output":[{"type":"message","content":[{"type":"output_text","text":"outbound stream_id"}]}]}}`), func(data []byte) bool { seen = data; return true })
	require.ErrorIs(t, err, io.EOF)
	require.Equal(t, "client", gjson.GetBytes(seen, "stream_id").String())
	require.Equal(t, "client", gjson.GetBytes(seen, "response.stream_id").String())
	require.JSONEq(t, `{"stream_id":"client"}`, gjson.GetBytes(seen, "response.metadata.nested").String())
	require.Equal(t, "outbound stream_id", gjson.GetBytes(seen, "response.output.0.content.0.text").String())
	err = response.handleMessage([]byte(`{"type":"error","stream_id":"outbound","status":400,"error":{"message":"rejected stream outbound","stream_id":"outbound"}}`), func(data []byte) bool { seen = data; return true })
	require.ErrorIs(t, err, io.EOF)
	require.Equal(t, "response.failed", gjson.GetBytes(seen, "type").String())
	require.Equal(t, "client", gjson.GetBytes(seen, "stream_id").String())
	require.NotContains(t, string(seen), "outbound")
	require.Equal(t, "rejected stream client", gjson.GetBytes(seen, "response.error.message").String())
}

func TestStreamIdentityRejectsOtherLaneBeforeDelivery(t *testing.T) {
	for _, payload := range []string{`{"type":"response.completed","stream_id":"foreign"}`, `{"type":"response.completed","response":{"metadata":{"stream_id":"foreign"}}}`} {
		response := &WsResponse{clientStreamID: "client", upstreamStreamID: "outbound"}
		err := response.handleMessage([]byte(payload), func([]byte) bool { t.Fatal("foreign stream delivered"); return true })
		require.Error(t, err)
		require.True(t, proxy.TransportReplayBlocked(err))
		require.True(t, response.connBroken)
	}
}
