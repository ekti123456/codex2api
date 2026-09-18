package proxy

import (
	"strings"

	"github.com/tidwall/gjson"
)

// Codex's FunctionCallOutput.call_id is optional: notifications and cross-thread
// messages have no corresponding invocation. Only this protocol type permits
// a missing/null call_id; an empty string, wrong type, or custom output does not
// acquire the same exemption. Names and namespaces are optional as well.
func responseContextStandaloneFunctionOutput(item gjson.Result) bool {
	if item.Get("type").String() != "function_call_output" {
		return false
	}
	id := item.Get("call_id")
	return !id.Exists() || id.Type == gjson.Null
}

// Only protocol containers contain context items. Schemas, arguments, grammar,
// metadata and text are data, even when their keys resemble protocol fields.
func responseContextMessage(value gjson.Result) bool {
	kind := strings.TrimSpace(value.Get("type").String())
	if kind == "message" {
		return true
	}
	if kind != "" {
		return false
	}
	switch value.Get("role").String() {
	case "user", "assistant", "developer", "system":
		return true
	}
	return false
}

func responseContextChildFields(value gjson.Result) []string {
	if responseContextMessage(value) {
		return []string{"content"}
	}
	switch value.Get("type").String() {
	case "additional_tools", "tool_search_output",
		"function_call", "custom_tool_call", "tool_search_call", "tool_call",
		"local_shell_call", "shell_call", "apply_patch_call", "mcp_tool_call",
		"reasoning", "compaction", "context_compaction", "compaction_summary",
		"configuration_update", "compaction_trigger", "item_reference",
		"input_text", "output_text", "text", "refusal", "summary_text",
		"input_file", "input_image", "image_url", "image_generation_call", "web_search_call":
		return nil
	case "function_call_output", "custom_tool_call_output", "tool_call_output", "tool_search_call_output",
		"local_shell_call_output", "shell_call_output", "apply_patch_call_output", "mcp_tool_call_output":
		return []string{"output"}
	case "agent_message":
		return []string{"content"}
	}
	// Unknown legacy containers remain conservatively inspected; only known
	// protocol types above establish an opaque payload boundary.
	var fields []string
	value.ForEach(func(key, child gjson.Result) bool {
		if child.IsArray() || child.IsObject() {
			fields = append(fields, key.String())
		}
		return true
	})
	return fields
}

func responseContextSafeType(value gjson.Result) string {
	if responseContextMessage(value) {
		return "message"
	}
	switch kind := value.Get("type").String(); kind {
	case "additional_tools", "tool_search_output", "tool_search_call", "function_call", "function_call_output", "custom_tool_call", "custom_tool_call_output", "reasoning", "compaction", "context_compaction", "compaction_summary", "input_file", "input_image", "item_reference", "configuration_update":
		return kind
	}
	return "unknown"
}

func responseContextSafeField(field string) string {
	switch field {
	case "content", "output", "file", "data", "image_url":
		return field
	}
	return "[field]"
}

func responseContextHasInput(input gjson.Result) bool {
	if input.IsArray() {
		for _, item := range input.Array() {
			if responseContextHasInput(item) {
				return true
			}
		}
		return false
	}
	if input.Type == gjson.String {
		return strings.TrimSpace(input.String()) != ""
	}
	if !input.IsObject() {
		return false
	}
	switch input.Get("type").String() {
	case "additional_tools", "configuration_update", "compaction_trigger":
		return false
	}
	if responseContextMessage(input) {
		content := input.Get("content")
		return content.IsArray() && len(content.Array()) > 0 || content.Type == gjson.String && strings.TrimSpace(content.String()) != ""
	}
	return true
}

// A hosted search output may have no call_id. Client search results with an
// explicit call_id need their matching search call, not an unrelated function.
func missingToolSearchCall(input gjson.Result) int {
	calls := make(map[string]bool)
	for _, item := range input.Array() {
		if item.Get("type").String() == "tool_search_call" {
			calls[item.Get("call_id").String()] = true
		}
	}
	for i, item := range input.Array() {
		if item.Get("type").String() == "tool_search_output" && item.Get("execution").String() == "client" && item.Get("call_id").String() != "" && !calls[item.Get("call_id").String()] {
			return i
		}
	}
	return -1
}
