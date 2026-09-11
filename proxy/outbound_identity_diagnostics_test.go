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

func TestOutboundIdentityHeadersPrivacyAndBounds(test *testing.T) {
	headers := http.Header{}
	headers.Set("User-Agent", "codex-tui/0.154.0")
	headers.Set("Originator", "codex-tui")
	headers.Set("Version", "0.154.0")
	headers.Set("X-Codex-Installation-Id", "31811466-ec40-4690-b07b-b4828d3095ff")
	headers.Add("Session-Id", "private-session")
	headers.Add("Session-Id", "private-second-session")
	headers.Set("Authorization", "Bearer private-access-token")
	headers.Set("Cookie", "private-cookie")
	headers.Set("X-Oai-Attestation", "private-attestation")
	headers.Set("X-Custom-Header", "private-custom-value")
	headers.Set(codexTurnMetadataHeader, `{"installation_id":"31811466-ec40-4690-b07b-b4828d3095ff","request_kind":"compaction","prompt":"private-prompt","token":"private-token"}`)
	captured := CaptureOutboundIdentityHeaders(headers)
	require.Equal(test, "codex-tui/0.154.0", captured.Headers["User-Agent"])
	require.Equal(test, "31811466-ec40-4690-b07b-b4828d3095ff", captured.Headers["X-Codex-Installation-Id"])
	require.Equal(test, "compaction", captured.TurnMetadata["request_kind"])
	require.Equal(test, "true", captured.Headers["Session-Id_multiple"])
	require.True(test, strings.HasPrefix(captured.Headers["Session-Id"], "hash:"))
	encoded, err := json.Marshal(captured)
	require.NoError(test, err)
	require.NotContains(test, string(encoded), "private-")
	headers.Set(codexTurnMetadataHeader, strings.Repeat("private-metadata", 2000))
	headers.Set("User-Agent", "sk-private-key")
	captured = CaptureOutboundIdentityHeaders(headers)
	require.Equal(test, "too_large", captured.TurnMetadata["metadata_status"])
	require.True(test, strings.HasPrefix(captured.Headers["User-Agent"], "hash:"))
}

