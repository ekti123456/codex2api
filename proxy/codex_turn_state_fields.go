package proxy

import (
	"bytes"
	"encoding/json"
	"net/http"
	"sort"
	"strings"

	"github.com/tidwall/gjson"
)

type turnStateReplacer func(value, carrier string) (string, error)

func isTurnStateField(key string) bool {
	return strings.EqualFold(strings.ReplaceAll(key, "_", "-"), "x-codex-turn-state")
}

func isTurnStateContainer(key string) bool {
	switch strings.ToLower(strings.ReplaceAll(key, "_", "-")) {
	case "headers", "metadata", "client-metadata", "x-codex-turn-metadata":
		return true
	}
	return false
}

// Only protocol envelopes and their metadata are traversed. Input/output,
// generated text, tool arguments and schemas are never interpreted as state.
// RawMessage preserves unrelated payloads and integer precision. Duplicate keys
// are normalized before both validation and rewriting, using the same semantics.
func rewriteTurnStateFields(raw []byte, carrier string, control bool, depth int, replace turnStateReplacer) ([]byte, bool, error) {
	if depth > 64 {
		return nil, true, nil // discard an uninspectable control subtree
	}
	value := gjson.ParseBytes(raw)
	if value.Type == gjson.String && control {
		decoded := strings.TrimSpace(value.String())
		if !strings.HasPrefix(decoded, "{") && !strings.HasPrefix(decoded, "[") {
			return raw, false, nil
		}
		if !gjson.Valid(decoded) {
			return nil, true, nil
		}
		out, changed, err := rewriteTurnStateFields([]byte(decoded), carrier, true, depth+1, replace)
		if err != nil || out == nil {
			return out, changed, err
		}
		if !changed {
			return raw, false, nil
		}
		encoded, err := json.Marshal(string(out))
		return encoded, true, err
	}
	if value.IsArray() && control {
		var items []json.RawMessage
		if err := json.Unmarshal(raw, &items); err != nil {
			return nil, true, nil
		}
		changed := false
		for i, item := range items {
			out, modified, err := rewriteTurnStateFields(item, carrier+"[]", true, depth+1, replace)
			if err != nil {
				return nil, false, err
			}
			if modified {
				changed = true
				if out == nil {
					out = []byte("null")
				}
				items[i] = out
			}
		}
		if !changed {
			return raw, false, nil
		}
		out, err := json.Marshal(items)
		return out, true, err
	}
	if !value.IsObject() {
		return raw, false, nil
	}
	var object map[string]json.RawMessage
	if err := json.Unmarshal(raw, &object); err != nil {
		return nil, false, err
	}
	count := 0
	value.ForEach(func(_, _ gjson.Result) bool { count++; return true })
	changed := count != len(object)
	keys := make([]string, 0, len(object))
	for key := range object {
		keys = append(keys, key)
	}
	sort.Strings(keys)
	for _, key := range keys {
		item := object[key]
		path := carrier + "." + key
		if isTurnStateField(key) {
			out, err := rewriteTurnStateValue(item, path, replace)
			if err != nil {
				return nil, false, err
			}
			if out == nil {
				delete(object, key)
				changed = true
			} else if !bytes.Equal(item, out) {
				object[key] = out
				changed = true
			}
			continue
		}
		childControl := control || isTurnStateContainer(key)
		if !childControl && !strings.EqualFold(key, "response") {
			continue
		}
		// These are data even when embedded in protocol metadata.
		switch strings.ToLower(key) {
		case "input", "output", "content", "arguments", "tools", "parameters", "schema", "text", "delta":
			continue
		}
		out, modified, err := rewriteTurnStateFields(item, path, childControl, depth+1, replace)
		if err != nil {
			return nil, false, err
		}
		if modified {
			changed = true
			if out == nil {
				delete(object, key)
			} else {
				object[key] = out
			}
		}
	}
	if !changed {
		return raw, false, nil
	}
	out, err := json.Marshal(object)
	return out, true, err
}

func rewriteTurnStateValue(raw []byte, carrier string, replace turnStateReplacer) ([]byte, error) {
	value := gjson.ParseBytes(raw)
	array := value.IsArray()
	if array {
		values := value.Array()
		if len(values) != 1 {
			return nil, nil
		}
		value = values[0]
	}
	if value.Type != gjson.String || strings.TrimSpace(value.String()) == "" {
		return nil, nil
	}
	replacement, err := replace(value.String(), carrier)
	if err != nil || replacement == "" {
		return nil, err
	}
	if array {
		return json.Marshal([]string{replacement})
	}
	return json.Marshal(replacement)
}

func rewriteTurnStateHeaders(headers http.Header, carrier string, replace turnStateReplacer) (http.Header, error) {
	out := headers.Clone()
	var stateValues []string
	for key, values := range headers {
		if isTurnStateField(key) {
			delete(out, key)
			if len(values) == 0 {
				continue
			}
			mapped := make([]string, 0, len(values))
			for _, value := range values {
				if strings.TrimSpace(value) == "" {
					continue
				}
				replacement, err := replace(value, carrier+"."+key)
				if err != nil {
					return nil, err
				}
				if replacement != "" {
					mapped = append(mapped, replacement)
				}
			}
			if len(mapped) > 0 {
				stateValues = append(stateValues, mapped...)
			}
		} else if isTurnStateContainer(key) {
			cleaned := make([]string, 0, len(values))
			for _, value := range values {
				if !gjson.Valid(value) {
					continue
				}
				raw, _, err := rewriteTurnStateFields([]byte(value), carrier+"."+key, true, 0, replace)
				if err != nil {
					return nil, err
				}
				if raw != nil {
					cleaned = append(cleaned, string(raw))
				}
			}
			if len(cleaned) == 0 {
				delete(out, key)
			} else {
				out[key] = cleaned
			}
		}
	}
	if len(stateValues) > 0 {
		out[http.CanonicalHeaderKey(codexTurnStateHeader)] = stateValues
	}
	return out, nil
}

// ClearCodexTurnStateHeaders is also used by WS handshake/frame reset paths.
// It removes state inside metadata headers as well as the dedicated header.
func ClearCodexTurnStateHeaders(headers http.Header) {
	cleaned, _ := rewriteTurnStateHeaders(headers, "request_header", func(string, string) (string, error) { return "", nil })
	for key := range headers {
		delete(headers, key)
	}
	for key, values := range cleaned {
		headers[key] = values
	}
}

func rewriteRequestTurnState(body []byte, headers http.Header, replace func(string, string) string) ([]byte, http.Header) {
	mapper := func(value, carrier string) (string, error) { return replace(value, carrier), nil }
	headers, _ = rewriteTurnStateHeaders(headers, "request_header", mapper)
	if len(body) == 0 {
		return body, headers
	}
	out, _, err := rewriteTurnStateFields(body, "request_metadata", false, 0, mapper)
	if err != nil {
		return nil, headers
	}
	return out, headers
}
