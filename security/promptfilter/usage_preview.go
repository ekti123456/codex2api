package promptfilter

import (
	"strings"

	"github.com/tidwall/gjson"
)

// UsageUserPreview reuses the protocol's current-input selection without
// running moderation or traversing historical prompt text. Only the short
// display excerpt is retained by callers.
func UsageUserPreview(body []byte) (string, bool) {
	input := gjson.GetBytes(body, "input")
	if !input.Exists() {
		return "", false
	}
	items := []gjson.Result{input}
	if input.IsArray() {
		items = input.Array()
	}
	selected := selectCurrentInputItems(items)
	var parts []string
	attachment := false
	var collect func(gjson.Result)
	collect = func(value gjson.Result) {
		if value.Type == gjson.String {
			parts = append(parts, value.String())
			return
		}
		if value.IsArray() {
			value.ForEach(func(_, part gjson.Result) bool { collect(part); return true })
			return
		}
		switch value.Get("type").String() {
		case "input_image", "image_url", "image", "input_file", "file":
			attachment = true
		case "input_text", "text":
			collect(value.Get("text"))
		}
	}
	found := false
	for i, current := range selected {
		if !current {
			continue
		}
		found = true
		item := items[i]
		if item.Type == gjson.String {
			collect(item)
		} else if content := item.Get("content"); content.Exists() {
			collect(content)
		} else {
			collect(item)
		}
	}
	if !found {
		return "", false
	}
	text := strings.TrimSpace(strings.ReplaceAll(strings.Join(parts, "\n"), "\r\n", "\n"))
	// Codex's attachment wrapper puts the actual request after this heading.
	for _, heading := range []string{"\n## My request:\n", "\n# My request:\n"} {
		if _, request, ok := strings.Cut(text, heading); ok {
			text = strings.TrimSpace(request)
			break
		}
	}
	if strings.HasPrefix(text, "You are performing a CONTEXT CHECKPOINT COMPACTION.") ||
		strings.HasPrefix(text, "<environment_context>") || strings.HasPrefix(text, "<turn_aborted>") {
		return "", false
	}
	text = strings.Join(strings.Fields(RedactSensitive(text)), " ")
	if text == "" {
		if attachment {
			return "图片或附件消息", true
		}
		return "", false
	}
	runes := []rune(text)
	if len(runes) > 20 {
		text = string(runes[:20]) + "…"
	}
	return text, true
}
