package proxy

import (
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestResponsesInputDiagnosticModes(test *testing.T) {
	for _, scenario := range []struct {
		name     string
		body     string
		metadata string
		endpoint string
		mode     string
	}{
		{"ordinary", `{"input":"hello"}`, "", "", "ordinary"},
		{"header_metadata", `{"input":[]}`, `{"request_kind":"compaction"}`, "", "metadata_only"},
		{"object_metadata", `{"client_metadata":{"x-codex-turn-metadata":{"request_kind":"compaction"}},"input":[]}`, "", "", "metadata_only"},
		{"string_metadata", `{"client_metadata":{"x-codex-turn-metadata":"{\"request_kind\":\"compaction\"}"},"input":[]}`, "", "", "metadata_only"},
		{"protocol", `{"input":{"type":"compaction_trigger"}}`, "", "", "protocol_trigger"},
		{"history", `{"input":[{"type":"compaction","encrypted_content":"opaque"}]}`, "", "", "history_only"},
		{"endpoint", `{}`, "", "/backend-api/codex/responses/compact", "compact_endpoint"},
		{"nested_tool_data", `{"input":[{"type":"function_call_output","output":{"type":"compaction_trigger"}}]}`, "", "", "ordinary"},
		{"nested_metadata", `{"input":[{"role":"user","content":"request_kind=compaction"}]}`, "", "", "ordinary"},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			headers := http.Header{}
			headers.Set(codexTurnMetadataHeader, scenario.metadata)
			shape := diagnoseResponsesInput([]byte(scenario.body), headers, scenario.endpoint)
			require.NotNil(test, shape)
			require.Equal(test, scenario.mode, shape.Mode)
			require.Equal(test, len(scenario.body), shape.JSONBytes)
		})
	}
	require.Nil(test, diagnoseResponsesInput([]byte(`invalid`), nil, ""))
	require.Nil(test, diagnoseResponsesInput([]byte(`{"messages":[]}`), nil, ""))
}

func TestResponsesInputDiagnosticPrivacyAndCounts(test *testing.T) {
	body := []byte(`{"previous_response_id":"private-response-id","client_metadata":{"installation_id":"private-device-id"},"input":[{"type":"compaction_trigger","extension":"private-extension"},{"type":"compaction","id":"private-item-id","encrypted_content":"private-encrypted"},{"type":"message","role":"developer","content":[{"type":"input_text","text":"[Conversation summary from earlier turns]\nprivate-summary"}]},{"type":"function_call_output","call_id":"private-call-id","output":"[tool output was not recorded]"},{"type":"compaction_trigger"}]}`)
	shape := diagnoseResponsesInput(body, nil, "")
	require.Equal(test, 5, shape.InputItems)
	require.Equal(test, 2, shape.ProtocolTriggerCount)
	require.True(test, shape.TriggerAtEnd)
	require.Equal(test, 1, shape.CompactionItems)
	require.Equal(test, 1, shape.EncryptedCompactionItems)
	require.Equal(test, len("private-encrypted"), shape.EncryptedCompactionBytes)
	require.Equal(test, 1, shape.CompactionItemsWithID)
	require.True(test, shape.PreviousResponseIDPresent)
	require.Equal(test, 1, shape.SummaryPrefixItems)
	require.Equal(test, 1, shape.ToolOutputPlaceholderItems)
	encoded, err := json.Marshal(shape)
	require.NoError(test, err)
	require.NotContains(test, string(encoded), "private-")
	require.Less(test, len(encoded), 1024)
}

func TestResponsesInputDiagnosticsCaptureIngressAndPreparedHTTP(test *testing.T) {
	var received []byte
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		received, _ = io.ReadAll(request.Body)
		writer.Header().Set("Content-Type", "application/json")
		writer.WriteHeader(http.StatusInternalServerError)
		_, _ = writer.Write([]byte(`{"error":{"code":"server_is_overloaded"}}`))
	}))
	defer server.Close()
	request := transportTestContext()
	account := &auth.Account{DBID: 42, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: server.URL, APIKey: "private-relay-key"}
	body := []byte(`{"model":"gpt-5.6-sol","input":[{"type":"compaction_trigger","extension":{"value":42}},{"role":"user","content":"private-input"}]}`)
	captureUsageRequestIngress(request, body)
	response, err := ExecuteOpenAIResponsesRequest(request.Request.Context(), account, body, "", nil)
	require.NoError(test, err)
	_, err = io.Copy(io.Discard, response.Body)
	require.NoError(test, err)
	require.NoError(test, response.Body.Close())
	usage := &database.UsageLogInput{AccountID: 42, StatusCode: 500}
	populateUpstreamTrace(request, usage)
	populateUsageRequestDiagnostics(request, usage)
	require.False(test, gjson.Get(usage.RequestDiagnostics, "responses_input.trigger_at_end").Bool())
	require.True(test, gjson.Get(usage.RequestDiagnostics, "upstream.responses_input.trigger_at_end").Bool())
	require.EqualValues(test, len(received), gjson.Get(usage.RequestDiagnostics, "upstream.responses_input.json_bytes").Int())
	require.EqualValues(test, 42, gjson.GetBytes(received, "input.1.extension.value").Int())
	require.NotContains(test, usage.RequestDiagnostics, "private-")
}

func TestResponsesInputDiagnosticsDoNotLeakAcrossAttempts(test *testing.T) {
	request := transportTestContext()
	beginUpstreamTrace(request.Request.Context(), &auth.Account{DBID: 17}, "", true)
	oldObserver := UpstreamTransportObserver(request.Request.Context())
	oldObserver.ResponsesInput([]byte(`{"input":[{"type":"compaction_trigger"}]}`), nil, "")
	first := snapshotUpstreamTrace(request.Request.Context())
	require.Equal(test, "protocol_trigger", first.Transport.ResponsesInput.Mode)
	beginUpstreamTrace(request.Request.Context(), &auth.Account{DBID: 17}, "", true)
	observer := UpstreamTransportObserver(request.Request.Context())
	observer.ResponsesInput([]byte(`{"input":"continue"}`), nil, "")
	oldObserver.ResponsesInput([]byte(`{"input":[{"type":"compaction_trigger"}]}`), nil, "")
	require.Equal(test, "ordinary", snapshotUpstreamTrace(request.Request.Context()).Transport.ResponsesInput.Mode)
	require.Equal(test, "protocol_trigger", first.Transport.ResponsesInput.Mode)
	observer.ResponsesInput([]byte(`{"input":"`+strings.Repeat("private-input", 1000)+`"}`), nil, "")
	encoded := transportDiagnosticJSON(snapshotUpstreamTrace(request.Request.Context()).Transport)
	require.NotContains(test, encoded, "private-input")
	require.Less(test, len(encoded), 2048)
}
