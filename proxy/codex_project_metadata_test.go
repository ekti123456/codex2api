package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestStripCodexProjectMetadata(test *testing.T) {
	canonical := `{"thread_id":"thread","window_id":"thread:71","window_number":71,"project_id":"project","projectId":null,"workspace_id":{"id":1},"workspaces":{"/project":{"has_changes":true}}}`
	for _, carrierName := range []string{"x-codex-turn-metadata", "x_codex_turn_metadata"} {
		for _, objectCarrier := range []bool{false, true} {
			var carrier any = canonical
			if objectCarrier {
				carrier = json.RawMessage(canonical)
			}
			body, err := json.Marshal(map[string]any{
				"client_metadata": map[string]any{
					"project_id": "flat", "projectId": true, "workspace_id": []string{"workspace"},
					"session_id": "root", "thread_id": "thread", carrierName: carrier,
				},
				"input": []any{map[string]any{"type": "function_call", "arguments": `{"project_id":"business-project"}`}},
				"tools": []any{map[string]any{"parameters": map[string]any{"properties": map[string]any{"workspace_id": map[string]any{"type": "string"}}}}},
			})
			require.NoError(test, err)
			original := bytes.Clone(body)
			cleaned := StripCodexProjectMetadata(body)
			require.True(test, gjson.ValidBytes(cleaned))
			require.Equal(test, original, body)
			embedded := gjson.GetBytes(cleaned, "client_metadata."+carrierName)
			require.Equal(test, objectCarrier, embedded.IsObject())
			for _, field := range []string{"project_id", "projectId", "workspace_id"} {
				require.False(test, gjson.GetBytes(cleaned, "client_metadata."+field).Exists())
				require.False(test, gjson.Get(embedded.String(), field).Exists())
			}
			require.Equal(test, "root", gjson.GetBytes(cleaned, "client_metadata.session_id").String())
			require.Equal(test, "thread:71", gjson.Get(embedded.String(), "window_id").String())
			require.Equal(test, int64(71), gjson.Get(embedded.String(), "window_number").Int())
			require.Equal(test, gjson.Get(canonical, "workspaces").Raw, gjson.Get(embedded.String(), "workspaces").Raw)
			require.Equal(test, gjson.GetBytes(body, "input").Raw, gjson.GetBytes(cleaned, "input").Raw)
			require.Equal(test, gjson.GetBytes(body, "tools").Raw, gjson.GetBytes(cleaned, "tools").Raw)
			require.Equal(test, cleaned, StripCodexProjectMetadata(cleaned))
		}
	}
	duplicate := []byte(`{"client_metadata":{"project_id":"first","\u0070roject_id":"second","projectId":1,"workspace_id":null,"thread_id":"keep"}}`)
	require.JSONEq(test, `{"client_metadata":{"thread_id":"keep"}}`, string(StripCodexProjectMetadata(duplicate)))
	escaped := []byte(`{"client_metadata":{"\u0070roject_\u0069d":"private"}}`)
	require.JSONEq(test, `{"client_metadata":{}}`, string(StripCodexProjectMetadata(escaped)))
}

func TestStripCodexProjectMetadataPreservesUnchangedBody(test *testing.T) {
	for _, raw := range []string{
		`{ "model": "gpt-6-astra", "input": [{"project_id":"business-data"}] }`,
		`{"client_metadata":{"session_id":"root","x-codex-turn-metadata":"{\"thread_id\":\"thread\"}"}}`,
		`{"client_metadata":{"x-codex-turn-metadata":"not-json"}}`,
		`{"client_metadata":null}`,
		`{"client_metadata":[]}`,
	} {
		body := []byte(raw)
		cleaned := StripCodexProjectMetadata(body)
		require.Equal(test, raw, string(cleaned))
		require.Same(test, &body[0], &cleaned[0])
	}
	require.Nil(test, StripCodexProjectMetadata(nil))
}

func TestStripCodexProjectMetadataHeaders(test *testing.T) {
	values := []string{`{"project_id":"one","thread_id":"thread"}`, `{"projectId":"two","workspace_id":"workspace","window_number":71}`}
	original := append([]string(nil), values...)
	headers := http.Header{
		"x-codex-turn-metadata": values,
		"X-Codex-Project-Id":    []string{"one"},
		"x-codex-workspace-id":  []string{"workspace"},
		"OpenAI-Project":        []string{"api-project"},
		"X-Codex-Window-Id":     []string{"thread:71"},
	}
	StripCodexProjectMetadataHeaders(headers)
	require.Equal(test, original, values)
	require.NotContains(test, headers, "X-Codex-Project-Id")
	require.NotContains(test, headers, "x-codex-workspace-id")
	require.Equal(test, []string{"api-project"}, headers["OpenAI-Project"])
	require.Equal(test, "thread:71", headers.Get("X-Codex-Window-Id"))
	require.JSONEq(test, `{"thread_id":"thread"}`, headers["x-codex-turn-metadata"][0])
	require.JSONEq(test, `{"window_number":71}`, headers["x-codex-turn-metadata"][1])
	unchanged := headers.Clone()
	StripCodexProjectMetadataHeaders(headers)
	require.Equal(test, unchanged, headers)
	StripCodexProjectMetadataHeaders(nil)
}

