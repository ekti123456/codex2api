package proxy

import (
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

func TestOutboundSchemaOfficialMetadataAndStringEncoding(t *testing.T) {
	account := &auth.Account{DBID: 42, AccessToken: "test-account-key"}
	const inventory = `{"functions":{"name":"functions","functions":{"exec":{"name":"exec","direct":true,"code_mode_name":null,"deferred":false,"source":{"kind":"harness","session_id":"private"}},"lookup":{"name":"lookup","direct":false,"code_mode_name":"mcp.lookup","deferred":true,"source":{"kind":"mcp","server_name":"mcp"}}}}}`
	const expected = `{"functions":{"name":"functions","functions":{"exec":{"name":"exec","direct":true,"code_mode_name":null,"deferred":false,"source":{"kind":"harness"}},"lookup":{"name":"lookup","direct":false,"code_mode_name":"mcp.lookup","deferred":true,"source":{"kind":"mcp","server_name":"mcp"}}}}}`
	for _, encoded := range []bool{false, true} {
		body := []byte(`{"model":"actual-model","reasoning":{"effort":"high"},"input":[{"role":"user","content":"session_id user content stays"}],"client_metadata":{"auto_review_enabled":"false","model":"stale-flat","reasoning_effort":"low","x-codex-turn-metadata":{"agent_name":"/root/private/child","session_id":"root","thread_id":"root","forked_from_ordinal_exclusive":12,"turn_trigger":"user","sandbox_mode":"workspace_write","auto_review_enabled":false,"node_repl_auto_review_required":true,"node_repl_disabled":false,"history_ingest_requested":true,"workspace_kind":"local","model":"stale","reasoning_effort":"medium","workspaces":{"C:/private":{"has_changes":false,"latest_git_commit_hash":"private-commit","associated_remote_urls":{"origin":"private-url"}}},"unknown":{"session_id":"private"}}}}`)
		body, _ = sjson.SetRawBytes(body, "client_metadata.x-codex-turn-metadata.tool_namespaces_info", []byte(inventory))
		if encoded {
			body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata", gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata").Raw)
		}
		originalInput := gjson.GetBytes(body, "input").Raw
		out, headers := PrepareCodexOutboundMetadata(account, body, nil)
		out, err := PrepareCodexFunctionalFields(context.Background(), account, out, headers, "user-key")
		require.NoError(t, err)
		out, headers = FinalizeCodexOutboundMetadata(out, headers)
		var flat map[string]string
		require.NoError(t, json.Unmarshal([]byte(gjson.GetBytes(out, "client_metadata").Raw), &flat))
		meta := gjson.Parse(flat["x-codex-turn-metadata"])
		require.Equal(t, gjson.False, meta.Get("auto_review_enabled").Type)
		require.Equal(t, "false", flat["auto_review_enabled"])
		require.Equal(t, int64(12), meta.Get("forked_from_ordinal_exclusive").Int())
		require.True(t, meta.Get("history_ingest_requested").Bool())
		require.Equal(t, "workspace_write", meta.Get("sandbox_mode").String())
		require.Equal(t, "actual-model", meta.Get("model").String())
		require.Equal(t, "actual-model", flat["model"])
		require.Equal(t, "high", flat["reasoning_effort"])
		require.Equal(t, "local", meta.Get("workspace_kind").String())
		require.JSONEq(t, expected, meta.Get("tool_namespaces_info").Raw)
		require.False(t, gjson.Get(headers.Get("X-Codex-Turn-Metadata"), "tool_namespaces_info").Exists())
		require.NotContains(t, string(out), "private")
		require.True(t, strings.HasPrefix(meta.Get("agent_name").String(), "/root/out_"))
		meta.Get("workspaces").ForEach(func(_, value gjson.Result) bool {
			require.Equal(t, gjson.False, value.Get("has_changes").Type)
			return true
		})
		require.Equal(t, originalInput, gjson.GetBytes(out, "input").Raw)
		require.NoError(t, ValidateCodexOutboundMetadata(out, headers))
		again, againHeaders := FinalizeCodexOutboundMetadata(out, headers)
		require.JSONEq(t, string(out), string(again))
		require.Equal(t, headers, againHeaders)
	}
}

