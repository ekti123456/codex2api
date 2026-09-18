package proxy

import (
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestCompactionMetadataCarriersAndCapture(t *testing.T) {
	for _, stringMetadata := range []bool{false, true} {
		t.Run(map[bool]string{false: "object", true: "string"}[stringMetadata], func(t *testing.T) {
			bodyMetadata := `{"request_kind":"compaction","compaction":{"implementation":"local","trigger":"auto","reason":"token_limit","phase":"mid_turn","strategy":"memento","extra":"private-field"}}`
			var encoded any = json.RawMessage(bodyMetadata)
			if stringMetadata {
				encoded = bodyMetadata
			}
			body, err := json.Marshal(map[string]any{"input": []any{}, "client_metadata": map[string]any{"x-codex-turn-metadata": encoded}})
			require.NoError(t, err)
			original := string(body)
			request := transportTestContext()
			request.Request.Header.Set(codexTurnMetadataHeader, `{"request_kind":"compaction","compaction":{"implementation":"responses_compaction_v2","trigger":"manual","reason":"user_requested","phase":"standalone_turn"}}`)
			captureUsageRequestIngress(request, body)
			beginUpstreamTrace(request.Request.Context(), &auth.Account{DBID: 17}, "", true)
			UpstreamTransportObserver(request.Request.Context()).ResponsesInput(body, nil, "/v1/responses")
			usage := &database.UsageLogInput{AccountID: 17, StatusCode: 200}
			populateUpstreamTrace(request, usage)
			populateUsageRequestDiagnostics(request, usage)
			require.Equal(t, "responses_compaction_v2", gjson.Get(usage.RequestDiagnostics, "responses_input.compaction_metadata.header.implementation").String())
			require.Equal(t, "local", gjson.Get(usage.RequestDiagnostics, "responses_input.compaction_metadata.body.implementation").String())
			require.Equal(t, "absent", gjson.Get(usage.RequestDiagnostics, "upstream.responses_input.compaction_metadata.header.state").String())
			require.Equal(t, "local", gjson.Get(usage.RequestDiagnostics, "upstream.responses_input.compaction_metadata.body.implementation").String())
			require.NotContains(t, usage.RequestDiagnostics, "private-field")
			require.Equal(t, original, string(body))
		})
	}
}

func TestCompactionMetadataMissingInvalidAndBounded(t *testing.T) {
	for _, tc := range []struct{ name, metadata, state string }{
		{"absent", `{}`, "absent"},
		{"null", `{"compaction":null}`, "invalid_type"},
		{"array", `{"compaction":[]}`, "invalid_type"},
		{"string", `{"compaction":"private-value"}`, "invalid_type"},
		{"invalid_parent", `null`, "invalid_metadata"},
		{"large", `{"compaction":{"extra":"` + strings.Repeat("private", 300) + `"}}`, "too_large"},
	} {
		t.Run(tc.name, func(t *testing.T) {
			value := captureCompactionMetadataValue(gjson.Parse(tc.metadata))
			require.Equal(t, tc.state, value.State)
			encoded, err := json.Marshal(value)
			require.NoError(t, err)
			require.NotContains(t, string(encoded), "private")
			require.Less(t, len(encoded), 128)
		})
	}
	value := captureCompactionMetadataValue(gjson.Parse(`{"compaction":{"implementation":"local","trigger":42,"reason":"private free text","phase":"","strategy":null}}`))
	require.Equal(t, "present", value.State)
	require.Equal(t, "local", value.Implementation)
	require.Equal(t, []string{"trigger", "reason", "phase", "strategy"}, value.InvalidFields)
	headers := http.Header{}
	headers.Set(codexTurnMetadataHeader, "not-json")
	shape := diagnoseResponsesInput([]byte(`{"input":[{"type":"compaction_trigger"}]}`), headers, "")
	require.Equal(t, "invalid_metadata", shape.CompactionMetadata.Header.State)
	require.Equal(t, "absent", shape.CompactionMetadata.Body.State)
	headers.Set(codexTurnMetadataHeader, strings.Repeat("x", 16385))
	require.Equal(t, "too_large", diagnoseResponsesInput([]byte(`{"input":[]}`), headers, "").CompactionMetadata.Header.State)
	require.Nil(t, diagnoseResponsesInput([]byte(`{"input":"ordinary"}`), nil, "").CompactionMetadata)
}

func TestCompactionMetadataLargeDiagnosticPreservesIngress(t *testing.T) {
	request := transportTestContext()
	body := []byte(`{"input":[],"client_metadata":{"x-codex-turn-metadata":{"request_kind":"compaction","compaction":{"implementation":"local","trigger":"auto"}}}}`)
	captureUsageRequestIngress(request, body)
	largeValue := strings.Repeat("x", 12*1024)
	usageRequestDiagnosticState(request).Incoming["large_snapshot"] = map[string]string{"value": largeValue}
	beginUpstreamTrace(request.Request.Context(), &auth.Account{DBID: 17}, "", true)
	UpstreamTransportObserver(request.Request.Context()).ResponsesInput(body, nil, "/v1/responses")
	usage := &database.UsageLogInput{AccountID: 17, StatusCode: 200}
	populateUpstreamTrace(request, usage)
	populateUsageRequestDiagnostics(request, usage)
	require.Greater(t, len(usage.RequestDiagnostics), 12*1024)
	require.False(t, gjson.Get(usage.RequestDiagnostics, "truncated").Bool())
	require.Equal(t, largeValue, gjson.Get(usage.RequestDiagnostics, "incoming.large_snapshot.value").String())
	for _, prefix := range []string{"responses_input", "upstream.responses_input"} {
		require.Equal(t, "local", gjson.Get(usage.RequestDiagnostics, prefix+".compaction_metadata.body.implementation").String())
	}
}
