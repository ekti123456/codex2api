package proxy

import (
	"context"
	"encoding/json"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestTurnStateUnifiedCleanupCarriers(t *testing.T) {
	for _, metadata := range []string{
		`{"x-codex-turn-state":"untrusted-secret"}`,
		`{"X-Codex-Turn-State":"untrusted-secret","x_codex_turn_state":"untrusted-secret"}`,
		`{"x-codex-turn-state":"untrusted-secret","x-codex-turn-state":"untrusted-secret"}`,
		`{"x-codex-turn-metadata":{"deep":[{"X-Codex-Turn-State":["untrusted-secret"]}]}}`,
		`{"x_codex_turn_metadata":"{\"deep\":{\"x-codex-turn-state\":\"untrusted-secret\"}}"}`,
		`{"x-codex-turn-metadata":{},"x-codex-turn-metadata":{"x-codex-turn-state":"untrusted-secret"}}`,
		`{"x-codex-turn-state":{"value":"untrusted-secret"}}`,
		`{"x-codex-turn-state":["untrusted-secret","untrusted-secret"]}`,
		`{"\u0078-codex-turn-state":"untrusted-secret"}`,
	} {
		body := []byte(`{"input":[{"role":"user","content":"business-text"}],"tools":[{"parameters":{"x-codex-turn-state":"business-value"}}],"client_metadata":` + metadata + `}`)
		headers := http.Header{"x-codex-turn-state": {"untrusted-secret"}, "X-Codex-Turn-State": {"untrusted-secret"}, "X-Codex-Turn-Metadata": {metadata}}
		account := &auth.Account{DBID: 123}
		for _, mode := range []string{"unbound", "initial", "restart"} {
			t.Run(mode+"/"+metadata, func(t *testing.T) {
				var out []byte
				var outgoing http.Header
				var err error
				switch mode {
				case "unbound":
					out, outgoing = PrepareCodexTurnStateOutbound(context.Background(), account, body, headers)
				case "initial":
					out, outgoing, err = PrepareInitialSessionOutbound(context.WithValue(context.Background(), initialSessionContextKey{}, &initialSessionDiagnostic{}), account, body, headers)
				case "restart":
					out, outgoing, _, err = cleanSessionRestartContext(headers, body, nil)
				}
				require.NoError(t, err)
				require.NotContains(t, string(out), "untrusted-secret")
				encoded, _ := json.Marshal(outgoing)
				require.NotContains(t, string(encoded), "untrusted-secret")
				require.Equal(t, gjson.GetBytes(body, "tools").Raw, gjson.GetBytes(out, "tools").Raw)
				require.Contains(t, string(out), "business-text")
				again, againHeaders := PrepareCodexTurnStateOutbound(context.Background(), account, out, outgoing)
				require.Equal(t, out, again)
				require.Equal(t, outgoing, againHeaders)
			})
		}
	}
}

func TestTurnStateNestedAliasRestoresAndClearsOnAccountOrGenerationChange(t *testing.T) {
	h, owner, target, _ := responsePrivacySetup(t)
	first, _, _ := aliasRequest(t, h, 101, "turn", "", false)
	alias, err := turnStateSessionFrom(first.Request.Context()).issue(first.Request.Context(), owner, "upstream-real", "response_header")
	require.NoError(t, err)
	c, body, identity := aliasRequest(t, h, 101, "turn", "", false)
	body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.nested.X-Codex-Turn-State", alias)
	h.bindTurnStateSession(c, body, identity)
	body = normalizeTurnStateIngress(c, body)
	require.Equal(t, "upstream-real", gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata.nested.X-Codex-Turn-State").String())
	out, _ := PrepareCodexTurnStateOutbound(c.Request.Context(), owner, body, nil)
	require.Contains(t, string(out), "upstream-real")
	out, _ = PrepareCodexTurnStateOutbound(c.Request.Context(), target, body, nil)
	require.NotContains(t, string(out), "upstream-real")
	ctx := context.WithValue(c.Request.Context(), sessionOutboundEpochContextKey{}, &sessionOutboundEpoch{record: database.SessionContinuityRecord{AccountID: owner.ID(), FailoverCount: 2}})
	out, _ = PrepareCodexTurnStateOutbound(ctx, owner, body, nil)
	require.NotContains(t, string(out), "upstream-real")
}

func TestTurnStateDuplicateEnvelopeAndDeepControlCleanup(t *testing.T) {
	for _, body := range []string{
		`{"input":"keep","client_metadata":{"x-codex-turn-state":"old-one"},"client_metadata":{"x-codex-turn-state":"old-two"}}`,
		`{"input":"keep","client_metadata":` + strings.Repeat(`{"nested":`, 70) + `{"x-codex-turn-state":"old-deep"}` + strings.Repeat(`}`, 70) + `}`,
		`{"input":"keep","client_metadata":{"x-codex-turn-metadata":"{\"x-codex-turn-state\":\"old-malformed\""}}`,
	} {
		out, _ := PrepareCodexTurnStateOutbound(context.Background(), nil, []byte(body), nil)
		require.True(t, gjson.ValidBytes(out))
		require.NotContains(t, string(out), "old-")
		require.Equal(t, "keep", gjson.GetBytes(out, "input").String())
	}
}

func TestTurnStateCustomHeadersCannotReintroduceUnverifiedState(t *testing.T) {
	account := &auth.Account{DBID: 71, CustomHeaders: map[string]string{
		"X-Codex-Turn-State":    "injected-secret",
		"X-Codex-Turn-Metadata": `{"turn_id":"keep","nested":{"x-codex-turn-state":"injected-secret"}}`,
	}}
	for _, relay := range []bool{false, true} {
		req, err := http.NewRequest(http.MethodPost, "https://example.invalid/responses", nil)
		require.NoError(t, err)
		if relay {
			applyOpenAIResponsesRequestHeaders(req, account, "test-key", nil)
		} else {
			applyCodexRequestHeaders(req, account, "test-token", "", "test-key", nil, nil)
		}
		require.Empty(t, req.Header.Get(codexTurnStateHeader))
		require.NotContains(t, req.Header.Get(codexTurnMetadataHeader), "injected-secret")
		require.Equal(t, "keep", gjson.Get(req.Header.Get(codexTurnMetadataHeader), "turn_id").String())
	}
}

func TestTurnStateResponseBodyAndNestedMetadataMasking(t *testing.T) {
	h, owner, _, _ := responsePrivacySetup(t)
	c, _, _ := aliasRequest(t, h, 101, "turn", "", false)
	ctx := c.Request.Context()
	payload := `{"type":"response.metadata","headers":{"X-Codex-Turn-State":["private-real"]},"client_metadata":{"x-codex-turn-state":"duplicate-old","x-codex-turn-state":"private-real","nested":{"X-Codex-Turn-State":"private-real"}},"metadata":{"x_codex_turn_metadata":"{\"x-codex-turn-state\":\"private-real\"}"},"response":{"client_metadata":{"x-codex-turn-state":"private-real"},"output":[{"type":"function_call","arguments":"{\"x-codex-turn-state\":\"business-value\"}"}]}}`
	for _, stream := range []bool{false, true} {
		wire, kind := payload, "application/json"
		if stream {
			wire = "data: " + payload + "\n\n"
			kind = "text/event-stream"
		}
		response := &http.Response{Header: http.Header{"Content-Type": {kind}, "X-Codex-Turn-State": {"private-real"}, "X-Codex-Turn-Metadata": {`{"nested":{"x-codex-turn-state":"private-real"}}`}}, Body: io.NopCloser(strings.NewReader(wire))}
		require.NoError(t, maskTurnStateResponse(ctx, owner, response))
		out, err := io.ReadAll(response.Body)
		require.NoError(t, err)
		require.NotContains(t, string(out), "private-real")
		require.NotContains(t, string(out), "duplicate-old")
		require.Contains(t, string(out), "business-value")
		alias := response.Header.Get(codexTurnStateHeader)
		require.True(t, h.db.IsManagedCodexTurnStateAlias(alias))
		record, found, err := h.db.ReadCodexTurnState(ctx, alias)
		require.NoError(t, err)
		require.True(t, found)
		require.Equal(t, "private-real", record.Real)
		require.Equal(t, alias, gjson.Get(response.Header.Get(codexTurnMetadataHeader), "nested.x-codex-turn-state").String())
		data := strings.TrimSpace(strings.TrimPrefix(string(out), "data: "))
		for _, path := range []string{"headers.X-Codex-Turn-State.0", "client_metadata.x-codex-turn-state", "client_metadata.nested.X-Codex-Turn-State", "response.client_metadata.x-codex-turn-state"} {
			require.Equal(t, alias, gjson.Get(data, path).String(), path)
		}
		require.Equal(t, alias, gjson.Get(gjson.Get(data, "metadata.x_codex_turn_metadata").String(), "x-codex-turn-state").String())
	}
	// Missing mapping context removes state in every carrier, never passes R.
	out, err := maskResponsePayload(context.Background(), owner, []byte(payload), false)
	require.NoError(t, err)
	require.NotContains(t, string(out), "private-real")
	// Persistence failure must not release the unmasked body.
	canceled, cancel := context.WithCancel(ctx)
	cancel()
	_, err = maskResponsePayload(canceled, owner, []byte(`{"client_metadata":{"x-codex-turn-state":"new-unpersistable-real"}}`), true)
	require.Error(t, err)
}