func TestOutboundSchemaRelayFunctionalFieldsFinalWire(t *testing.T) {
	oldResin := GetResinConfig()
	SetResinConfig(nil)
	t.Cleanup(func() { SetResinConfig(oldResin) })
	type capture struct {
		body    []byte
		headers http.Header
	}
	seen := make(chan capture, 6)
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		body, _ := io.ReadAll(r.Body)
		seen <- capture{body, r.Header.Clone()}
		w.Header().Set("Content-Type", "application/json")
		_, _ = w.Write([]byte(`{"id":"test-response","output":[]}`))
	}))
	defer server.Close()
	account := &auth.Account{DBID: 42, UpstreamType: auth.UpstreamOpenAIResponses, BaseURL: server.URL, APIKey: "account-key"}
	body := []byte(`{"model":"gpt-6-astra","input":"unchanged","user":"private-subject","safety_identifier":"private-subject","metadata":{"category":"bugfix","user_id":"private-subject","session_id":"stale-session","nested":"{\"session_id\":\"private\"}"},"moderation":{"model":"omni-moderation-latest","policy":{"input":{"mode":"score","request_id":"private"},"output":{"mode":"block"}},"identity":{"account_id":"private"}},"prompt_cache_options":{"mode":"explicit","ttl":"30m","prewarm":true,"comparison_response_id":"resp_provider_owned","identity":{"session_id":"private"}},"access_programs":{"cyber":"standard","identity":{"session_id":"private"}},"client_metadata":{"x-codex-turn-metadata":{"session_id":"private-session","thread_id":"private-session","agent_name":"/root/private"}},"tools":[],"stream_options":{"include_obfuscation":true}}`)
	var alias string
	for _, endpoint := range []string{"create", "create", "compact"} {
		var response *http.Response
		var err error
		headers := http.Header{"Authorization": {"Bearer caller-key"}}
		if endpoint == "create" {
			response, err = ExecuteOpenAIResponsesRequest(context.Background(), account, body, "", headers)
		} else {
			response, err = ExecuteOpenAIResponsesCompactRequest(context.Background(), account, body, "", headers)
		}
		require.NoError(t, err)
		_, err = io.Copy(io.Discard, response.Body)
		require.NoError(t, err)
		require.NoError(t, response.Body.Close())
		wire := <-seen
		require.NotContains(t, string(wire.body), "private")
		require.JSONEq(t, `{"mode":"explicit","ttl":"30m","prewarm":true,"comparison_response_id":"resp_provider_owned"}`, gjson.GetBytes(wire.body, "prompt_cache_options").Raw)
		if endpoint == "compact" {
			for _, key := range []string{"metadata", "user", "safety_identifier", "moderation", "tools", "access_programs", "stream_options", "client_metadata"} {
				require.False(t, gjson.GetBytes(wire.body, key).Exists(), key)
			}
			continue
		}
		subject := gjson.GetBytes(wire.body, "user").String()
		require.NotEmpty(t, subject)
		require.LessOrEqual(t, len(subject), 64)
		require.Equal(t, subject, gjson.GetBytes(wire.body, "safety_identifier").String())
		require.Equal(t, subject, gjson.GetBytes(wire.body, "metadata.user_id").String())
		if alias == "" {
			alias = subject
		} else {
			require.Equal(t, alias, subject)
		}
		require.Equal(t, "bugfix", gjson.GetBytes(wire.body, "metadata.category").String())
		meta := diagnosticMetadataObject(gjson.GetBytes(wire.body, "client_metadata.x-codex-turn-metadata"))
		require.Equal(t, meta.Get("session_id").String(), gjson.GetBytes(wire.body, "metadata.session_id").String())
		require.JSONEq(t, `{"model":"omni-moderation-latest","policy":{"input":{"mode":"score"},"output":{"mode":"block"}}}`, gjson.GetBytes(wire.body, "moderation").Raw)
		require.JSONEq(t, `{"cyber":"standard"}`, gjson.GetBytes(wire.body, "access_programs").Raw)
		var flat map[string]string
		require.NoError(t, json.Unmarshal([]byte(gjson.GetBytes(wire.body, "client_metadata").Raw), &flat))
		require.NoError(t, ValidateCodexOutboundMetadata(wire.body, wire.headers))
	}
	for _, control := range []string{`{"model":"gpt-6-astra","generate":false}`, `{"model":"gpt-6-astra","stream_id":"main"}`} {
		_, err := ExecuteOpenAIResponsesRequest(context.Background(), account, []byte(control), "", nil)
		require.Error(t, err)
		select {
		case <-seen:
			t.Fatal("WS-only control was sent as an HTTP generation")
		default:
		}
	}
}

