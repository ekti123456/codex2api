package proxy

import (
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
)

func TestUpstreamResponseModelObservationAndIsolation(t *testing.T) {
	r := transportTestContext()
	account := &auth.Account{DBID: 17}
	beginUpstreamTrace(r.Request.Context(), account, "", true)
	observer := UpstreamTransportObserver(r.Request.Context())
	observer.Event([]byte(`{"type":"response.created","response":{"model":"gpt-6-astra"}}`))
	observer.Event([]byte(`{"type":"response.completed","response":{"model":"gpt-5.6-luna"}}`))
	observer.Event([]byte(`{"type":"response.output_text.delta","delta":"model: other"}`))
	d := snapshotUpstreamTrace(r.Request.Context()).Transport
	require.Equal(t, "gpt-5.6-luna", d.ResponseModel)
	require.True(t, d.ResponseModelConflict)
	input := &database.UsageLogInput{AccountID: 17, Model: "gpt-6-astra", EffectiveModel: "gpt-6-astra", StatusCode: 200}
	populateUpstreamTrace(r, input)
	require.Equal(t, "gpt-5.6-luna", input.UpstreamResponseModel)
	require.Equal(t, "gpt-6-astra", input.Model)
	require.Equal(t, "gpt-6-astra", input.EffectiveModel)
	// New attempt / WS turn starts empty; even a late old event cannot leak in.
	beginUpstreamTrace(r.Request.Context(), account, "", true)
	observer.Event([]byte(`{"model":"late-old-model"}`))
	require.Empty(t, snapshotUpstreamTrace(r.Request.Context()).Transport.ResponseModel)
	for _, payload := range []string{`{"output":[{"model":"nested-tool-model"}]}`, `{"model":42}`, `{"model":"broken"`, `{"model":"sk-secret"}`, `{"model":"model\nheader"}`} {
		UpstreamTransportObserver(r.Request.Context()).Event([]byte(payload))
		require.Empty(t, snapshotUpstreamTrace(r.Request.Context()).Transport.ResponseModel, payload)
	}
}

func TestUpstreamResponseModelHTTPWireUnchanged(t *testing.T) {
	for _, stream := range []bool{false, true} {
		t.Run(map[bool]string{false: "json", true: "sse"}[stream], func(t *testing.T) {
			r := transportTestContext()
			const requestBody = `{"model":"gpt-6-astra","input":"hi"}`
			payload := `{"id":"response-1","model":"gpt-5.6-luna","output":[]}`
			contentType := "application/json"
			if stream {
				contentType = "text/event-stream"
				payload = "event: response.completed\ndata: {\"type\":\"response.completed\",\"response\":{\"model\":\"gpt-5.6-luna\"}}\n\n"
			}
			calls := 0
			client := &http.Client{Transport: diagnosticRoundTripper(func(req *http.Request) (*http.Response, error) {
				calls++
				body, err := io.ReadAll(req.Body)
				require.NoError(t, err)
				require.Equal(t, requestBody, string(body))
				require.Equal(t, http.Header{"Content-Type": {"application/json"}}, req.Header)
				return &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {contentType}}, Body: io.NopCloser(strings.NewReader(payload))}, nil
			})}
			out := httptest.NewRequest(http.MethodPost, "http://upstream.invalid/responses", strings.NewReader(requestBody)).WithContext(r.Request.Context())
			out.RequestURI = ""
			out.Header.Set("Content-Type", "application/json")
			resp, err := doTracedUpstreamRequest(client, out, &auth.Account{DBID: 17}, "")
			require.NoError(t, err)
			defer resp.Body.Close()
			// The encrypted-content observer used by real Codex requests must
			// not hide the transport observer or change any response bytes.
			resp.Body = &encryptedErrorObserver{ReadCloser: resp.Body, stream: stream, record: func([]byte) {}}
			if stream {
				err = ReadSSEStreamWithEvent(resp.Body, func(event string, data []byte) bool {
					require.Equal(t, "response.completed", event)
					require.Equal(t, `{"type":"response.completed","response":{"model":"gpt-5.6-luna"}}`, string(data))
					return true
				})
				require.NoError(t, err)
			} else {
				body, err := readObservedUpstreamJSON(resp.Body)
				require.NoError(t, err)
				require.Equal(t, payload, string(body))
			}
			require.Equal(t, 1, calls)
			require.Equal(t, "gpt-5.6-luna", snapshotUpstreamTrace(r.Request.Context()).Transport.ResponseModel)
		})
	}
}

func BenchmarkUpstreamResponseModelDelta(b *testing.B) {
	payload := []byte(`{"type":"response.output_text.delta","delta":"A short text fragment","output_index":0,"content_index":0}`)
	var attempt upstreamTraceAttempt
	b.ReportAllocs()
	for b.Loop() {
		observeUpstreamResponseModel(&attempt, payload, "response.output_text.delta")
	}
}
