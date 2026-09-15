package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"strings"
	"testing"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const preservationTools = `[{"type":"namespace","name":"private_namespace","tools":[{"type":"function","name":"execute","description":"private-description","parameters":{"type":"object","properties":{"role":{"type":"string"},"file_id":{"type":"string"},"encrypted_content":{"type":"string"},"large":{"const":9007199254740993}},"required":["role"]}},{"type":"custom","name":"patch","format":{"type":"grammar","syntax":"lark","definition":"start: /.+/"}}]}]`

func preservationBody() []byte {
	return []byte(`{"instructions":"instructions","tool_choice":"auto","tools":` + preservationTools + `,"input":[{"type":"additional_tools","role":"developer","tools":` + preservationTools + `},{"type":"tool_search_call","execution":"client","call_id":"search","arguments":{"role":"editor","file_id":"business-data","encrypted_content":"ordinary-argument"}},{"type":"tool_search_output","execution":"client","status":"completed","call_id":"search","tools":` + preservationTools + `},{"role":"developer","content":"<skills_instructions>skill catalog</skills_instructions>"},{"role":"user","content":"<skill>selected skill</skill>"},{"type":"function_call","call_id":"execute","name":"execute","arguments":"{}"},{"type":"function_call_output","call_id":"execute","output":"read SKILL.md"},{"type":"configuration_update","reasoning":{"effort":"medium"}},{"role":"user","content":[{"type":"input_text","text":"continue"},{"type":"input_file","file_data":"inline-data"}]}]}`)
}

// Small wire fixture used by HTTP and native-WS integration tests.
func addSessionTools(t *testing.T, body []byte) []byte {
	t.Helper()
	input := gjson.GetBytes(body, "input")
	var items []json.RawMessage
	items = append(items, json.RawMessage(`{"type":"additional_tools","role":"developer","tools":[{"type":"function","name":"exec_command","description":"execute","parameters":{"type":"object","properties":{"role":{"type":"string"},"file_id":{"type":"string"}}}}]}`))
	if input.IsArray() {
		for _, item := range input.Array() {
			items = append(items, json.RawMessage(item.Raw))
		}
	} else {
		raw, _ := json.Marshal(map[string]any{"role": "user", "content": input.String()})
		items = append(items, raw)
	}
	raw, _ := json.Marshal(items)
	out, err := sjson.SetRawBytes(body, "input", raw)
	require.NoError(t, err)
	return out
}

func assertSessionTools(t *testing.T, body []byte) {
	t.Helper()
	item := gjson.GetBytes(body, `input.#(type=="additional_tools")`)
	require.Equal(t, "developer", item.Get("role").String())
	require.Equal(t, "exec_command", item.Get("tools.0.name").String())
	require.True(t, item.Get("tools.0.parameters.properties.role").Exists())
	require.True(t, item.Get("tools.0.parameters.properties.file_id").Exists())
}

func TestSessionToolsDiagnosticsAndGuard(t *testing.T) {
	body := preservationBody()
	for _, path := range []string{"tools", "input.0", "input.2.tools.0.tools.0.description", "tool_choice"} {
		damaged, err := sjson.DeleteBytes(body, path)
		require.NoError(t, err)
		report := &database.SessionContextCleanup{}
		failure := checkSessionToolPreservation(body, damaged, report)
		require.Error(t, failure)
		require.Equal(t, "lost", report.ToolPreservation)
		converted := sessionToolPreservationAPIError(failure, report)
		require.NotNil(t, converted)
		require.Equal(t, http.StatusInternalServerError, api.HTTPStatusCode(converted.Code))
		require.Equal(t, api.ErrorTypeServer, converted.Type)
	}
	report := &database.SessionContextCleanup{}
	require.NoError(t, checkSessionToolPreservation(body, body, report))
	require.Equal(t, 2, report.ToolsBefore.AdditionalItems+report.ToolsBefore.SearchOutputItems)
	require.Equal(t, 3, report.ToolsBefore.Functions)
	raw, err := json.Marshal(report)
	require.NoError(t, err)
	for _, secret := range []string{"private_namespace", "private-description", "business-data", "selected skill"} {
		require.NotContains(t, string(raw), secret)
	}
	require.NoError(t, checkSessionToolPreservation([]byte(`{"input":"hi"}`), []byte(`{"input":"hi"}`), &database.SessionContextCleanup{}))
	cleaned, _, removed, err := cleanSessionRestartContext(nil, []byte(`{"input":[`+strings.Repeat(`{"type":"reasoning","encrypted_content":"secret"},`, 40)+`{"role":"user","content":"continue"}]}`), nil)
	require.NoError(t, err)
	require.NotEmpty(t, cleaned)
	require.Len(t, removed.Items, 32)
	require.Equal(t, 8, removed.OmittedItems)
	require.Equal(t, "input[0]", removed.Items[0].Path)
}

