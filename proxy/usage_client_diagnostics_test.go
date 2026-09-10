package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"net/http"
	"reflect"
	"strings"
	"testing"

	"github.com/codex2api/database"
)

func TestUsageRequestDiagnosticsDeviceSourcesAndTransport(test *testing.T) {
	ctx := promptSessionLimitTestContext(testRootSessionA)
	ctx.Request.Header.Set("X-Codex-Installation-Id", testRootSessionA)
	ctx.Request.Header.Set("X-Device-Id", "private-custom-device")
	ctx.Request.Header.Set("User-Agent", "codex-tui/0.153.3 (Windows 11; x86_64)")
	ctx.Request.Header.Set("Originator", "codex-tui")
	ctx.Request.Header.Set("X-Stainless-OS", "Windows")
	ctx.Request.Header.Set("X-Codex-Turn-Metadata", `{"installation_id":"`+testLeafSessionA+`","context_window_id":"`+testLeafSessionA+`","window_number":0,"turn_started_at_unix_ms":1789030800000,"workspaces":{"private-path":{}},"project_id":"private-project"}`)
	body := []byte(`{"model":"gpt-6-astra","input":"private-prompt","client_metadata":{"installationId":"` + testRootSessionA + `","device_id":true,"client_version":"0.153.3","os_name":"Windows 11","timezone":"America/New_York","x-codex-turn-metadata":{"installation_id":"` + testLeafSessionA + `"}}}`)
	original := append([]byte(nil), body...)
	headers := ctx.Request.Header.Clone()
	handler := &Handler{}
	policy := verifiedNewAPIPolicyContext{MetaVerified: true, Platform: "test-platform", Meta: newAPIPolicyMeta{InstallationID: testRootSessionA, TokenID: 22, ChannelID: 18}}
	handler.captureUsageRequestResolution(ctx, body, requestSessionIdentity{}, requestRootSessionIdentity{}, policy, "verified")
	attachUserAgentAudit(ctx)
	RecordUpstreamUserAgent(ctx.Request.Context(), "codex-tui/0.153.3 (Linux; x86_64)")
	input := &database.UsageLogInput{Model: "gpt-6-astra", EffectiveModel: "gpt-6-astra", RequestID: testRootSessionA, APIKeyID: 7, APIKeyName: "main key", Stream: true, ViaWebsocket: true}
	populateUsageRequestDiagnostics(ctx, input)
	snapshot := readUsageDiagnosticSnapshot(test, input)
	if snapshot.Incoming["headers"]["X-Codex-Installation-Id"] != testRootSessionA || snapshot.Incoming["turn_metadata_header"]["installation_id"] != testLeafSessionA || snapshot.Incoming["signed_newapi"]["installation_id"] != testRootSessionA {
		test.Fatalf("device sources were lost or merged: %+v", snapshot.Incoming)
	}
	if snapshot.Incoming["turn_metadata_header"]["window_number"] != "0" || snapshot.Incoming["turn_metadata_header"]["context_window_id"] != testLeafSessionA || snapshot.Incoming["turn_metadata_header"]["turn_started_at_unix_ms"] != "1789030800000" {
		test.Fatalf("missing context-window metadata: %+v", snapshot.Incoming)
	}
	if snapshot.Incoming["client_metadata"]["device_id"] != "invalid_type" || snapshot.Incoming["signed_newapi"]["token_id"] != "22" || snapshot.Incoming["signed_newapi"]["channel_id"] != "18" {
		test.Fatalf("invalid metadata handling: %+v", snapshot.Incoming)
	}
	info := snapshot.Request
	if snapshot.CaptureStatus != "captured" || info == nil || info.Transport != "http" || !info.UpstreamViaWS || !info.Stream || info.APIKeyID != 7 || info.UserAgentOverridden == nil || !*info.UserAgentOverridden || info.ClientUserAgent == info.UpstreamUserAgent {
		test.Fatalf("incorrect transport/client snapshot: %+v", info)
	}
	for _, private := range []string{"private-custom-device", "private-path", "private-project", "private-prompt"} {
		if strings.Contains(input.RequestDiagnostics, private) {
			test.Errorf("leaked %s", private)
		}
	}
	if !bytes.Equal(body, original) || !reflect.DeepEqual(headers, ctx.Request.Header) {
		test.Fatal("diagnostic capture changed forwarded metadata")
	}
}

