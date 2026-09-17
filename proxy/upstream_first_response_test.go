package proxy

import (
	"net/http"
	"net/http/httptest"
	"testing"
	"time"

	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestUpstreamFirstResponseTimingNormalCommitAndRetry(t *testing.T) {
	start := time.Now()
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	failedHeaders := make(http.Header)
	failed := upstreamFirstResponseTiming{enabled: true, requestStart: start, attemptStart: start}
	failed.observe(failedHeaders, gjson.Parse(`{"type":"codex.rate_limits"}`), start.Add(100*time.Millisecond))
	require.False(t, c.Writer.Written(), "observing metadata must not commit HTTP 200")
	require.Empty(t, recorder.Header())

	winnerHeaders := make(http.Header)
	winner := upstreamFirstResponseTiming{enabled: true, requestStart: start, attemptStart: start.Add(2 * time.Second)}
	for _, event := range []string{"response.created", "response.in_progress", "response.failed", "response.completed", "error", "ping", "keepalive", "heartbeat"} {
		winner.observe(winnerHeaders, gjson.Parse(`{"type":"`+event+`"}`), start.Add(2100*time.Millisecond))
	}
	require.Empty(t, winnerHeaders)
	winner.observe(winnerHeaders, gjson.Parse(`{"type":"response.metadata"}`), start.Add(3*time.Second))
	winner.observe(winnerHeaders, gjson.Parse(`{"type":"response.output_text.delta","delta":"hello"}`), start.Add(10*time.Second))
	relayUpstreamFirstResponseHeaders(c, winnerHeaders)
	require.False(t, c.Writer.Written(), "staging timing must not flush or write a body")
	c.String(http.StatusOK, "hello")
	result := recorder.Result()
	require.Equal(t, "3000", result.Header.Get(upstreamFirstResponseHeader), "include earlier attempt and retry wait")
	require.Equal(t, "1000", result.Header.Get(upstreamAttemptFirstResponseHeader), "use the winning attempt")
	require.Equal(t, "hello", recorder.Body.String())
	// A later event or abandoned attempt cannot change already committed headers.
	relayUpstreamFirstResponseHeaders(c, failedHeaders)
	require.Equal(t, "3000", result.Header.Get(upstreamFirstResponseHeader))
}

func TestUpstreamFirstResponseDisabledOrCommittedFallsBack(t *testing.T) {
	start := time.Now()
	headers := make(http.Header)
	disabled := upstreamFirstResponseTiming{requestStart: start, attemptStart: start}
	disabled.observe(headers, gjson.Parse(`{"type":"codex.rate_limits"}`), start.Add(time.Second))
	require.Empty(t, headers)
	recorder := httptest.NewRecorder()
	c, _ := gin.CreateTestContext(recorder)
	c.Writer.WriteHeaderNow() // An existing heartbeat has already committed.
	enabled := upstreamFirstResponseTiming{enabled: true, requestStart: start, attemptStart: start}
	enabled.observe(headers, gjson.Parse(`{"type":"codex.rate_limits"}`), start.Add(time.Second))
	relayUpstreamFirstResponseHeaders(c, headers)
	require.Empty(t, recorder.Result().Header.Get(upstreamTimingHeader))
}