func projectMetadataRequestFixture(test *testing.T) (http.Header, []byte) {
	test.Helper()
	headers, body := fingerprintMetadataFixture(test, "root", "thread", "parent", "fork", 71)
	canonical := headers.Get(codexTurnMetadataHeader)
	for _, field := range []string{"project_id", "projectId", "workspace_id"} {
		var err error
		canonical, err = sjson.Set(canonical, field, "private-project")
		require.NoError(test, err)
		body, err = sjson.SetBytes(body, "client_metadata."+field, "private-project")
		require.NoError(test, err)
	}
	headers.Set(codexTurnMetadataHeader, canonical)
	headers.Set("OpenAI-Project", "api-project")
	body, err := sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata", canonical)
	require.NoError(test, err)
	return headers, body
}

func requireNoCodexProjectMetadata(test *testing.T, body []byte, headers http.Header) {
	test.Helper()
	metadata := gjson.GetBytes(body, "client_metadata")
	for _, field := range []string{"project_id", "projectId", "workspace_id"} {
		require.False(test, metadata.Get(field).Exists(), field)
		require.False(test, gjson.Get(metadata.Get("x-codex-turn-metadata").String(), field).Exists(), field)
		require.False(test, gjson.Get(headers.Get(codexTurnMetadataHeader), field).Exists(), field)
	}
	require.Empty(test, headers.Get("X-Codex-Project-Id"))
	require.Empty(test, headers.Get("X-Codex-Workspace-Id"))
}

