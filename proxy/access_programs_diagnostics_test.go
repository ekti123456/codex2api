package proxy

import (
	"bytes"
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

func TestAccessProgramsCaptureTypesAndBounds(t *testing.T) {
	for _, value := range []string{`null`, `{}`, `[]`, `""`, `false`, `9007199254740993`, `{"cyber":"daybreak_blue"}`, `{"cyber":"daybreak_red"}`, `{"cyber":"standard"}`} {
		t.Run(value, func(t *testing.T) {
			body := []byte(`{"input":"private-input","access_programs":` + value + `}`)
			got := captureAccessPrograms(body)
			require.Equal(t, "present", got.State)
			require.Equal(t, value, string(got.Value))
			clear(body)
			encoded, err := json.Marshal(got)
			require.NoError(t, err)
			require.Equal(t, value, gjson.GetBytes(encoded, "value").Raw)
			require.NotContains(t, string(encoded), "private-input")
		})
	}
	require.Equal(t, "absent", captureAccessPrograms([]byte(`{"client_metadata":{"access_programs":"nested"},"input":[]}`)).State)
	require.Equal(t, "invalid_json", captureAccessPrograms([]byte(`{"access_programs":`)).State)
	large := `"` + strings.Repeat("x", maxAccessProgramsDiagnosticBytes) + `"`
	got := captureAccessPrograms([]byte(`{"access_programs":` + large + `}`))
	require.Equal(t, "too_large", got.State)
	require.Nil(t, got.Value)
	require.Equal(t, len(large), got.Bytes)
	require.Len(t, got.SHA256, 64)
	escaped := captureAccessPrograms([]byte(`{"access_programs":"` + strings.Repeat("<", 300) + `"}`))
	require.Equal(t, "too_large", escaped.State, "JSON escaping must also respect the log budget")
}

func TestAccessProgramsHTTPFinalPayloadRulesAndError(t *testing.T) {
	for _, tc := range []struct{ name, body, rules, want string }{
		{"unchanged", `{"access_programs":{"cyber":"daybreak_blue"}}`, `{}`, `{"cyber":"daybreak_blue"}`},
		{"replace", `{"access_programs":{"cyber":"daybreak_blue"}}`, `{"override":[{"params":{"access_programs":{"cyber":"standard"}}}]}`, `{"cyber":"standard"}`},
		{"add", `{}`, `{"override":[{"params":{"access_programs":{"cyber":"daybreak_red"}}}]}`, `{"cyber":"daybreak_red"}`},
		{"remove", `{"access_programs":{"cyber":"daybreak_blue"}}`, `{"filter":[{"params":["access_programs"]}]}`, ""},
		{"null", `{"access_programs":null}`, `{}`, `null`},
	} {
		t.Run(tc.name, func(t *testing.T) {
			withPayloadRules(t, tc.rules)
			seen := make(chan []byte, 1)
			server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
				body, _ := io.ReadAll(r.Body)
				seen <- body
				w.Header().Set("Content-Type", "application/json")
				w.WriteHeader(http.StatusBadRequest)
				_, _ = io.WriteString(w, `{"error":{"code":"unsupported_parameter","type":"invalid_request_error","message":"The access_programs parameter is not enabled for this organization."}}`)
			}))
			defer server.Close()
			request := transportTestContext()
			body := []byte(tc.body)
			captureUsageRequestIngress(request, body)
			prepared := ApplyPayloadRulesToBody(body, "gpt-6-astra", nil, nil)
			captureUsageRequestIngress(request, prepared) // Resolution must not replace original ingress.
			outgoing, err := http.NewRequestWithContext(request.Request.Context(), http.MethodPost, server.URL+"/v1/responses", bytes.NewReader(prepared))
			require.NoError(t, err)
			response, err := doTracedUpstreamRequest(server.Client(), outgoing, &auth.Account{DBID: 1712}, "", prepared)
			require.NoError(t, err)
			_, err = io.Copy(io.Discard, response.Body)
			require.NoError(t, err)
			require.NoError(t, response.Body.Close())
			usage := &database.UsageLogInput{AccountID: 1712, StatusCode: 400}
			populateUpstreamTrace(request, usage)
			populateUsageRequestDiagnostics(request, usage)
			detail := readUsageDiagnosticSnapshot(t, usage)
			require.Equal(t, captureAccessPrograms(body), detail.AccessPrograms.Inbound)
			require.Equal(t, captureAccessPrograms(<-seen), detail.AccessPrograms.Outbound)
			require.Equal(t, tc.want, string(detail.AccessPrograms.Outbound.Value))
			require.Equal(t, "unsupported_parameter", detail.Upstream.ErrorCode)
			require.Equal(t, "invalid_request_error", detail.Upstream.ErrorType)
			require.Equal(t, 400, response.StatusCode)
		})
	}
}

