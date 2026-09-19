package proxy

import (
	"bytes"
	"context"
	"encoding/json"
	"net/http"
	"reflect"
	"strings"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

const preservedInputSnapshotKey = "session_preserved_input_snapshot"

type preservedInputPreparedKey struct{}

// Restore the original input once after protocol translation, before payload
// rules. Final outbound input consistency enforcement is temporarily disabled.
func PreparePreservedInputTransport(ctx context.Context, body []byte) (context.Context, []byte) {
	if !PreserveSessionInput(ctx) || ctx.Value(preservedInputPreparedKey{}) != nil {
		return ctx, body
	}
	epoch := outboundEpochFromContext(ctx)
	if epoch == nil || len(epoch.preservedInput) == 0 {
		return ctx, body
	}
	updated, err := sjson.SetRawBytes(body, "input", epoch.preservedInput)
	if err != nil {
		return ctx, body
	}
	return context.WithValue(ctx, preservedInputPreparedKey{}, true), updated
}

func ValidateSessionOutboundRequest(ctx context.Context, account *auth.Account, body []byte) error {
	if !gjson.ValidBytes(body) || !gjson.ParseBytes(body).IsObject() {
		return codexAccountIdentityError("出站请求正文无效，已停止发送。")
	}
	if err := ValidateBackgroundAccountMatch(ctx, account); err != nil {
		return err
	}
	// TEMPORARY: allow history identity rewrites while the preserve-input
	// conflict is investigated. Restore ValidatePreservedSessionInput afterward.
	return nil
}

// PreserveSessionInput follows the committed segment, not a live setting that
// could silently downgrade an existing conversation after a reload.
func PreserveSessionInput(ctx context.Context) bool {
	if ctx == nil {
		return false
	}
	if plan, _ := ctx.Value(sessionAccountFailoverContextKey{}).(*sessionAccountFailoverPlan); plan != nil && plan.PreserveInput {
		return true
	}
	epoch := outboundEpochFromContext(ctx)
	return epoch != nil && epoch.record.PreserveRestartInput
}

func preserveInputError(message string) *Error {
	return &Error{Code: "codex_session_failover_context_required", Type: ErrorTypeInvalidRequest, HTTPStatus: http.StatusBadRequest, Message: message}
}

func preserveSessionRestartInput(payload map[string]json.RawMessage, headers http.Header, original []byte, report *database.SessionContextCleanup) ([]byte, http.Header, *database.SessionContextCleanup, error) {
	input := gjson.ParseBytes(payload["input"])
	hasPrevious := gjson.ParseBytes(payload["previous_response_id"]).String() != ""
	if !hasPrevious && !(input.Type == gjson.String && strings.TrimSpace(input.String()) != "" || input.IsArray() && len(input.Array()) > 0) {
		return nil, headers, report, preserveInputError("完整保留 input 模式需要客户端提供完整输入数组或文本，不能仅依赖旧账号的续写状态。")
	}
	if input.IsArray() && !hasPrevious {
		pairing, missingOutput := inspectPreservedToolPairing(input)
		if pairing != nil {
			report.ToolPairing = pairing
			report.ToolsBefore = summarizeSessionTools(original)
			if missingOutput {
				return nil, headers, report, preserveInputError("完整保留 input 模式发现工具结果缺少对应调用，请恢复完整历史后重试；不会删除工具结果重试。")
			}
			return nil, headers, report, preserveInputError("完整保留 input 模式缺少动态工具搜索结果对应的调用，请恢复完整历史后重试。")
		}
	}
	// RawMessage avoids converting tool schema integers through float64.
	cleaned, err := json.Marshal(payload)
	if err == nil {
		err = checkSessionToolPreservation(original, cleaned, report)
	}
	return cleaned, headers, report, err
}

// ValidatePreservedSessionInput catches any later input rewrite before send.
// The snapshot is request-local; no prompt or ciphertext is persisted here.
func ValidatePreservedSessionInput(ctx context.Context, body []byte) error {
	if !PreserveSessionInput(ctx) {
		return nil
	}
	epoch := outboundEpochFromContext(ctx)
	if epoch == nil || len(epoch.preservedInput) == 0 {
		return nil
	}
	actual := gjson.GetBytes(body, "input").Raw
	if actual == string(epoch.preservedInput) {
		return nil
	}
	decode := func(raw []byte) (any, error) {
		decoder := json.NewDecoder(bytes.NewReader(raw))
		decoder.UseNumber()
		var value any
		err := decoder.Decode(&value)
		return value, err
	}
	want, first := decode(epoch.preservedInput)
	got, second := decode([]byte(actual))
	if first == nil && second == nil && reflect.DeepEqual(want, got) {
		return nil
	}
	return preserveInputError("完整保留 input 模式检测到出站输入被其他处理修改，已停止发送；请检查输入改写规则或恢复完整上下文。")
}
