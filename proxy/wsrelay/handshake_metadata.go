package wsrelay

import (
	"net/http"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func prepareCodexHandshakeSnapshot(headers http.Header) {
	headers.Del("X-Codex-Turn-State")
	const name = "X-Codex-Turn-Metadata"
	raw := headers.Get(name)
	if raw == "" {
		return
	}
	if !gjson.Valid(raw) || !gjson.Parse(raw).IsObject() {
		headers.Del(name)
		return
	}
	for _, field := range []string{"tool_namespaces_info", "tools", "tool_metadata"} {
		raw, _ = sjson.Delete(raw, field)
	}
	if len(raw) > 8192 {
		bounded := "{}"
		for _, field := range []string{"session_id", "thread_id", "parent_thread_id", "forked_from_thread_id", "installation_id", "window_id", "window_number", "context_window_id", "turn_id", "root_turn_id", "parent_turn_id", "turn_started_at_unix_ms", "thread_source", "request_kind", "subagent_kind", "analytics_enabled"} {
			value := gjson.Get(raw, field)
			if value.Exists() && !value.IsObject() && !value.IsArray() && len(value.Raw) <= 512 {
				bounded, _ = sjson.SetRaw(bounded, field, value.Raw)
			}
		}
		raw = bounded
	}
	if len(raw) > 8192 {
		headers.Del(name)
	} else {
		headers.Set(name, raw)
	}
}

func stripCodexHandshakeSnapshotFromProfile(headers http.Header) {
	metadata := gjson.Parse(headers.Get("X-Codex-Turn-Metadata"))
	if enabled := metadata.Get("analytics_enabled"); enabled.Type == gjson.True || enabled.Type == gjson.False {
		headers.Set("Codex-Profile-Analytics-Enabled", enabled.Raw)
	}
	for _, field := range []string{"session_id", "thread_id", "installation_id", "thread_source", "subagent_kind", "parent_thread_id", "forked_from_thread_id"} {
		if value := metadata.Get(field); value.Type == gjson.String && value.String() != "" {
			headers.Set("Codex-Profile-"+field, value.String())
		}
	}
	if kind := metadata.Get("request_kind").String(); kind != "" && kind != "turn" && kind != "compaction" {
		headers.Set("Codex-Profile-Request-Kind", kind)
	}
	for _, name := range []string{"X-Codex-Turn-State", "X-Codex-Turn-Metadata", "X-Codex-Window-Id"} {
		headers.Del(name)
	}
}