func TestAccessProgramsAttemptsAndFrameIsolation(t *testing.T) {
	request := transportTestContext()
	body := []byte(`{"access_programs":{"cyber":"daybreak_blue"},"input":[]}`)
	captureUsageRequestIngress(request, body)
	beginUpstreamTrace(request.Request.Context(), &auth.Account{DBID: 17}, "", true)
	oldObserver := UpstreamTransportObserver(request.Request.Context())
	oldObserver.ResponsesInput(body, nil, "")
	first := snapshotUpstreamTrace(request.Request.Context())
	beginUsageSelectionAttempt(request, 2)
	beginUpstreamTrace(request.Request.Context(), &auth.Account{DBID: 18}, "", true)
	oldObserver.ResponsesInput([]byte(`{"access_programs":"stale"}`), nil, "")
	usage := &database.UsageLogInput{AccountID: 18}
	populateUpstreamTrace(request, usage)
	populateUsageRequestDiagnostics(request, usage)
	require.Nil(t, readUsageDiagnosticSnapshot(t, usage).AccessPrograms.Outbound, "not captured must not mean absent or reuse a prior attempt")
	UpstreamTransportObserver(request.Request.Context()).ResponsesInput([]byte(`{"type":"response.create","input":[]}`), nil, "")
	usage = &database.UsageLogInput{AccountID: 18}
	populateUpstreamTrace(request, usage)
	populateUsageRequestDiagnostics(request, usage)
	detail := readUsageDiagnosticSnapshot(t, usage)
	require.Equal(t, captureAccessPrograms(body), detail.AccessPrograms.Inbound)
	require.Equal(t, "absent", detail.AccessPrograms.Outbound.State)
	require.Equal(t, 2, detail.Attempt)
	require.Equal(t, captureAccessPrograms(body), first.Transport.AccessPrograms)
	resetPromptPolicyRequestCorrelationID(request)
	resetCodexInternalRequestClassificationFrame(request)
	resetUpstreamRequestTrace(request)
	captureUsageRequestIngress(request, []byte(`{"access_programs":null,"input":[]}`))
	usage = &database.UsageLogInput{}
	populateUsageRequestDiagnostics(request, usage)
	detail = readUsageDiagnosticSnapshot(t, usage)
	require.Equal(t, "null", string(detail.AccessPrograms.Inbound.Value))
	require.Nil(t, detail.AccessPrograms.Outbound)
}

func TestAccessProgramsLargeDiagnosticPreservesIngressAndEgress(t *testing.T) {
	request := transportTestContext()
	body := []byte(`{"access_programs":{"cyber":"daybreak_blue"},"input":[]}`)
	captureUsageRequestIngress(request, body)
	largeValue := strings.Repeat("x", 12*1024)
	usageRequestDiagnosticState(request).Incoming["oversized"] = map[string]string{"value": largeValue}
	beginUpstreamTrace(request.Request.Context(), &auth.Account{DBID: 17}, "", true)
	observer := UpstreamTransportObserver(request.Request.Context())
	observer.ResponsesInput(body, nil, "")
	observer.updateOutboundIdentity(func(identity *outboundIdentityDiagnostic) {
		identity.HTTP = &OutboundHeaderDiagnostic{Headers: map[string]string{"User-Agent": largeValue}}
	})
	usage := &database.UsageLogInput{AccountID: 17}
	populateUpstreamTrace(request, usage)
	populateUsageRequestDiagnostics(request, usage)
	require.Greater(t, len(usage.RequestDiagnostics), 24*1024)
	detail := readUsageDiagnosticSnapshot(t, usage)
	require.False(t, detail.Truncated)
	require.Equal(t, largeValue, detail.Incoming["oversized"]["value"])
	require.False(t, detail.Upstream.OutboundIdentity.Truncated)
	require.Equal(t, largeValue, detail.Upstream.OutboundIdentity.HTTP.Headers["User-Agent"])
	require.Equal(t, captureAccessPrograms(body), detail.AccessPrograms.Inbound)
	require.Equal(t, detail.AccessPrograms.Inbound, detail.AccessPrograms.Outbound)
}
