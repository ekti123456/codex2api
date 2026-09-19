package wsrelay

import (
	"encoding/json"
	"fmt"
	"strings"

	"github.com/codex2api/proxy"
	"github.com/tidwall/gjson"
)

// Rewrite protocol stream references, including echoes in response metadata.
// Text deltas and tool content are business data, not protocol identity carriers.
func restoreStreamIdentity(raw []byte, original, upstream string, control bool, depth int) ([]byte, error) {
	if depth > 64 {
		return nil, fmt.Errorf("websocket stream metadata too deep")
	}
	value := gjson.ParseBytes(raw)
	if value.Type == gjson.String && control && gjson.Valid(value.String()) {
		decoded := gjson.Parse(value.String())
		if decoded.IsObject() || decoded.IsArray() {
			out, err := restoreStreamIdentity([]byte(value.String()), original, upstream, true, depth+1)
			if err != nil {
				return nil, err
			}
			return json.Marshal(string(out))
		}
	}
	if value.Type == gjson.String && control && upstream != "" && strings.Contains(value.String(), upstream) {
		// Error messages can quote a lane ID without using a stream_id field.
		return json.Marshal(strings.ReplaceAll(value.String(), upstream, original))
	}
	if value.IsArray() {
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, err
		}
		for i := range items {
			out, err := restoreStreamIdentity(items[i], original, upstream, control, depth+1)
			if err != nil {
				return nil, err
			}
			items[i] = out
		}
		return json.Marshal(items)
	}
	if !value.IsObject() {
		return raw, nil
	}
	var fields map[string]json.RawMessage
	if err := json.Unmarshal(raw, &fields); err != nil {
		return nil, err
	}
	for key, rawValue := range fields {
		field := strings.ToLower(strings.NewReplacer("_", "", "-", "").Replace(key))
		if !control && (proxy.ResponseOpaquePayloadField(value.Get("type").String(), key) || proxy.ResponseToolErrorField(value.Get("type").String(), key)) {
			continue
		}
		if field == "streamid" {
			id := gjson.ParseBytes(rawValue)
			if id.Type == gjson.Null {
				continue
			}
			if upstream == "" || id.Type != gjson.String || id.String() != upstream {
				return nil, fmt.Errorf("unexpected websocket stream_id")
			}
			fields[key], _ = json.Marshal(original)
			continue
		}
		childControl := control || (field != "response" && field != "output" && field != "item" && field != "part")
		out, err := restoreStreamIdentity(rawValue, original, upstream, childControl, depth+1)
		if err != nil {
			return nil, err
		}
		fields[key] = out
	}
	return json.Marshal(fields)
}
