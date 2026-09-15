package proxy

import (
	"bytes"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
)

func sessionToolPreservationAPIError(err error, report *database.SessionContextCleanup) *api.APIError {
	failure := codexIdentityRequestError(err)
	if failure == nil || failure.Code != "codex_session_tool_context_lost" {
		return nil
	}
	failure.Details = map[string]any{"retry": "stop", "context_cleanup": report}
	return failure
}

func summarizeSessionTools(body []byte) *database.SessionToolSummary {
	if !gjson.ValidBytes(body) {
		return nil
	}
	root := gjson.ParseBytes(body)
	summary := &database.SessionToolSummary{Choice: "absent"}
	digest := sha256.New()
	write := func(kind string, value gjson.Result) {
		// Decode only declarations, not the conversation. UseNumber preserves large
		// schema numbers and canonical marshaling ignores object key ordering.
		_, _ = digest.Write([]byte(kind))
		_, _ = digest.Write([]byte{0})
		if !value.Exists() {
			_, _ = digest.Write([]byte("absent\x00"))
			return
		}
		decoder := json.NewDecoder(bytes.NewBufferString(value.Raw))
		decoder.UseNumber()
		var parsed any
		if decoder.Decode(&parsed) == nil {
			canonical, _ := json.Marshal(parsed)
			_, _ = digest.Write(canonical)
		}
		_, _ = digest.Write([]byte{0})
	}
	var count func(gjson.Result)
	count = func(tools gjson.Result) {
		for _, tool := range tools.Array() {
			switch tool.Get("type").String() {
			case "namespace":
				summary.Namespaces++
				count(tool.Get("tools"))
			case "function":
				summary.Functions++
			case "custom":
				summary.Custom++
			default:
				summary.Other++
			}
		}
	}
	tools := root.Get("tools")
	summary.TopLevel = len(tools.Array())
	write("tools", tools)
	count(tools)
	choice := root.Get("tool_choice")
	write("tool_choice", choice)
	if choice.Exists() {
		summary.Choice = "configured"
		switch choice.String() {
		case "auto", "none", "required":
			summary.Choice = choice.String()
		}
	}
	root.Get("input").ForEach(func(_, item gjson.Result) bool {
		switch item.Get("type").String() {
		case "additional_tools":
			summary.AdditionalItems++
			write("role", item.Get("role"))
			write("additional_tools", item.Get("tools"))
			count(item.Get("tools"))
		case "tool_search_output":
			summary.SearchOutputItems++
			for _, field := range []string{"execution", "status", "call_id"} {
				write(field, item.Get(field))
			}
			write("tool_search_output", item.Get("tools"))
			count(item.Get("tools"))
		}
		return true
	})
	summary.Digest = hex.EncodeToString(digest.Sum(nil))
	return summary
}

func checkSessionToolPreservation(before, after []byte, report *database.SessionContextCleanup) error {
	report.ToolsBefore = summarizeSessionTools(before)
	report.ToolsAfter = summarizeSessionTools(after)
	if report.ToolsBefore != nil && report.ToolsAfter != nil && *report.ToolsBefore == *report.ToolsAfter {
		report.ToolPreservation = "preserved"
		return nil
	}
	report.ToolPreservation = "lost"
	return &Error{Code: "codex_session_tool_context_lost", Type: ErrorTypeServerError, HTTPStatus: http.StatusInternalServerError, Message: "网关处理会话时工具上下文完整性校验失败，请联系管理员检查诊断。"}
}

func sessionToolsEqual(before, after []byte) bool {
	a, b := summarizeSessionTools(before), summarizeSessionTools(after)
	return a != nil && b != nil && *a == *b
}
