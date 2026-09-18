package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"sync/atomic"
	"testing"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestPreserveInputToolPairingDiagnostics(t *testing.T) {
	body := []byte(`{"input":[{"type":"function_call","call_id":"paired","arguments":"private-arguments"},{"type":"function_call_output","call_id":"paired","output":"private-output"},{"type":"function_call_output","call_id":"missing-function","output":"private-output"},{"type":"custom_tool_call_output","call_id":"missing-custom","output":"private-output"},{"type":"custom_tool_call_output","output":"private-output"},{"type":"tool_search_output","execution":"client","call_id":"missing-search","tools":[]}],"tools":[{"type":"function","name":"private-tool"}]}`)
	before := bytes.Clone(body)
	_, _, report, err := cleanSessionRestartContext(nil, body, nil, true)
	require.ErrorContains(t, err, "工具结果缺少对应调用")
	require.Equal(t, before, body)
	require.Empty(t, report.Removed)
	encoded, err := json.Marshal(report)
	require.NoError(t, err)
	pairing := gjson.GetBytes(encoded, "tool_pairing")
	require.Equal(t, "input_top_level", pairing.Get("scope").String())
	require.EqualValues(t, 6, pairing.Get("input_items").Int())
	require.EqualValues(t, 1, pairing.Get("call_items").Int())
	require.EqualValues(t, 5, pairing.Get("output_items").Int())
	require.EqualValues(t, 4, pairing.Get("missing_call_count").Int())
	require.Equal(t, "input[2].call_id", pairing.Get("missing_calls.0.path").String())
	require.Equal(t, "function_call_output", pairing.Get("missing_calls.0.item_type").String())
	require.Equal(t, "function_call", pairing.Get("missing_calls.0.expected_call_type").String())
	require.Equal(t, "missing-function", pairing.Get("missing_calls.0.call_id").String())
	require.Equal(t, "custom_tool_call", pairing.Get("missing_calls.1.expected_call_type").String())
	require.Equal(t, "missing", pairing.Get("missing_calls.2.call_id_state").String())
	require.Equal(t, "tool_search_call", pairing.Get("missing_calls.3.expected_call_type").String())
	require.EqualValues(t, 1, gjson.GetBytes(encoded, "tools_before.functions").Int())
	require.NotContains(t, string(encoded), "private-")
}

func TestPreserveInputToolPairingDecisionsUnchanged(t *testing.T) {
	for _, tc := range []struct {
		name, body              string
		knownPrevious, rejected bool
	}{
		{"paired function", `{"input":[{"type":"function_call_output","call_id":"a"},{"type":"function_call","call_id":"a"}]}`, false, false},
		{"paired custom", `{"input":[{"type":"custom_tool_call","call_id":"a"},{"type":"custom_tool_call_output","call_id":"a"}]}`, false, false},
		{"hosted search", `{"input":[{"type":"tool_search_output","execution":"server","call_id":"a"}]}`, false, false},
		{"client search without id", `{"input":[{"type":"tool_search_output","execution":"client"}]}`, false, false},
		{"paired search", `{"input":[{"type":"tool_search_call","call_id":"a"},{"type":"tool_search_output","execution":"client","call_id":"a"}]}`, false, false},
		{"search requires search call", `{"input":[{"type":"function_call","call_id":"a"},{"type":"tool_search_output","execution":"client","call_id":"a"}]}`, false, true},
		{"known continuation", `{"previous_response_id":"known","input":[{"type":"function_call_output","call_id":"a"}]}`, true, false},
		{"unknown continuation", `{"previous_response_id":"unknown","input":[{"type":"function_call_output","call_id":"a"}]}`, false, true},
		{"empty", `{"input":[]}`, false, true},
		// Preserve the current suffix/call_id matcher; diagnostics must not silently tighten it.
		{"same id other call type", `{"input":[{"type":"custom_tool_call","call_id":"a"},{"type":"function_call_output","call_id":"a"}]}`, false, false},
	} {
		t.Run(tc.name, func(t *testing.T) {
			body := []byte(tc.body)
			before := bytes.Clone(body)
			cleaned, _, _, err := cleanSessionRestartContext(nil, body, func(kind, value string) bool {
				return tc.knownPrevious && kind == "previous_response_id" && value == "known"
			}, true)
			require.Equal(t, tc.rejected, err != nil)
			require.Equal(t, before, body)
			if err == nil {
				require.JSONEq(t, gjson.GetBytes(body, "input").Raw, gjson.GetBytes(cleaned, "input").Raw)
			}
		})
	}
}

func TestPreserveInputPairingDiagnosticsBounded(t *testing.T) {
	var items []map[string]any
	for i := 0; i < 20; i++ {
		items = append(items, map[string]any{"type": "function_call_output", "call_id": strings.Repeat("a", 2000), "output": strings.Repeat("secret", 1000)})
	}
	body, err := json.Marshal(map[string]any{"input": items})
	require.NoError(t, err)
	_, _, report, err := cleanSessionRestartContext(nil, body, nil, true)
	require.Error(t, err)
	encoded, err := json.Marshal(report)
	require.NoError(t, err)
	pairing := gjson.GetBytes(encoded, "tool_pairing")
	require.EqualValues(t, 20, pairing.Get("missing_call_count").Int())
	require.Len(t, pairing.Get("missing_calls").Array(), 8)
	require.EqualValues(t, 12, pairing.Get("omitted_items").Int())
	require.True(t, pairing.Get("missing_calls.0.value_truncated").Bool())
	require.LessOrEqual(t, len(pairing.Get("missing_calls.0.call_id").String()), 128)
	require.NotContains(t, string(encoded), "secret")
}

