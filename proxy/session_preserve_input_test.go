package proxy

import (
	"bytes"
	"context"
	"net/http"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestSessionPreserveInputRetainsOpaqueHistoryAndTools(t *testing.T) {
	body := preservationBody()
	body, _ = sjson.SetRawBytes(body, "input.-1", []byte(`{"type":"reasoning","id":"old-item","encrypted_content":"old-cipher"}`))
	body, _ = sjson.SetRawBytes(body, "input.-1", []byte(`{"type":"compaction","id":"old-compact","encrypted_content":"old-summary"}`))
	body, _ = sjson.SetRawBytes(body, "tools.0.parameters.minimum", []byte(`9007199254740993`))
	body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-state", "old-turn")
	headers := http.Header{"X-Codex-Turn-State": {"old-turn"}}
	before := bytes.Clone(body)
	cleaned, outgoing, report, err := cleanSessionRestartContext(headers, body, nil, true)
	require.NoError(t, err)
	require.JSONEq(t, gjson.GetBytes(body, "input").Raw, gjson.GetBytes(cleaned, "input").Raw)
	require.Equal(t, "9007199254740993", gjson.GetBytes(cleaned, "tools.0.parameters.minimum").Raw)
	require.Equal(t, "preserve_input", report.Mode)
	require.Empty(t, outgoing.Get("X-Codex-Turn-State"))
	require.False(t, gjson.GetBytes(cleaned, "client_metadata.x-codex-turn-state").Exists())
	require.Equal(t, "preserved", report.ToolPreservation)
	require.Equal(t, before, body)
	require.Equal(t, "old-turn", headers.Get("X-Codex-Turn-State"))
	repeated, _, _, err := cleanSessionRestartContext(outgoing, cleaned, nil, true)
	require.NoError(t, err)
	require.JSONEq(t, string(cleaned), string(repeated))
}

func TestSessionPreserveInputRejectsIncrementalHistoryBeforeSwitch(t *testing.T) {
	for _, body := range []string{
		`{"previous_response_id":"old","input":[{"role":"user","content":"continue"}]}`,
		`{"conversation":"old","input":"continue"}`,
		`{"input":[{"type":"function_call_output","call_id":"missing","output":"result"}]}`,
		`{"input":[]}`,
	} {
		_, _, report, err := cleanSessionRestartContext(nil, []byte(body), nil, true)
		require.Error(t, err, body)
		require.Equal(t, "preserve_input", report.Mode)
	}
	// A verified current-segment response can still receive incremental input.
	_, _, _, err := cleanSessionRestartContext(nil, []byte(`{"previous_response_id":"known","input":"continue"}`), func(kind, token string) bool { return kind == "previous_response_id" && token == "known" }, true)
	require.NoError(t, err)
}

func TestSessionPreserveInputSwitchAndRestore(t *testing.T) {
	handler, owner, target, key := failoverTestSetup(t, true)
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexSessionFailoverPreserveInput = true; return s })
	atomic.StoreInt32(&owner.Disabled, 1)
	request, body := failoverTestRequest(t, handler)
	body, _ = sjson.SetRawBytes(body, "input", []byte(`[{"type":"compaction","id":"old-id","encrypted_content":"old-history"},{"role":"user","content":"continue"}]`))
	body = addSessionTools(t, body)
	handler.recordCompactionProvenanceFromPayload(context.Background(), owner, []byte(`{"output":[{"type":"compaction","encrypted_content":"old-history"}]}`))
	body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-state", "old-turn")
	request.Set(ingressRequestBodyContextKey, body)
	require.Nil(t, handler.configureSessionModelAffinity(request, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
	selected, _, handled := handler.takeSessionAccountFailover(request.Request.Context(), key, 0, nil, nil, auth.DispatchPolicyStandard)
	require.True(t, handled)
	require.Same(t, target, selected)
	handler.store.Release(selected)
	require.Nil(t, handler.commitSessionContinuity(request, target))
	stored, found, err := handler.db.ReadSessionContinuity(context.Background(), hashRiskIdentity(key))
	require.NoError(t, err)
	require.True(t, found)
	require.True(t, stored.PreserveRestartInput)
	require.EqualValues(t, 1, stored.FailoverCount)
	// Reloading settings or the process cannot silently enable lossy cleanup.
	UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexSessionFailoverPreserveInput = false; return s })
	handler.continuityRecords = nil
	resumed, _ := failoverTestRequest(t, handler)
	resumed.Set(ingressRequestBodyContextKey, body)
	require.Nil(t, handler.configureSessionModelAffinity(resumed, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
	ctx := resumed.Request.Context()
	require.True(t, PreserveSessionInput(ctx))
	affinity, err := handler.resolveCompactionAffinity(ctx, body)
	require.NoError(t, err)
	require.False(t, affinity.Known)
	cleaned, outgoing, err := PrepareSessionRestartOutbound(ctx, target, body, request.Request.Header)
	require.NoError(t, err)
	require.JSONEq(t, gjson.GetBytes(body, "input").Raw, gjson.GetBytes(cleaned, "input").Raw)
	assertSessionTools(t, cleaned)
	require.False(t, gjson.GetBytes(cleaned, "client_metadata.x-codex-turn-state").Exists())
	require.NoError(t, ValidatePreservedSessionInput(ctx, cleaned))
	mutated, _ := sjson.DeleteBytes(cleaned, "input.1")
	require.Error(t, ValidatePreservedSessionInput(ctx, mutated))
	unmodified, attempt := prepareEncryptedContentAttempt(ctx, target, cleaned, key, outgoing)
	require.Nil(t, attempt)
	require.Equal(t, cleaned, unmodified)
	// Environment rewriting must not mutate input in this mode.
	ctx = WithCodexEnvironment(ctx, func(string) *time.Location { t.Fatal("unexpected timezone lookup"); return nil }, time.Now())
	require.Equal(t, cleaned, ApplyCodexEnvironment(ctx, cleaned, "proxy"))
}
