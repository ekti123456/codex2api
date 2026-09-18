package proxy

import (
	"bufio"
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestResponsePrivacyPreservesProtocolBusinessPayloads(t *testing.T) {
	h, account, _, _ := responsePrivacySetup(t)
	c, _, _ := responsePrivacyRequest(t, h, 101, "turn", "")
	for _, item := range []string{
		`{"type":"reasoning","id":"rs_keep","summary":[{"type":"summary_text","text":"{\"session_id\":\"business-session\",\"error\":{\"message\":\"business-error\"}}"}],"encrypted_content":"opaque"}`,
		`{"type":"computer_call","id":"cu_keep","call_id":"call_keep","action":{"type":"type","text":"{\"account_id\":\"business-account\",\"error\":{\"message\":\"business-error\"}}"}}`,
		`{"type":"shell_call","id":"sh_keep","call_id":"call_keep","action":{"commands":["echo example"],"env":{"USER":"business-user","TOKEN":"business-token"}}}`,
		`{"type":"code_interpreter_call","id":"ci_keep","code":"{\"account_id\":\"business-account\",\"error\":{\"message\":\"business-error\"}}","outputs":[{"type":"logs","logs":"{\"session_id\":\"business-session\"}"}]}`,
		`{"type":"file_search_call","id":"fs_keep","results":[{"file_id":"file_keep","text":"{\"account_id\":\"business-account\",\"error\":{\"message\":\"business-error\"}}","attributes":{"user":"business-user"}}]}`,
		`{"type":"function_call_output","call_id":"call_keep","output":{"type":"error","error":{"message":"business-error"},"account_id":"business-account","session_id":"business-session"}}`,
		`{"type":"apply_patch_call","id":"ap_keep","call_id":"call_keep","operation":{"type":"create_file","path":"example.json","diff":"{\"session_id\":\"business-session\"}"}}`,
		`{"type":"mcp_call","id":"mcp_keep","output":"{\"session_id\":\"business-session\"}","arguments":"{}"}`,
		`{"type":"message","id":"msg_keep","role":"assistant","content":[{"type":"output_text","text":"{\"account_id\":\"business-account\"}"}]}`,
		`{"type":"function_call","id":"fc_keep","call_id":"call_keep","name":"lookup","arguments":"{\"session_id\":\"business-session\",\"error\":{\"message\":\"business-error\"}}"}`,
	} {
		t.Run(gjson.Get(item, "type").String(), func(t *testing.T) {
			for _, envelope := range []struct {
				name, body, path string
				jsonResponse     bool
			}{
				{"json", `{"id":"resp_business","output":[` + item + `]}`, "output.0", true},
				{"completed", `{"type":"response.completed","response":{"id":"resp_business","output":[` + item + `]}}`, "response.output.0", false},
				{"item_done", `{"type":"response.output_item.done","item":` + item + `}`, "item", false},
			} {
				t.Run(envelope.name, func(t *testing.T) {
					masked, err := maskResponsePayload(c.Request.Context(), account, []byte(envelope.body), envelope.jsonResponse)
					require.NoError(t, err)
					require.JSONEq(t, item, gjson.GetBytes(masked, envelope.path).Raw, "identity masking altered business data")
					projected := publicResponseErrorPayload(c, []byte(envelope.body))
					require.JSONEq(t, item, gjson.GetBytes(projected, envelope.path).Raw, "public error projection altered business data")
					// Item metadata remains private next to its opaque business fields.
					withMetadata, err := sjson.SetBytes([]byte(envelope.body), envelope.path+".metadata", map[string]string{"account_id": "private-control", "session_id": "private-control"})
					require.NoError(t, err)
					masked, err = maskResponsePayload(c.Request.Context(), account, withMetadata, envelope.jsonResponse)
					require.NoError(t, err)
					require.NotContains(t, string(masked), "private-control")
				})
			}
		})
	}
	// Adding a business-looking type inside metadata cannot exempt it from cleanup.
	metadata, err := json.Marshal(map[string]any{"type": "reasoning", "summary": []any{map[string]string{"text": `{"account_id":"private-control"}`}}})
	require.NoError(t, err)
	masked, err := maskResponsePayload(c.Request.Context(), account, []byte(`{"metadata":`+string(metadata)+`}`), true)
	require.NoError(t, err)
	require.NotContains(t, string(masked), "private-control")
}

func TestResponsePrivacyStreamDeliversBeforeUpstreamFinishes(t *testing.T) {
	h, account, _, _ := responsePrivacySetup(t)
	c, _, _ := responsePrivacyRequest(t, h, 101, "turn", "")
	reader, writer := io.Pipe()
	defer reader.Close()
	defer writer.Close()
	response := &http.Response{StatusCode: http.StatusOK, Header: http.Header{"Content-Type": {"text/event-stream"}}, Body: reader}
	require.NoError(t, maskTurnStateResponse(c.Request.Context(), account, response))
	frame := `data: {"type":"response.reasoning_summary_part.done","part":{"type":"summary_text","text":"{\"account_id\":\"business-account\"}"}}` + "\n\n"
	writeDone := make(chan error, 1)
	go func() { _, err := io.WriteString(writer, frame); writeDone <- err }()
	timer := time.AfterFunc(2*time.Second, func() { _ = writer.CloseWithError(context.DeadlineExceeded) })
	defer timer.Stop()
	// The producer remains open after its first event. Reading it must not need
	// response.completed or EOF, and public projection must preserve its text.
	line, err := bufio.NewReader(response.Body).ReadString('\n')
	require.NoError(t, err)
	require.NoError(t, <-writeDone)
	payload := []byte(strings.TrimSpace(strings.TrimPrefix(line, "data: ")))
	projected := publicResponseErrorPayload(c, payload)
	require.Equal(t, `{"account_id":"business-account"}`, gjson.GetBytes(projected, "part.text").String())
}

func TestTurnStateCleanupPreservesBusinessRequestBytes(t *testing.T) {
	body := []byte(`{"model":"gpt-5.5","input":[{"type":"message","role":"user","content":"x-codex-turn-state is a business topic"},{"type":"function_call_output","call_id":"call_keep","output":{"x-codex-turn-state":"business-state","session_id":"business-session","integer":9007199254740993}}],"tools":[{"type":"function","name":"lookup","parameters":{"type":"object","properties":{"x-codex-turn-state":{"type":"string"},"account_id":{"type":"string"}}}}],"client_metadata":{"x-codex-turn-state":"unverified-control"}}`)
	out, headers := PrepareCodexTurnStateOutbound(context.Background(), nil, body, http.Header{"X-Codex-Turn-State": {"unverified-control"}})
	require.NotContains(t, string(out), "unverified-control")
	require.Empty(t, headers.Get(codexTurnStateHeader))
	for _, path := range []string{"input", "tools"} {
		require.Equal(t, gjson.GetBytes(body, path).Raw, gjson.GetBytes(out, path).Raw)
	}
}