func TestPreserveInputPairingUsageBudgetRetainsCounts(t *testing.T) {
	request := promptSessionLimitTestContext(testRootSessionA)
	state := usageRequestDiagnosticState(request)
	// Escaping control characters expands JSON substantially, even when each
	// identifier has already been capped. The decision must remain inspectable.
	typ := strings.Repeat("\x00", 50) + "_call_output"
	items := make([]map[string]string, 12)
	for i := range items {
		items[i] = map[string]string{"type": typ, "call_id": strings.Repeat("\x00", 128)}
	}
	body, err := json.Marshal(map[string]any{"input": items})
	require.NoError(t, err)
	_, _, cleanup, err := cleanSessionRestartContext(nil, body, nil, true)
	require.Error(t, err)
	state.AccountFailover = &sessionAccountFailoverDiagnostic{Result: "blocked", BlockReason: "incomplete_tool_context", ContextCleanup: cleanup}
	state.Continuity = &sessionContinuityDiagnostic{AccountFailover: state.AccountFailover}
	usage := &database.UsageLogInput{}
	populateUsageRequestDiagnostics(request, usage)
	require.NotEmpty(t, usage.RequestDiagnostics)
	require.LessOrEqual(t, len(usage.RequestDiagnostics), database.MaxUsageRequestDiagnosticsBytes)
	require.True(t, gjson.Get(usage.RequestDiagnostics, "truncated").Bool())
	pairing := gjson.Get(usage.RequestDiagnostics, "account_failover.context_cleanup.tool_pairing")
	require.EqualValues(t, 12, pairing.Get("missing_call_count").Int())
	require.EqualValues(t, 12, len(pairing.Get("missing_calls").Array())+int(pairing.Get("omitted_items").Int()))
	require.NotEmpty(t, pairing.Get("missing_calls").Array())
	require.Len(t, state.AccountFailover.ContextCleanup.ToolPairing.MissingCalls, 8)
	require.Equal(t, 4, state.AccountFailover.ContextCleanup.ToolPairing.OmittedItems)
}

func TestPreserveInputPairingDiagnosticsReachUsageAndServiceErrors(t *testing.T) {
	for _, migrated := range []bool{false, true} {
		name := "before_switch"
		if migrated {
			name = "after_switch"
		}
		t.Run(name, func(t *testing.T) {
			handler, owner, target, key := failoverTestSetup(t, true)
			UpdateRuntimeSettings(func(s RuntimeSettings) RuntimeSettings { s.CodexSessionFailoverPreserveInput = true; return s })
			if migrated {
				_, _, err := handler.db.SwitchSessionContinuityAccount(t.Context(), database.SessionAccountFailover{RootKey: hashRiskIdentity(key), ExpectedAccountID: owner.ID(), AccountID: target.ID(), Reason: "account_usage_exhausted", LossyContextRestart: true, PreserveRestartInput: true})
				require.NoError(t, err)
			} else {
				atomic.StoreInt32(&owner.Disabled, 1)
			}
			request, body := failoverTestRequest(t, handler)
			body, err := sjson.SetRawBytes(body, "input", []byte(`[{"type":"function_call_output","call_id":"missing-call","output":"private-output"}]`))
			require.NoError(t, err)
			finish := handler.beginServiceErrorAudit(request)
			failure := handler.configureSessionModelAffinity(request, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body)
			require.NotNil(t, failure)
			require.Equal(t, "codex_session_failover_context_required", string(failure.Code))
			require.Equal(t, http.StatusBadRequest, api.HTTPStatusCode(failure.Code))
			require.Contains(t, failure.Message, "工具结果缺少对应调用")
			require.Equal(t, "false", request.Writer.Header().Get("X-Should-Retry"))
			trigger := "account_disabled"
			if migrated {
				trigger = "account_usage_exhausted"
			}
			diagnostic := usageRequestDiagnosticState(request).AccountFailover
			require.Equal(t, "incomplete_tool_context", diagnostic.BlockReason)
			require.Equal(t, trigger, diagnostic.TriggerReason)
			require.Equal(t, name, diagnostic.Phase)
			require.Equal(t, "input[0].call_id", diagnostic.ContextBlockers[0].Path)
			usage := &database.UsageLogInput{}
			populateUsageRequestDiagnostics(request, usage)
			require.Equal(t, "missing-call", gjson.Get(usage.RequestDiagnostics, "account_failover.context_cleanup.tool_pairing.missing_calls.0.call_id").String())
			api.SendError(request, failure)
			finish()
			page := serviceErrorTestPage(t, handler)
			require.Len(t, page.Items, 1)
			event, err := json.Marshal(page.Items[0])
			require.NoError(t, err)
			require.Equal(t, "missing-call", gjson.GetBytes(event, "account_failover.context_cleanup.tool_pairing.missing_calls.0.call_id").String())
			require.NotContains(t, string(event), "private-output")
			record, _, err := handler.db.ReadSessionContinuity(t.Context(), hashRiskIdentity(key))
			require.NoError(t, err)
			if migrated {
				require.Equal(t, target.ID(), record.AccountID)
				require.EqualValues(t, 1, record.FailoverCount)
			} else {
				require.Equal(t, owner.ID(), record.AccountID)
				require.Zero(t, record.FailoverCount)
			}
		})
	}
}