func TestCodexProjectMetadataStrippedFromHTTPAndCompact(test *testing.T) {
	previousResin := GetResinConfig()
	test.Cleanup(func() { SetResinConfig(previousResin) })
	type capture struct {
		headers http.Header
		body    []byte
	}
	received := make(chan capture, 1)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		received <- capture{request.Header.Clone(), readUpstreamRequestBody(request)}
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"id":"resp_test"}`))
	}))
	test.Cleanup(server.Close)
	SetResinConfig(&ResinConfig{BaseURL: server.URL, PlatformName: "project-metadata-test"})
	for _, mode := range []string{auth.CodexFingerprintModeOff, auth.CodexFingerprintModeDevice, auth.CodexFingerprintModeSession, auth.CodexFingerprintModeFull} {
		test.Run(mode, func(test *testing.T) {
			for _, compact := range []bool{false, true} {
				headers, body := projectMetadataRequestFixture(test)
				originalBody, originalHeaders := bytes.Clone(body), headers.Clone()
				account := &auth.Account{DBID: 42002, AccessToken: "dummy-token", CodexFingerprintMode: mode, CustomHeaders: map[string]string{
					codexTurnMetadataHeader: `{"project_id":"custom","projectId":"custom","workspace_id":"custom","thread_id":"custom-thread"}`,
					"X-Codex-Project-Id":    "custom", "X-Codex-Workspace-Id": "custom", "OpenAI-Project": "api-project",
				}}
				var response *http.Response
				var err error
				if compact {
					response, err = ExecuteCompactRequest(context.Background(), account, body, "cache-key", "", "key", nil, headers)
				} else {
					response, err = ExecuteRequest(context.Background(), account, body, "cache-key", "", "key", nil, headers, false)
				}
				require.NoError(test, err)
				_, err = io.Copy(io.Discard, response.Body)
				require.NoError(test, err)
				require.NoError(test, response.Body.Close())
				sent := <-received
				requireNoCodexProjectMetadata(test, sent.body, sent.headers)
				require.Equal(test, "api-project", sent.headers.Get("OpenAI-Project"))
				require.Equal(test, "custom-thread", gjson.Get(sent.headers.Get(codexTurnMetadataHeader), "thread_id").String())
				require.Equal(test, int64(71), gjson.Get(gjson.GetBytes(sent.body, "client_metadata.x-codex-turn-metadata").String(), "window_number").Int())
				require.Equal(test, originalBody, body)
				require.Equal(test, originalHeaders, headers)
			}
		})
	}
}

func TestCodexProjectMetadataStrippedFromResponsesRelay(test *testing.T) {
	for _, compact := range []bool{false, true} {
		type capture struct {
			headers http.Header
			body    []byte
		}
		received := make(chan capture, 1)
		server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
			received <- capture{request.Header.Clone(), readUpstreamRequestBody(request)}
			writer.Header().Set("Content-Type", "application/json")
			_, _ = writer.Write([]byte(`{"id":"resp_relay"}`))
		}))
		test.Cleanup(server.Close)
		headers, body := projectMetadataRequestFixture(test)
		originalHeaders, originalBody := headers.Clone(), bytes.Clone(body)
		account := &auth.Account{DBID: 42003, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: server.URL, APIKey: "relay-token", CustomHeaders: map[string]string{
			codexTurnMetadataHeader: headers.Get(codexTurnMetadataHeader), "X-Codex-Project-Id": "custom-project",
		}}
		var response *http.Response
		var err error
		if compact {
			response, err = ExecuteOpenAIResponsesCompactRequest(context.Background(), account, body, "", headers)
		} else {
			response, err = ExecuteOpenAIResponsesRequest(context.Background(), account, body, "", headers)
		}
		require.NoError(test, err)
		_, err = io.Copy(io.Discard, response.Body)
		require.NoError(test, err)
		require.NoError(test, response.Body.Close())
		sent := <-received
		requireNoCodexProjectMetadata(test, sent.body, sent.headers)
		require.Equal(test, "api-project", sent.headers.Get("OpenAI-Project"))
		require.Equal(test, "thread", gjson.GetBytes(sent.body, "client_metadata.thread_id").String())
		require.Equal(test, originalHeaders, headers)
		require.Equal(test, originalBody, body)
	}
}

func TestCodexProjectMetadataStrippedFromRelayRetry(test *testing.T) {
	type capture struct {
		headers http.Header
		body    []byte
	}
	received := make(chan capture, 2)
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		body := readUpstreamRequestBody(request)
		received <- capture{request.Header.Clone(), body}
		writer.Header().Set("Content-Type", "application/json")
		if codexClientInstallationID(body) == "" {
			writer.WriteHeader(http.StatusForbidden)
			_, _ = writer.Write([]byte(`{"error":{"code":"codex_access_restricted"}}`))
			return
		}
		_, _ = writer.Write([]byte(`{"id":"resp_retry"}`))
	}))
	test.Cleanup(server.Close)
	account := &auth.Account{
		DBID: 42004, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: server.URL, APIKey: "relay-token",
		CodexPassthroughMode: auth.CodexPassthroughModeOff, CodexClientMetadataMode: auth.CodexClientMetadataModeAuto,
	}
	capabilityKey := openAIResponsesCodexMetadataCapabilityKey(account, server.URL)
	test.Cleanup(func() { openAIResponsesCodexMetadataRequired.Delete(capabilityKey) })
	body := []byte(`{"model":"gpt-6-astra","client_metadata":{"project_id":"private","workspace_id":"workspace","x-codex-turn-metadata":"{\"projectId\":\"project\",\"thread_id\":\"thread\"}"}}`)
	response, err := ExecuteOpenAIResponsesRequest(context.Background(), account, body, "", nil)
	require.NoError(test, err)
	require.Equal(test, http.StatusOK, response.StatusCode)
	_, err = io.Copy(io.Discard, response.Body)
	require.NoError(test, err)
	require.NoError(test, response.Body.Close())
	require.Len(test, received, 2)
	initial, retry := <-received, <-received
	requireNoCodexProjectMetadata(test, initial.body, initial.headers)
	requireNoCodexProjectMetadata(test, retry.body, retry.headers)
	require.Empty(test, codexClientInstallationID(initial.body))
	require.NotEmpty(test, codexClientInstallationID(retry.body))
}

func BenchmarkStripCodexProjectMetadata(benchmark *testing.B) {
	for _, sample := range []struct{ name, body string }{
		{"no_metadata", `{"input":"` + strings.Repeat("x", 1024*1024) + `"}`},
		{"unchanged", `{"input":"` + strings.Repeat("x", 1024*1024) + `","client_metadata":{"x-codex-turn-metadata":"{\"thread_id\":\"thread\",\"window_number\":71}"}}`},
		{"strip", `{"client_metadata":{"project_id":"remove","x-codex-turn-metadata":"{\"workspace_id\":\"remove\",\"thread_id\":\"thread\"}"}}`},
	} {
		benchmark.Run(sample.name, func(benchmark *testing.B) {
			body := []byte(sample.body)
			benchmark.SetBytes(int64(len(body)))
			benchmark.ReportAllocs()
			benchmark.ResetTimer()
			for iteration := 0; iteration < benchmark.N; iteration++ {
				_ = StripCodexProjectMetadata(body)
			}
		})
	}
}