func TestUsageRequestDiagnosticsDeviceWSFramesDoNotInherit(test *testing.T) {
	ctx := promptSessionLimitTestContext(testRootSessionA)
	ctx.Request.Method = http.MethodGet
	ctx.Request.Header.Set("Upgrade", "websocket")
	ctx.Request.Header.Set("Connection", "Upgrade")
	ctx.Request.Header.Set("X-Codex-Installation-Id", testRootSessionA)
	attachUserAgentAudit(ctx)
	captureUsageRequestIngress(ctx, []byte(`{"client_metadata":{"installation_id":"`+testLeafSessionA+`"}}`))
	input := &database.UsageLogInput{}
	populateUsageRequestDiagnostics(ctx, input)
	first := input.RequestDiagnostics
	resetPromptPolicyRequestCorrelationID(ctx)
	resetCodexInternalRequestClassificationFrame(ctx)
	resetUpstreamUserAgentAudit(ctx.Request.Context())
	captureUsageRequestIngress(ctx, []byte(`{"client_metadata":{"thread_source":"user"}}`))
	populateUsageRequestDiagnostics(ctx, input)
	snapshot := readUsageDiagnosticSnapshot(test, input)
	if strings.Contains(input.RequestDiagnostics, testLeafSessionA) || snapshot.Incoming["client_metadata"]["installation_id"] != "" || snapshot.Incoming["headers"]["X-Codex-Installation-Id"] != testRootSessionA {
		test.Fatalf("frame inherited a previous body device or lost handshake provenance: %+v", snapshot.Incoming)
	}
	if snapshot.Request.Transport != "websocket" || snapshot.Request.UserAgentOverridden != nil || !strings.Contains(first, testLeafSessionA) {
		test.Fatalf("unknown upstream was inferred or original snapshot changed: %+v", snapshot.Request)
	}
}

func TestUsageRequestDiagnosticsDevicePrivacyAndBounds(test *testing.T) {
	ctx := promptSessionLimitTestContext("")
	ctx.Request.Header.Set("Authorization", "Bearer plain-secret-value")
	ctx.Request.Header.Set("User-Agent", "codex-tui plain-secret-value "+strings.Repeat("x", 4096))
	ctx.Request.Header.Set("Cookie", "session=private-cookie")
	ctx.Request.Header.Set("X-Stainless-Runtime", "node")
	ctx.Request.Header.Set("X-Stainless-Runtime-Version", "24.0.0")
	body := []byte(`{"client_metadata":{"installation_id":"sk-secret-device-id","client_version":"sk-secret-version","turn_started_at_unix_ms":"wrong-type","workspaces":{"private-path":{}},"access_token":"private-token"}}`)
	captureUsageRequestIngress(ctx, body)
	input := &database.UsageLogInput{}
	populateUsageRequestDiagnostics(ctx, input)
	for _, secret := range []string{"plain-secret-value", "private-cookie", "sk-secret-device-id", "sk-secret-version", "private-path", "private-token"} {
		if strings.Contains(input.RequestDiagnostics, secret) {
			test.Errorf("leaked %q", secret)
		}
	}
	if len(input.RequestDiagnostics) > database.MaxUsageRequestDiagnosticsBytes || !json.Valid([]byte(input.RequestDiagnostics)) {
		test.Fatal("device diagnostics escaped snapshot bounds")
	}
	snapshot := readUsageDiagnosticSnapshot(test, input)
	if len(snapshot.Incoming["headers"]["User-Agent"]) > 600 || snapshot.Incoming["client_metadata"]["turn_started_at_unix_ms"] != "invalid_type" {
		test.Fatalf("unsafe field limits: %+v", snapshot.Incoming)
	}
}

func TestUsageRequestDiagnosticsClaudeDeviceMetadata(test *testing.T) {
	ctx := promptSessionLimitTestContext("")
	ctx.Request.URL.Path = "/v1/messages"
	userID, _ := json.Marshal(map[string]string{"device_id": "private-claude-device", "account_uuid": testRootSessionA, "session_id": testLeafSessionA, "email": "private@example.com"})
	body, _ := json.Marshal(map[string]any{"metadata": map[string]string{"user_id": string(userID)}})
	captureUsageRequestIngress(ctx, body)
	input := &database.UsageLogInput{}
	populateUsageRequestDiagnostics(ctx, input)
	snapshot := readUsageDiagnosticSnapshot(test, input)
	metadata := snapshot.Incoming["metadata.user_id"]
	if metadata["account_uuid"] != testRootSessionA || metadata["session_id"] != testLeafSessionA || !strings.HasPrefix(metadata["device_id"], "hash:") || strings.Contains(input.RequestDiagnostics, "private") {
		test.Fatalf("unsafe Claude metadata: %+v", metadata)
	}
}

func TestUsageDiagnosticClientInfoBounded(test *testing.T) {
	incoming := make(map[string]map[string]string)
	for index := 0; index < 10; index++ {
		incoming[fmt.Sprint(index)] = map[string]string{"installation_id": testRootSessionA, "client_name": strings.Repeat("x", 500), "client_version": "0.1"}
	}
	info := usageDiagnosticClientInfo(incoming)
	bytes := 0
	for key, value := range info {
		bytes += len(key) + len(value)
	}
	if bytes > 4096 || len(info) > 24 || len(info) == 0 {
		test.Fatalf("service client info unbounded: entries=%d bytes=%d", len(info), bytes)
	}
}
