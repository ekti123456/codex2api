package proxy

import (
	"context"
	"errors"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestResponsesDeliveryDiagnosticsSeparateGenerationAndDelivery(test *testing.T) {
	for _, scenario := range []struct {
		name        string
		canceled    bool
		writeErr    error
		disposition string
		wantStatus  string
	}{
		{name: "successful write", disposition: "accepted", wantStatus: "write_accepted"},
		{name: "drain after cancellation", canceled: true, wantStatus: "client_canceled"},
		{name: "terminal write failed", disposition: "accepted", writeErr: errors.New("broken pipe"), wantStatus: "write_failed"},
		{name: "private replay is not delivery", disposition: "buffered", wantStatus: "buffered"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			request := transportTestContext()
			beginUpstreamTrace(request.Request.Context(), &auth.Account{DBID: 17}, "", true)
			observer := UpstreamTransportObserver(request.Request.Context())
			observer.ResponsesTerminal("response.incomplete", []byte(`{"response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"usage":{"input_tokens":64500,"output_tokens":174},"output":[{"secret":"never log content"}]}}`), scenario.canceled)
			if scenario.disposition != "" {
				observer.ResponsesTerminalWrite(scenario.disposition, scenario.writeErr)
			}
			var ctxErr error
			if scenario.canceled {
				ctxErr = context.Canceled
			}
			observer.ResponsesDeliveryFinished(ctxErr, scenario.writeErr)
			input := &database.UsageLogInput{AccountID: 17, StatusCode: 200, Stream: true}
			populateUpstreamTrace(request, input)
			populateUsageRequestDiagnostics(request, input)
			delivery := gjson.Get(input.RequestDiagnostics, "upstream.stream_delivery")
			require.Equal(test, "response.incomplete", delivery.Get("terminal_event").String())
			require.Equal(test, "incomplete", delivery.Get("response_status").String())
			require.Equal(test, "max_output_tokens", delivery.Get("incomplete_reason").String())
			require.Equal(test, "upstream", delivery.Get("usage_source").String())
			require.Equal(test, scenario.canceled, delivery.Get("usage_received_after_cancel").Bool())
			require.Equal(test, scenario.wantStatus, delivery.Get("downstream_status").String())
			require.NotContains(test, input.RequestDiagnostics, "never log content")
		})
	}
}

func TestResponsesHandlerRecordsIncompleteDeliveryWithoutRetry(test *testing.T) {
	handler, calls := newChatStreamTerminalTestHandler(test, []string{
		`{"type":"response.output_text.delta","delta":"half"}`,
		`{"type":"response.incomplete","response":{"status":"incomplete","incomplete_details":{"reason":"max_output_tokens"},"output":[],"usage":{"input_tokens":11,"output_tokens":7,"total_tokens":18}}}`,
	})
	recorder := httptest.NewRecorder()
	request, _ := gin.CreateTestContext(recorder)
	request.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", strings.NewReader(`{"model":"gpt-5.5","stream":true,"input":"hello"}`))
	attachUpstreamTrace(request, handler.store)
	handler.Responses(request)
	require.Equal(test, http.StatusOK, recorder.Code)
	require.Contains(test, recorder.Body.String(), `"type":"response.incomplete"`)
	require.EqualValues(test, 1, calls.Load())
	trace := snapshotUpstreamTrace(request.Request.Context())
	require.NotNil(test, trace.Transport)
	require.NotNil(test, trace.Transport.StreamDelivery)
	require.Equal(test, "incomplete", trace.Transport.StreamDelivery.ResponseStatus)
	require.Equal(test, "write_accepted", trace.Transport.StreamDelivery.DownstreamStatus)
	require.Equal(test, "upstream", trace.Transport.StreamDelivery.UsageSource)
}

func TestResponsesDeliverySnapshotAndRetriesStayIsolated(test *testing.T) {
	request := transportTestContext()
	beginUpstreamTrace(request.Request.Context(), &auth.Account{DBID: 17}, "", false)
	observer := UpstreamTransportObserver(request.Request.Context())
	observer.ResponsesTerminal("response.completed", []byte(`{"response":{"status":"completed","usage":{}}}`), false)
	snapshot := snapshotUpstreamTrace(request.Request.Context())
	observer.ResponsesTerminalWrite("accepted", nil)
	observer.ResponsesDeliveryFinished(nil, nil)
	require.Equal(test, "not_attempted", snapshot.Transport.StreamDelivery.TerminalWrite)
	beginUpstreamTrace(request.Request.Context(), &auth.Account{DBID: 18}, "", false)
	observer.ResponsesDeliveryFinished(context.Canceled, nil)
	require.Nil(test, snapshotUpstreamTrace(request.Request.Context()).Transport.StreamDelivery)
}