func TestOutboundIdentityHTTPRecordsFinalOverridesAndBody(test *testing.T) {
	var receivedHeaders http.Header
	var receivedBody []byte
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		receivedHeaders = request.Header.Clone()
		receivedBody, _ = io.ReadAll(request.Body)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"id":"response-test"}`))
	}))
	defer server.Close()
	request := transportTestContext()
	const incomingDevice = "31811466-ec40-4690-b07b-b4828d3095ff"
	const customHeaderDevice = "41811466-ec40-4690-b07b-b4828d3095ff"
	body := []byte(`{"model":"gpt-5.6-sol","client_metadata":{"x-codex-installation-id":"` + incomingDevice + `","x-codex-turn-metadata":{"installation_id":"` + incomingDevice + `","request_kind":"compaction"}},"input":[{"role":"user","content":"private-input"}]}`)
	downstream := http.Header{}
	downstream.Set("X-Codex-Installation-Id", incomingDevice)
	account := &auth.Account{DBID: 1695, AccessToken: "private-token", CodexFingerprintMode: auth.CodexFingerprintModeDevice,
		CustomHeaders: map[string]string{"X-Codex-Installation-Id": customHeaderDevice, "User-Agent": "custom-client/1", "Originator": "custom-client", "Version": "1"}}
	fingerprint := NewCodexFingerprint(account, downstream, body)
	prepared := fingerprint.ApplyBody(body)
	outgoing, err := http.NewRequestWithContext(request.Request.Context(), http.MethodPost, server.URL+"/responses", strings.NewReader(string(prepared)))
	require.NoError(test, err)
	applyCodexRequestHeaders(outgoing, account, account.AccessToken, "session", "key", nil, downstream, fingerprint)
	captureUsageRequestIngress(request, body)
	response, err := doTracedUpstreamRequest(server.Client(), outgoing, account, "", prepared)
	require.NoError(test, err)
	require.NoError(test, response.Body.Close())
	usage := &database.UsageLogInput{AccountID: account.DBID, StatusCode: 200}
	populateUpstreamTrace(request, usage)
	populateUsageRequestDiagnostics(request, usage)
	for _, name := range []string{"User-Agent", "Originator", "Version", "X-Codex-Installation-Id"} {
		require.Equal(test, receivedHeaders.Get(name), gjson.Get(usage.RequestDiagnostics, "upstream.outbound_identity.http.headers."+name).String())
	}
	actualDevice := gjson.GetBytes(receivedBody, "client_metadata.x-codex-installation-id").String()
	require.NotEqual(test, incomingDevice, actualDevice)
	require.Equal(test, customHeaderDevice, actualDevice)
	require.Equal(test, actualDevice, gjson.Get(usage.RequestDiagnostics, "upstream.outbound_identity.body.client_metadata.x-codex-installation-id").String())
	require.Equal(test, actualDevice, gjson.Get(usage.RequestDiagnostics, "upstream.outbound_identity.body.turn_metadata.installation_id").String())
	require.NotContains(test, usage.RequestDiagnostics, "private-")
}

func TestOutboundIdentityDeviceValuesAreAccountScoped(test *testing.T) {
	body := []byte(`{"client_metadata":{"x-codex-installation-id":"31811466-ec40-4690-b07b-b4828d3095ff"},"input":[]}`)
	devices := make(map[string]bool)
	for _, accountID := range []int64{1691, 1707} {
		account := &auth.Account{DBID: accountID, CodexFingerprintMode: auth.CodexFingerprintModeDevice}
		prepared := NewCodexFingerprint(account, nil, body).ApplyBody(body)
		diagnostic := captureOutboundIdentityBody(prepared)
		device := diagnostic.ClientMetadata["x-codex-installation-id"]
		require.True(test, validSessionGraphUUID(device))
		require.False(test, devices[device])
		devices[device] = true
	}
}

func TestOutboundIdentityDoesNotHideHeaderBodyMismatch(test *testing.T) {
	request := transportTestContext()
	beginUpstreamTrace(request.Request.Context(), &auth.Account{DBID: 17}, "", false)
	observer := UpstreamTransportObserver(request.Request.Context())
	headers := http.Header{}
	headers.Set("X-Codex-Installation-Id", "31811466-ec40-4690-b07b-b4828d3095ff")
	observer.OutboundHTTPIdentity(headers)
	observer.ResponsesInput([]byte(`{"client_metadata":{"x-codex-installation-id":"41811466-ec40-4690-b07b-b4828d3095ff"},"input":[]}`), headers, "/responses")
	identity := snapshotUpstreamTrace(request.Request.Context()).Transport.OutboundIdentity
	require.Equal(test, "31811466-ec40-4690-b07b-b4828d3095ff", identity.HTTP.Headers["X-Codex-Installation-Id"])
	require.Equal(test, "41811466-ec40-4690-b07b-b4828d3095ff", identity.Body.ClientMetadata["x-codex-installation-id"])
}

func TestOutboundIdentityWebsocketHandshakeAndFrameSnapshots(test *testing.T) {
	request := transportTestContext()
	account := &auth.Account{DBID: 17}
	beginUpstreamTrace(request.Request.Context(), account, "", true)
	observer := UpstreamTransportObserver(request.Request.Context())
	stored := CaptureOutboundIdentityHeaders(http.Header{"User-Agent": {"original-client/1"}})
	observer.OutboundWebsocketHandshake(stored)
	observer.ResponsesInput([]byte(`{"client_metadata":{"x-codex-turn-metadata":{"window_number":1}},"input":[]}`), nil, "")
	first := snapshotUpstreamTrace(request.Request.Context())
	observer.ResponsesInput([]byte(`{"client_metadata":{"x-codex-turn-metadata":{"window_number":2}},"input":[]}`), nil, "")
	require.Equal(test, "1", first.Transport.OutboundIdentity.Body.TurnMetadata["window_number"])
	require.Equal(test, "2", snapshotUpstreamTrace(request.Request.Context()).Transport.OutboundIdentity.Body.TurnMetadata["window_number"])
	stored.Headers["User-Agent"] = "not-actually-sent/2"
	require.Equal(test, "original-client/1", first.Transport.OutboundIdentity.WSHandshake.Headers["User-Agent"])
	beginUpstreamTrace(request.Request.Context(), &auth.Account{DBID: 18}, "", false)
	observer.OutboundWebsocketHandshake(stored)
	observer.ResponsesInput([]byte(`{"input":[]}`), nil, "")
	require.Nil(test, snapshotUpstreamTrace(request.Request.Context()).Transport.OutboundIdentity)
}

func TestOutboundIdentityOversizeDoesNotEraseRequestDiagnostic(test *testing.T) {
	request := transportTestContext()
	state := usageRequestDiagnosticState(request)
	state.Resolved = &usageRequestResolution{ThreadSource: "user", RootState: "resolved"}
	metadata := make(map[string]string)
	for _, name := range []string{"client_name", "client_version", "os_name", "os_version", "arch", "timezone"} {
		metadata[name] = strings.Repeat("client-value", 42)
	}
	body, err := json.Marshal(map[string]any{"client_metadata": map[string]any{"x-codex-turn-metadata": metadata, "client_name": metadata["client_name"], "client_version": metadata["client_version"], "os_name": metadata["os_name"], "os_version": metadata["os_version"], "arch": metadata["arch"], "timezone": metadata["timezone"]}, "input": []any{}})
	require.NoError(test, err)
	encodedMetadata, err := json.Marshal(metadata)
	require.NoError(test, err)
	headers := http.Header{}
	headers.Set(codexTurnMetadataHeader, string(encodedMetadata))
	for _, name := range []string{"User-Agent", "Originator", "Version", "OpenAI-Beta", "X-Codex-Beta-Features"} {
		headers.Set(name, strings.Repeat("client", 84))
	}
	beginUpstreamTrace(request.Request.Context(), &auth.Account{DBID: 17}, "", false)
	observer := UpstreamTransportObserver(request.Request.Context())
	observer.OutboundHTTPIdentity(headers)
	observer.ResponsesInput(body, headers, "/responses")
	usage := &database.UsageLogInput{AccountID: 17, StatusCode: 500}
	populateUpstreamTrace(request, usage)
	populateUsageRequestDiagnostics(request, usage)
	require.NotEmpty(test, usage.RequestDiagnostics)
	require.LessOrEqual(test, len(usage.RequestDiagnostics), database.MaxUsageRequestDiagnosticsBytes)
	require.Equal(test, "user", gjson.Get(usage.RequestDiagnostics, "resolved.thread_source").String())
	require.True(test, gjson.Get(usage.RequestDiagnostics, "truncated").Bool())
	require.True(test, gjson.Get(usage.RequestDiagnostics, "upstream.outbound_identity.truncated").Bool())
	require.False(test, snapshotUpstreamTrace(request.Request.Context()).Transport.OutboundIdentity.Truncated)
}
