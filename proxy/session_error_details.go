package proxy

import (
	"fmt"
	"strings"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

// Only extract error fields from response JSON; never persist an entire body.
// Usage messages are already sanitized by the usage logger and may be plain
// transport diagnostics or its "code · type · message" representation.
func parseSessionErrorMessage(text string, usage bool) *api.APIError {
	text = strings.TrimSpace(text)
	if text == "" {
		return nil
	}
	if gjson.Valid(text) {
		root := gjson.Parse(text)
		for _, path := range []string{"error", "response.error", "response.status_details.error", ""} {
			value := root
			if path != "" {
				value = root.Get(path)
			}
			code, message, kind := value.Get("code"), value.Get("message"), value.Get("type")
			if path == "" && message.Type != gjson.String && root.Get("error").Type == gjson.String {
				message = root.Get("error")
			}
			if message.Type != gjson.String && code.Type != gjson.String && code.Type != gjson.Number {
				continue
			}
			failure := &api.APIError{}
			if code.Type == gjson.String || code.Type == gjson.Number {
				failure.Code = api.ErrorCode(code.String())
			}
			if message.Type == gjson.String {
				failure.Message = message.String()
			}
			if kind.Type == gjson.String && kind.String() != "error" {
				failure.Type = api.ErrorType(kind.String())
			}
			return failure
		}
	}
	if !usage {
		return nil
	}
	failure := &api.APIError{Message: text}
	if code, rest, ok := strings.Cut(text, " · "); ok && sessionErrorLabel(code) {
		failure.Code, failure.Message = api.ErrorCode(code), rest
		if kind, message, ok := strings.Cut(rest, " · "); ok && sessionErrorLabel(kind) && strings.HasSuffix(kind, "_error") {
			failure.Type, failure.Message = api.ErrorType(kind), message
		}
	}
	return failure
}

func sessionErrorLabel(value string) bool {
	if value == "" || len(value) > 128 {
		return false
	}
	for _, c := range value {
		if !(c >= 'a' && c <= 'z' || c >= 'A' && c <= 'Z' || c >= '0' && c <= '9' || c == '_' || c == '-' || c == '.') {
			return false
		}
	}
	return true
}

func sessionErrorDetails(ctx *gin.Context, state *serviceErrorAudit, status int, reported *api.APIError) (*api.APIError, *database.SessionErrorDiagnostics) {
	state.usageMu.Lock()
	defer state.usageMu.Unlock()
	diagnostic := &database.SessionErrorDiagnostics{
		StatusCode: status, StatusSource: "reported_error", ErrorSource: "reported_error",
		UsageCaptured: state.finalUsageStatus != 0, UsageStatus: state.finalUsageStatus,
		UsageRequestID: state.finalUsageRequestID, UsageErrorMessage: state.finalUsageMessage,
		UpstreamErrorKind: state.finalUsageErrorKind, ObservedStatus: state.observedStatus, ResponseStatus: state.responseStatus,
		ObservedError: sessionErrorDetail(ctx, state.observedError), ResponseError: sessionErrorDetail(ctx, state.responseError),
	}
	if state.finalUsageStatus == status {
		diagnostic.StatusSource = "final_usage"
	} else if state.observedStatus == status {
		diagnostic.StatusSource = "error_observer"
	} else if state.responseStatus == status || !state.websocket && ctx.Writer.Status() == status {
		diagnostic.StatusSource = "http_response"
	}
	failure := reported
	if state.responseStatus == status && state.responseError != nil {
		failure, diagnostic.ErrorSource = state.responseError, "http_response"
	}
	if state.observedStatus == status && state.observedError != nil {
		failure, diagnostic.ErrorSource = state.observedError, "error_observer"
	}
	if state.finalUsageStatus == status {
		if usage := parseSessionErrorMessage(state.finalUsageMessage, true); usage != nil && usage.Message != fmt.Sprintf("HTTP %d", status) {
			// Never attach an earlier retry's code to a different final message.
			if failure != nil && (usage.Message == failure.Message || usage.Code != "" && usage.Code == failure.Code) {
				if usage.Code == "" {
					usage.Code = failure.Code
				}
				if usage.Type == "" {
					usage.Type = failure.Type
				}
			}
			failure, diagnostic.ErrorSource = usage, "final_usage"
		}
	}
	if failure == nil {
		failure = &api.APIError{}
	}
	result := *failure
	result.Details = nil
	if result.Code == "" {
		result.Code = api.ErrorCode(fmt.Sprintf("http_%d", status))
	}
	diagnostic.ErrorCodeFallback = string(result.Code) == fmt.Sprintf("http_%d", status)
	if result.Message == "" {
		result.Message = fmt.Sprintf("请求返回 HTTP %d", status)
	}
	if string(result.Code) == fmt.Sprintf("http_%d", status) && (result.Message == fmt.Sprintf("请求返回 HTTP %d", status) || result.Message == "Service request rejected") {
		diagnostic.ErrorSource = "http_status_fallback"
	}
	return &result, diagnostic
}

func sessionErrorDetail(ctx *gin.Context, failure *api.APIError) *database.SessionErrorDetail {
	if failure == nil {
		return nil
	}
	return &database.SessionErrorDetail{
		Code:    serviceErrorSafeText(ctx, string(failure.Code), 128),
		Message: serviceErrorSafeText(ctx, failure.Message, 2048),
		Type:    serviceErrorSafeText(ctx, string(failure.Type), 128),
	}
}
