package proxy

import (
	"bytes"
	"encoding/json"
	"fmt"
	"sync/atomic"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestSessionStandaloneOutputsKeepWireShape(t *testing.T) {
	for _, callID := range []string{"", `,"call_id":null`} {
		item := `{"type":"function_call_output","name":"notify","namespace":"codex_app","output":"new message"` + callID + `}`
		body := []byte(`{"model":"gpt-5.6-sol","input":[` + item + `]}`)
		for _, prepare := range []func([]byte) ([]byte, string){PrepareResponsesBody, PrepareResponsesWebSocketBody} {
			prepared, _ := prepare(body)
			require.JSONEq(t, item, gjson.GetBytes(prepared, "input.0").Raw)
		}
		// Invalid arguments on an unrelated, id-less call must not consume a
		// legitimate standalone message when the sanitizer removes that call.
		var malformed map[string]any
		require.NoError(t, json.Unmarshal([]byte(`{"input":[{"type":"function_call","name":"broken","arguments":"{"},`+item+`]}`), &malformed))
		require.True(t, sanitizeMalformedResponsesFunctionCalls(malformed))
		require.False(t, repairResponsesToolCallPairing(malformed))
		encoded, err := json.Marshal(malformed)
		require.NoError(t, err)
		require.JSONEq(t, `[`+item+`]`, gjson.GetBytes(encoded, "input").Raw)
	}
}

func TestSessionStandaloneOutputRequiresHistoryOnlyWhenReferencingResponse(t *testing.T) {
	resetResponseCacheStateForTest(testResponseCacheConfig())
	body := []byte(`{"model":"gpt-5.6-sol","previous_response_id":"resp_missing_standalone","input":[{"type":"function_call_output","name":"notify","output":"message"}]}`)
	prepared := prepareResponsesBodyForOwnerDetailed(body, "standalone-test")
	require.True(t, prepared.RequiresLocalContext)
	_, _, unavailable := responseCachePreparationFailure(prepared)
	require.True(t, unavailable)
	require.Equal(t, "function_call_output", gjson.GetBytes(prepared.Body, "input.0.type").String())
	paired, err := sjson.SetBytes(body, "input.0.call_id", "missing")
	require.NoError(t, err)
	dependent := prepareResponsesBodyForOwnerDetailed(paired, "standalone-test")
	require.True(t, dependent.RequiresLocalContext)
	_, _, unavailable = responseCachePreparationFailure(dependent)
	require.True(t, unavailable)
	standalone, err := sjson.DeleteBytes(body, "previous_response_id")
	require.NoError(t, err)
	independent := prepareResponsesBodyForOwnerDetailed(standalone, "standalone-test")
	_, _, unavailable = responseCachePreparationFailure(independent)
	require.False(t, unavailable)
	require.Equal(t, "function_call_output", gjson.GetBytes(independent.Body, "input.0.type").String())
}

func TestSessionStandaloneFunctionOutputsPreservedAcrossContextModes(t *testing.T) {
	// Official Codex models call_id as Option<String>; standalone notification
	// and cross-thread messages are projected into prompts without a call.
	for _, item := range []string{
		`{"type":"function_call_output","name":"send_message_to_thread","namespace":"codex_app","output":"cross-thread message"}`,
		`{"type":"function_call_output","call_id":null,"name":"notifications","namespace":"slack","output":[{"type":"input_text","text":"notification"}]}`,
		`{"type":"function_call_output","output":"standalone without an optional name"}`,
	} {
		for _, preserve := range []bool{false, true} {
			t.Run(fmt.Sprintf("%s/preserve=%t", gjson.Get(item, "name").String(), preserve), func(t *testing.T) {
				body := []byte(`{"input":[` + item + `]}`)
				before := bytes.Clone(body)
				reason, blockers := inspectSessionFailoverContext(nil, body, nil)
				require.Empty(t, reason)
				require.Empty(t, blockers)
				cleaned, _, report, err := cleanSessionRestartContext(nil, body, nil, preserve)
				require.NoError(t, err)
				require.JSONEq(t, gjson.GetBytes(body, "input").Raw, gjson.GetBytes(cleaned, "input").Raw)
				require.Empty(t, report.Removed)
				require.Nil(t, report.ToolPairing)
				require.Equal(t, before, body)
				repeated, _, _, err := cleanSessionRestartContext(nil, cleaned, nil, preserve)
				require.NoError(t, err)
				require.JSONEq(t, string(cleaned), string(repeated))
			})
		}
	}
}

func TestSessionStandaloneOutputsDoNotHideBrokenPairs(t *testing.T) {
	for _, broken := range []string{
		`{"type":"function_call_output","call_id":"missing","output":"orphan"}`,
		`{"type":"function_call_output","call_id":"","output":"orphan"}`,
		`{"type":"function_call_output","call_id":123,"output":"orphan"}`,
		`{"type":"custom_tool_call_output","output":"orphan"}`,
		`{"type":"custom_tool_call_output","call_id":null,"output":"orphan"}`,
	} {
		body := []byte(`{"input":[{"type":"function_call_output","name":"notify","output":"standalone"},` + broken + `]}`)
		_, _, report, err := cleanSessionRestartContext(nil, body, nil, true)
		require.Error(t, err, broken)
		require.Equal(t, 1, report.ToolPairing.MissingCallCount)
		require.Equal(t, "input[1].call_id", report.ToolPairing.MissingCalls[0].Path)
		reason, blockers := inspectSessionFailoverContext(nil, body, nil)
		require.Equal(t, "incomplete_tool_context", reason)
		require.Equal(t, "input[1].call_id", blockers[0].Path)
		cleaned, _, cleanup, err := cleanSessionRestartContext(nil, body, nil, false)
		require.NoError(t, err)
		require.Equal(t, 1, cleanup.Removed["orphan_tool_output"])
		require.Len(t, gjson.GetBytes(cleaned, "input").Array(), 1)
		require.Equal(t, "standalone", gjson.GetBytes(cleaned, "input.0.output").String())
	}
}

func TestSessionStandaloneOutputsSurviveFailoverAndRestore(t *testing.T) {
	for _, preserve := range []bool{false, true} {
		t.Run(fmt.Sprintf("preserve=%t", preserve), func(t *testing.T) {
			handler, owner, target, key := failoverTestSetup(t, true)
			UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexSessionFailoverPreserveInput = preserve; return s })
			atomic.StoreInt32(&owner.Disabled, 1)
			require.True(t, handler.store.ApplyAccountGroups(owner.ID(), []int64{3, 11}))
			require.True(t, handler.store.ApplyAccountGroups(target.ID(), []int64{11, 3}))
			require.True(t, handler.store.ApplyAccountTags(owner.ID(), []string{"card-owner"}))
			require.True(t, handler.store.ApplyAccountTags(target.ID(), []string{"card-target"}))
			request, body := failoverTestRequest(t, handler)
			body, err := sjson.SetRawBytes(body, "input", []byte(`[{"type":"function_call","call_id":"paired","name":"exec","arguments":"{}"},{"type":"function_call_output","call_id":"paired","output":"normal result"},{"type":"function_call_output","name":"send_message_to_thread","namespace":"codex_app","output":"message"}]`))
			require.NoError(t, err)
			body = addSessionTools(t, body)
			request.Set(ingressRequestBodyContextKey, body)
			require.Nil(t, handler.configureSessionModelAffinity(request, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
			selected, _, handled := handler.takeSessionAccountFailover(request.Request.Context(), key, 0, nil, nil, auth.DispatchPolicyStandard)
			require.True(t, handled)
			require.Same(t, target, selected)
			handler.store.Release(selected)
			require.Nil(t, handler.commitSessionContinuity(request, target))
			for _, resumed := range []bool{false, true} {
				if resumed {
					handler.continuityRecords = nil
					request, _ = failoverTestRequest(t, handler)
					request.Set(ingressRequestBodyContextKey, body)
					require.Nil(t, handler.configureSessionModelAffinity(request, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
				}
				cleaned, _, err := PrepareSessionRestartOutbound(request.Request.Context(), target, body, request.Request.Header)
				require.NoError(t, err)
				require.JSONEq(t, gjson.GetBytes(body, "input").Raw, gjson.GetBytes(cleaned, "input").Raw)
				assertSessionTools(t, cleaned)
			}
		})
	}
}

func TestSessionStandaloneOutputsRestoreLegacyMigration(t *testing.T) {
	handler, owner, target, key := failoverTestSetup(t, true)
	_, _, err := handler.db.SwitchSessionContinuityAccount(t.Context(), database.SessionAccountFailover{RootKey: hashRiskIdentity(key), ExpectedAccountID: owner.ID(), AccountID: target.ID(), Reason: "account_usage_exhausted"})
	require.NoError(t, err)
	request, body := failoverTestRequest(t, handler)
	body, err = sjson.SetRawBytes(body, "input", []byte(`[{"type":"function_call_output","name":"notify","output":"notification"}]`))
	require.NoError(t, err)
	require.Nil(t, handler.restoreMigratedSessionOwner(request, key, body))
}
