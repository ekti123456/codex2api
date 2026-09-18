package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"

	"github.com/codex2api/api"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const publicUpstreamFailureMessage = "上游请求失败，请稍后重试。"

// Only fixed, protocol-relevant codes/messages are public. Provider prose,
// details and unknown codes can all contain private identifiers or credentials.
func publicErrorMessage(code string) (string, bool) {
	switch code {
	case "context_length_exceeded", "max_tokens_exceeded":
		return "The request exceeds the model context limit. Reduce the input and retry.", true
	case "previous_response_not_found", "response_not_found", "response_context_unavailable":
		return "The previous response is unavailable. Send the full conversation and retry.", true
	case "invalid_encrypted_content":
		return "Encrypted context is unavailable. Send the full conversation and retry.", true
	case "invalid_request_error", "invalid_request", "invalid_parameter", "invalid_value", "invalid_type", "unsupported_parameter", "missing_required_parameter":
		return "The upstream service rejected the request parameters.", true
	case "invalid_prompt":
		return "The upstream service rejected the prompt.", true
	case "rate_limit_exceeded", "rate_limit_reached", "rate_limit_error", "usage_limit_reached", "insufficient_quota", "account_pool_usage_limit_reached":
		return "The upstream service is temporarily rate limited. Please retry later.", true
	case "server_is_overloaded", "slow_down":
		return "Selected model is at capacity. Please try a different model.", true
	case codexEmptyIncompleteErrorCode:
		return codexEmptyIncompleteErrorMessage, true
	case "upstream_timeout":
		return "The upstream request timed out. Please retry later.", true
	case "upstream_stream_break":
		return "The upstream response was interrupted. Please retry.", true
	case "model_not_found", "unsupported_model":
		return "The requested model is unavailable.", true
	case "account_pool_deactivated", "deactivated_workspace":
		return "No available account in the pool (upstream workspace deactivated), please retry later", true
	case "invalid_auth", "authentication_error", "permission_denied", "insufficient_permissions", "missing_scope":
		return "The upstream service could not authorize this request.", true
	case "upstream_error", "server_error", "internal_error", "service_unavailable":
		return publicUpstreamFailureMessage, true
	}
	return "", false
}

func publicUpstreamAPIError(c *gin.Context, body []byte, status int, fallbackCode string) *api.APIError {
	// These constructors produce gateway-owned messages and details. Never
	// accept a provider's alleged "codex2api_safety" object as trusted metadata.
	if isUpstreamPromptSafetyRefusal(body) {
		return upstreamPromptSafetyAPIError(c, body)
	}
	if isExplicitUpstreamCyberPolicy(body) {
		code := upstreamCyberPolicyCode(responseFailedErrorBody(body))
		return api.NewAPIError(api.ErrorCode(code), upstreamPolicyUserMessage(code, false), api.ErrorTypeInvalidRequest)
	}
	parsed := gjson.ParseBytes(body)
	if promptSafetyDiagnostic(c) != nil && (parsed.Get("error.code").String() == "invalid_prompt" || parsed.Get("response.error.code").String() == "invalid_prompt") {
		return upstreamPromptSafetyAPIError(c, body)
	}
	codes := []string{}
	for _, path := range []string{"error", "response.error", "response.status_details.error", "detail", ""} {
		obj := parsed
		if path != "" {
			obj = parsed.Get(path)
		}
		code, message := strings.TrimSpace(obj.Get("code").String()), obj.Get("message").String()
		if isCodexCapacityCodeOrMessage(code, message) {
			if code != "slow_down" {
				code = "server_is_overloaded"
			}
		}
		codes = append(codes, code)
	}
	codes = append(codes, fallbackCode)
	for _, code := range codes {
		if message, ok := publicErrorMessage(code); ok {
			errorType := api.ErrorTypeUpstream
			if code == "server_is_overloaded" || code == "slow_down" {
				errorType = "service_unavailable_error"
			}
			return api.NewAPIError(api.ErrorCode(code), message, errorType)
		}
	}
	if status < 400 || status > 599 {
		status = http.StatusBadGateway
	}
	return api.NewAPIError(api.ErrorCode(fmt.Sprintf("upstream_%d", status)), publicUpstreamFailureMessage, api.ErrorTypeUpstream)
}

func publicUpstreamMessage(code string) string {
	if message, ok := publicErrorMessage(code); ok {
		return message
	}
	return publicUpstreamFailureMessage
}

// Run only on the client copy, after retry, billing and safety classification.
// This function is shared by HTTP JSON, SSE, and WebSocket event writers.
func publicResponseErrorPayload(c *gin.Context, data []byte) []byte {
	parsed := gjson.ParseBytes(data)
	if !parsed.IsObject() {
		return data
	}
	interesting := false
	parsed.ForEach(func(k, v gjson.Result) bool {
		interesting = privacyField(k.String()) == "error" || isTurnStateContainer(k.String()) || ((!responseBusinessField(k.String()) || privacyField(k.String()) == "output") && (v.IsObject() || v.IsArray())) || k.String() == "type" && v.String() == "error"
		return !interesting
	})
	if !interesting {
		return data
	}
	var walk func(json.RawMessage, bool, int) json.RawMessage
	walk = func(raw json.RawMessage, control bool, depth int) json.RawMessage {
		if depth > 64 {
			return []byte("null")
		}
		var object map[string]json.RawMessage
		if json.Unmarshal(raw, &object) != nil || object == nil {
			var items []json.RawMessage
			if json.Unmarshal(raw, &items) == nil && items != nil {
				for i, v := range items {
					items[i] = walk(v, control, depth+1)
				}
				out, _ := json.Marshal(items)
				return out
			}
			if control {
				var encoded string
				if json.Unmarshal(raw, &encoded) == nil && json.Valid([]byte(encoded)) && (strings.HasPrefix(strings.TrimSpace(encoded), "{") || strings.HasPrefix(strings.TrimSpace(encoded), "[")) {
					out, _ := json.Marshal(string(walk([]byte(encoded), true, depth+1)))
					return out
				}
			}
			return raw
		}
		if gjson.GetBytes(raw, "type").String() == "error" {
			errObj, _ := json.Marshal(publicUpstreamAPIError(c, raw, 502, "upstream_error"))
			out, _ := json.Marshal(map[string]json.RawMessage{"type": json.RawMessage(`"error"`), "error": errObj})
			return out
		}
		var kind string
		_ = json.Unmarshal(object["type"], &kind)
		for key, v := range object {
			if !control && responseOpaquePayloadField(kind, key) {
				continue
			}
			if privacyField(key) == "error" && string(v) != "null" {
				body, _ := json.Marshal(map[string]json.RawMessage{"error": v})
				object[key], _ = json.Marshal(publicUpstreamAPIError(c, body, 502, "upstream_error"))
				continue
			}
			field := privacyField(key)
			object[key] = walk(v, control || (field != "response" && field != "item" && field != "part" && field != "output"), depth+1)
		}
		out, _ := json.Marshal(object)
		return out
	}
	return walk(data, false, 0)
}