func TestSessionToolsSearchPairingAndCache(t *testing.T) {
	body := preservationBody()
	var root map[string]any
	require.NoError(t, json.Unmarshal(body, &root))
	require.False(t, repairResponsesToolCallPairing(root))
	for _, mode := range []string{"client", "server"} {
		output := []byte(`{"type":"tool_search_output","execution":"` + mode + `","status":"completed","call_id":null,"tools":` + preservationTools + `}`)
		item, ok := replayableCachedOutputItem(gjson.ParseBytes(output))
		require.True(t, ok)
		require.JSONEq(t, string(output), string(item))
	}
	callType, isCall, isOutput := responseContextPairType("tool_search_output")
	require.Equal(t, "tool_search_call", callType)
	require.False(t, isCall)
	require.True(t, isOutput)
	for _, execution := range []string{"client", "server"} {
		candidate := []byte(`{"input":[{"type":"tool_search_output","execution":"` + execution + `","call_id":"missing","status":"completed","tools":` + preservationTools + `},{"role":"user","content":"continue"}]}`)
		_, _, _, err := cleanSessionRestartContext(nil, candidate, nil)
		if execution == "client" {
			require.ErrorContains(t, err, "对应的调用")
		} else {
			require.NoError(t, err)
		}
	}
	legacy := []byte(`{"input":[{"type":"tool_search_call_output","call_id":"orphan","output":"old output"},{"role":"user","content":"continue"}]}`)
	_, _, report, err := cleanSessionRestartContext(nil, legacy, nil)
	require.NoError(t, err)
	require.Equal(t, 1, report.Removed["orphan_tool_output"])
}

func TestSessionToolsDeepSchemasAndOrder(t *testing.T) {
	deep := `{"type":"string"}`
	for i := 0; i < 90; i++ {
		deep = `{"type":"object","properties":{"role":` + deep + `}}`
	}
	body := preservationBody()
	body, _ = sjson.SetRawBytes(body, "input.0.tools.0.tools.0.parameters", []byte(deep))
	// A second declaration must remain after the intervening user context.
	body, _ = sjson.SetRawBytes(body, "input.-1", []byte(`{"type":"additional_tools","role":"developer","tools":[]}`))
	cleaned, _, _, err := cleanSessionRestartContext(nil, body, nil)
	require.NoError(t, err)
	require.JSONEq(t, string(body), string(cleaned))
	_, _, _, err = cleanSessionRestartContext(nil, []byte(`{"input":{"type":"reasoning","encrypted_content":"old"}}`), nil)
	require.Error(t, err)
}

func BenchmarkSessionToolsLongContext(b *testing.B) {
	body := preservationBody()
	body, _ = sjson.SetBytes(body, "input.8.content.0.text", strings.Repeat("x", 4<<20))
	b.SetBytes(int64(len(body)))
	b.ReportAllocs()
	b.ResetTimer()
	for i := 0; i < b.N; i++ {
		_, _, _, err := cleanSessionRestartContext(nil, body, nil)
		if err != nil {
			b.Fatal(err)
		}
	}
}

func TestSessionToolsPreserveOfficialDeclarations(t *testing.T) {
	body := preservationBody()
	original := bytes.Clone(body)
	cleaned, _, report, err := cleanSessionRestartContext(nil, body, nil)
	require.NoError(t, err)
	require.JSONEq(t, string(body), string(cleaned))
	require.Contains(t, string(cleaned), "9007199254740993")
	require.Empty(t, report.Removed)
	require.Equal(t, original, body)
	reason, blockers := inspectSessionFailoverContext(nil, body, nil)
	require.Empty(t, reason)
	require.Empty(t, blockers)
	again, _, _, err := cleanSessionRestartContext(nil, cleaned, nil)
	require.NoError(t, err)
	require.JSONEq(t, string(cleaned), string(again))
}

func TestSessionToolsEncryptedRetryPreservesDeclarations(t *testing.T) {
	var root map[string]json.RawMessage
	require.NoError(t, json.Unmarshal(preservationBody(), &root))
	input := gjson.ParseBytes(root["input"]).Array()
	items := []json.RawMessage{json.RawMessage(`{"type":"reasoning","encrypted_content":"old-cipher"}`)}
	for _, item := range input {
		items = append(items, json.RawMessage(item.Raw))
	}
	root["input"], _ = json.Marshal(items)
	body, _ := json.Marshal(root)
	cleaned, changed := stripInvalidEncryptedContentFromResponsesBody(body)
	require.True(t, changed)
	require.JSONEq(t, string(preservationBody()), string(cleaned))
	require.Contains(t, string(cleaned), "9007199254740993")
}

func TestSessionToolsDoNotSubstituteForContext(t *testing.T) {
	for _, item := range []string{`{"type":"additional_tools","role":"developer","tools":` + preservationTools + `}`, `{"type":"configuration_update","reasoning":{"effort":"medium"}}`} {
		_, _, _, err := cleanSessionRestartContext(nil, []byte(`{"input":[`+item+`,{"type":"reasoning","encrypted_content":"old"}]}`), nil)
		require.Error(t, err)
	}
}

func TestSessionToolsMixedOldDependencies(t *testing.T) {
	body := preservationBody()
	body = []byte(strings.Replace(string(body), `"input":[`, `"previous_response_id":"old-response","input":[{"type":"reasoning","encrypted_content":"old-cipher"},{"role":"user","content":[{"type":"input_file","file_id":"old-file"}]},`, 1))
	cleaned, _, report, err := cleanSessionRestartContext(nil, body, nil)
	require.NoError(t, err)
	require.JSONEq(t, string(preservationBody()), string(cleaned))
	require.Equal(t, 1, report.Removed["reasoning"])
	require.Equal(t, 1, report.Removed["file_reference"])
	require.Equal(t, 1, report.Removed["empty_message"])
}
