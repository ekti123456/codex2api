package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"errors"
	"regexp"
	"strings"

	"github.com/codex2api/auth"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

var responseIDInError = regexp.MustCompile(`\bresp_[A-Za-z0-9_-]+`)

// Whitelist only protocol-consumed headers. Trace, organization, account,
// cookies, quota/reset and unknown extension headers are not public metadata.
// Safety/moderation/verification metadata lives outside these dictionaries and
// is deliberately preserved, as are model and usage semantics.
func maskResponseHeaders(ctx context.Context, account *auth.Account, raw json.RawMessage) (json.RawMessage, error) {
	if len(raw) == 0 || bytes.Equal(raw, []byte("null")) {
		return raw, nil
	}
	var headers map[string]json.RawMessage
	if json.Unmarshal(raw, &headers) != nil || headers == nil {
		return []byte(`{}`), nil
	}
	for key, value := range headers {
		switch strings.ToLower(key) {
		case "x-codex-turn-state":
			var real string
			array := false
			if json.Unmarshal(value, &real) != nil {
				var values []string
				if json.Unmarshal(value, &values) != nil || len(values) != 1 {
					delete(headers, key)
					continue
				}
				real, array = values[0], true
			}
			state := turnStateSessionFrom(ctx)
			if state == nil {
				delete(headers, key)
				continue
			}
			alias, err := state.issue(ctx, account, real, "response_metadata")
			if err != nil {
				return nil, err
			}
			if alias == "" {
				delete(headers, key)
			} else if array {
				headers[key], _ = json.Marshal([]string{alias})
			} else {
				headers[key], _ = json.Marshal(alias)
			}
		case "openai-model", "x-openai-model", "x-reasoning-included":
			// Preserve type for the official header parser (string or array).
		default:
			delete(headers, key)
		}
	}
	return json.Marshal(headers)
}

// Touch protocol envelopes only, never arbitrary tool arguments, generated
// text, output item IDs or input history. This also covers compact/non-SSE JSON.
func maskResponsePayload(ctx context.Context, account *auth.Account, data []byte, jsonResponse bool) ([]byte, error) {
	parsed := gjson.ParseBytes(data)
	interesting := jsonResponse
	if !interesting && parsed.IsObject() {
		// Scan top-level keys once. Repeated path lookups would rescan a large
		// text/tool delta many times even though it requires no transformation.
		parsed.ForEach(func(key, value gjson.Result) bool {
			switch key.String() {
			case "headers", "response", "response_id", "previous_response_id", "error":
				interesting = true
			case "metadata":
				interesting = value.Get("headers").Exists()
			case "message", "detail":
				interesting = parsed.Get("type").String() == "error"
			}
			return !interesting
		})
	}
	if !interesting {
		return data, nil
	}
	var payload map[string]json.RawMessage
	if json.Unmarshal(data, &payload) != nil || payload == nil {
		return nil, errors.New("invalid upstream response envelope")
	}
	state := responseIdentityFrom(ctx)
	var maskObject func(map[string]json.RawMessage, bool) error
	maskObject = func(object map[string]json.RawMessage, responseObject bool) error {
		if raw, ok := object["headers"]; ok {
			masked, err := maskResponseHeaders(ctx, account, raw)
			if err != nil {
				return err
			}
			object["headers"] = masked
		}
		if raw, ok := object["metadata"]; ok {
			var metadata map[string]json.RawMessage
			if json.Unmarshal(raw, &metadata) == nil && metadata != nil {
				if headers, found := metadata["headers"]; found {
					masked, err := maskResponseHeaders(ctx, account, headers)
					if err != nil {
						return err
					}
					metadata["headers"] = masked
					object["metadata"], _ = json.Marshal(metadata)
				}
			}
		}
		fields := []string{"response_id", "previous_response_id"}
		if responseObject {
			fields = append(fields, "id")
		}
		if state != nil {
			for _, field := range fields {
				raw, ok := object[field]
				if !ok || bytes.Equal(raw, []byte("null")) {
					continue
				}
				var real string
				if json.Unmarshal(raw, &real) != nil {
					return errors.New("invalid upstream response ID")
				}
				if real == "" {
					continue
				}
				alias, err := state.issue(ctx, account, real)
				if err != nil {
					return err
				}
				object[field], _ = json.Marshal(alias)
			}
		}
		return nil
	}
	if err := maskObject(payload, jsonResponse); err != nil {
		return nil, err
	}
	if raw, ok := payload["response"]; ok && !bytes.Equal(raw, []byte("null")) {
		var response map[string]json.RawMessage
		if json.Unmarshal(raw, &response) != nil || response == nil {
			return nil, errors.New("invalid upstream response")
		}
		if err := maskObject(response, true); err != nil {
			return nil, err
		}
		payload["response"], _ = json.Marshal(response)
	}
	encoded, err := json.Marshal(payload)
	if err != nil || state == nil {
		return encoded, err
	}
	// Upstream not-found errors sometimes quote the original response ID. Only
	// sanitize error strings; a generated answer/tool argument is not a header.
	for _, path := range []string{"error.message", "response.error.message", "response.status_details.error.message", "message", "detail"} {
		value := gjson.GetBytes(encoded, path)
		if value.Type != gjson.String || !responseIDInError.MatchString(value.String()) {
			continue
		}
		original := value.String()
		var mappingErr error
		masked := responseIDInError.ReplaceAllStringFunc(original, func(real string) string {
			alias, issueErr := state.publicErrorReference(ctx, account, real)
			if issueErr != nil {
				mappingErr = issueErr
			}
			return alias
		})
		if mappingErr != nil {
			return nil, mappingErr
		}
		state.log(responseIdentityEvent{Action: "masked_error_reference", OriginalError: original})
		encoded, err = sjson.SetBytes(encoded, path, masked)
		if err != nil {
			return nil, err
		}
	}
	return encoded, nil
}