func TestOutboundSchemaHTTPDoesNotGenerateOnPrewarmFallback(t *testing.T) {
	for _, body := range []string{`{"generate":false}`, `{"stream_id":"main"}`, `{"generate":"false"}`} {
		_, err := prepareCodexHTTPControls([]byte(body))
		require.Error(t, err)
	}
	body, err := prepareCodexHTTPControls([]byte(`{"generate":true,"stream_id":null,"type":"response.create","input":[{"type":"message"}]}`))
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(body, "generate").Exists())
	require.False(t, gjson.GetBytes(body, "type").Exists())
	require.False(t, gjson.GetBytes(body, "stream_id").Exists())
	require.Equal(t, "message", gjson.GetBytes(body, "input.0.type").String())
}

func TestOutboundSchemaComparisonResponseBindingAndEcho(t *testing.T) {
	handler, account, other, _ := responsePrivacySetup(t)
	c, _, _ := responsePrivacyRequest(t, handler, 101, "first", "")
	alias, err := responseIdentityFrom(c.Request.Context()).issue(c.Request.Context(), account, "resp_comparison_original")
	require.NoError(t, err)
	next, body, _ := responsePrivacyRequest(t, handler, 101, "second", "")
	body, _ = sjson.SetBytes(body, "prompt_cache_options.comparison_response_id", alias)
	original := append([]byte(nil), body...)
	for i := 0; i < 2; i++ {
		body, err = prepareResponseIdentityOutbound(next.Request.Context(), account, body)
		require.NoError(t, err)
		require.Equal(t, "resp_comparison_original", gjson.GetBytes(body, "prompt_cache_options.comparison_response_id").String())
		require.Nil(t, responseIdentityFrom(next.Request.Context()).incoming, "comparison must not become a continuation")
	}
	echo, err := maskResponsePayload(next.Request.Context(), account, []byte(`{"type":"response.completed","response":{"id":"resp_new","prompt_cache_options":{"comparison_response_id":"resp_comparison_original"},"client_metadata":{"nested":"{\"agent_name\":\"/root/out_native\"}"},"output":[]}}`), false)
	require.NoError(t, err)
	require.Equal(t, alias, gjson.GetBytes(echo, "response.prompt_cache_options.comparison_response_id").String())
	require.NotContains(t, string(echo), "resp_comparison_original")
	require.NotContains(t, string(echo), "out_native")
	_, err = prepareResponseIdentityOutbound(next.Request.Context(), other, body)
	require.Error(t, err)
	foreign, _, _ := responsePrivacyRequest(t, handler, 102, "second", "")
	_, err = prepareResponseIdentityOutbound(foreign.Request.Context(), account, original)
	require.Error(t, err)
	unknown, _ := sjson.SetBytes(original, "prompt_cache_options.comparison_response_id", "resp_unknown")
	_, err = prepareResponseIdentityOutbound(next.Request.Context(), account, unknown)
	require.Error(t, err)
}

func TestOutboundSchemaStreamScopeAndAgentAncestry(t *testing.T) {
	account := &auth.Account{DBID: 42, AccessToken: "account-secret"}
	body := []byte(`{"stream_id":"lane.1","client_metadata":{"x-codex-turn-metadata":"{\"thread_id\":\"mapped-thread\"}"}}`)
	_, original, first, err := PrepareCodexStreamID(context.Background(), account, body, nil, "caller-a")
	require.NoError(t, err)
	require.Equal(t, "lane.1", original)
	for _, caller := range []string{"caller-a", "caller-b"} {
		_, _, mapped, err := PrepareCodexStreamID(context.Background(), account, body, nil, caller)
		require.NoError(t, err)
		if caller == "caller-a" {
			require.Equal(t, first, mapped)
		} else {
			require.NotEqual(t, first, mapped)
		}
	}
	for _, invalid := range []string{`""`, `"lane/1"`, `1`, `"中文"`} {
		bad, _ := sjson.SetRawBytes(body, "stream_id", []byte(invalid))
		_, _, _, err := PrepareCodexStreamID(context.Background(), account, bad, nil, "caller-a")
		require.Error(t, err)
	}
	var parent string
	for _, name := range []string{"/root/agent", "/root/agent/child"} {
		body := []byte(`{"client_metadata":{"x-codex-turn-metadata":{}}}`)
		body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.agent_name", name)
		out, err := PrepareCodexFunctionalFields(context.Background(), account, body, nil, "caller-a")
		require.NoError(t, err)
		mapped := diagnosticMetadataObject(gjson.GetBytes(out, "client_metadata.x-codex-turn-metadata")).Get("agent_name").String()
		if parent == "" {
			parent = mapped
		} else {
			require.True(t, strings.HasPrefix(mapped, parent+"/"))
		}
	}
}
