package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func cleanSessionRestartContext(headers http.Header, body []byte, known sessionContextTokenVerifier, preserveInput ...bool) ([]byte, http.Header, *database.SessionContextCleanup, error) {
	report := &database.SessionContextCleanup{Mode: "lossy_restart", Phase: "prepared", Removed: make(map[string]int)}
	preserve := len(preserveInput) > 0 && preserveInput[0]
	if preserve {
		report.Mode = "preserve_input"
	}
	var payload map[string]json.RawMessage
	if err := json.Unmarshal(body, &payload); err != nil || payload == nil {
		return nil, nil, report, codexAccountIdentityError("换号重开无法解析请求正文。")
	}
	allowed := func(kind, value string) bool { return known != nil && known(kind, value) }
	currentPath, currentType := "", ""
	remove := func(kind string) {
		report.Removed[kind]++
		if len(report.Items) < 32 {
			report.Items = append(report.Items, database.SessionContextRemoval{Kind: kind, Path: currentPath, ItemType: currentType})
		} else {
			report.OmittedItems++
		}
	}
	for _, field := range []string{"previous_response_id", "conversation", "conversation_id"} {
		currentPath, currentType = field, ""
		value := gjson.ParseBytes(payload[field])
		if value.Exists() && value.Type != gjson.Null && value.String() != "" && !allowed(field, value.String()) {
			if preserve {
				return nil, nil, report, &Error{Code: "codex_session_failover_context_required", Type: ErrorTypeInvalidRequest, HTTPStatus: http.StatusBadRequest, Message: "完整保留 input 模式无法跨账号复用 " + field + "，请让客户端提供完整历史并移除此续写引用后重试。"}
			}
			delete(payload, field)
			remove(field)
		}
	}
	headers = headers.Clone()
	if token := headers.Get("X-Codex-Turn-State"); token != "" && !allowed("turn_state", token) {
		headers.Del("X-Codex-Turn-State")
		currentPath, currentType = "headers.X-Codex-Turn-State", ""
		remove("turn_state")
	}
	var metadata map[string]json.RawMessage
	if json.Unmarshal(payload["client_metadata"], &metadata) == nil && metadata != nil {
		if token := gjson.ParseBytes(metadata["x-codex-turn-state"]).String(); token != "" && !allowed("turn_state", token) {
			delete(metadata, "x-codex-turn-state")
			payload["client_metadata"], _ = json.Marshal(metadata)
			currentPath, currentType = "client_metadata.x-codex-turn-state", ""
			remove("turn_state")
		}
	}
	if preserve {
		return preserveSessionRestartInput(payload, headers, body, report)
	}
	var scrub func(gjson.Result, int, string) (json.RawMessage, bool)
	scrub = func(value gjson.Result, depth int, path string) (json.RawMessage, bool) {
		currentPath, currentType = path, responseContextSafeType(value)
		if depth > 64 {
			remove("unsupported_depth")
			return nil, false
		}
		if value.IsArray() {
			items := make([]json.RawMessage, 0)
			for index, item := range value.Array() {
				if cleaned, keep := scrub(item, depth+1, path+"["+strconv.Itoa(index)+"]"); keep {
					items = append(items, cleaned)
				}
			}
			raw, _ := json.Marshal(items)
			return raw, true
		}
		if !value.IsObject() {
			return json.RawMessage(value.Raw), true
		}
		kind := value.Get("type").String()
		if token := value.Get("encrypted_content"); token.Exists() && token.Type != gjson.Null && token.String() != "" && !allowed("encrypted_content", token.String()) {
			category := "encrypted_content"
			if kind == "reasoning" {
				category = "reasoning"
			}
			if kind == "compaction" || kind == "context_compaction" || kind == "compaction_summary" {
				category = "compaction"
			}
			remove(category)
			return nil, false
		}
		if kind == "item_reference" && !allowed("item_reference", value.Get("id").String()) {
			remove("item_reference")
			return nil, false
		}
		var object map[string]json.RawMessage
		_ = json.Unmarshal([]byte(value.Raw), &object)
		if token := value.Get("file_id"); token.Exists() && token.Type != gjson.Null && token.String() != "" && !allowed("file_id", token.String()) {
			remove("file_reference")
			if value.Get("file_data").String() == "" && value.Get("file_url").String() == "" && value.Get("image_url").Type != gjson.String {
				return nil, false
			}
			delete(object, "file_id")
		}
		for _, field := range responseContextChildFields(value) {
			child := gjson.ParseBytes(object[field])
			if !child.IsArray() && !child.IsObject() {
				continue
			}
			cleaned, keep := scrub(child, depth+1, path+"."+responseContextSafeField(field))
			if keep {
				object[field] = cleaned
			} else {
				delete(object, field)
			}
		}
		currentPath, currentType = path, responseContextSafeType(value)
		if responseContextMessage(value) {
			content := gjson.ParseBytes(object["content"])
			if !content.Exists() || content.IsArray() && len(content.Array()) == 0 || content.Type == gjson.String && strings.TrimSpace(content.String()) == "" {
				remove("empty_message")
				return nil, false
			}
		}
		raw, _ := json.Marshal(object)
		return raw, true
	}
	input := gjson.ParseBytes(payload["input"])
	if input.IsObject() {
		return nil, headers, report, &Error{Code: "codex_session_failover_context_required", Type: ErrorTypeInvalidRequest, HTTPStatus: http.StatusBadRequest, Message: "重建会话的 input 必须是输入项数组或文本，请检查请求格式。"}
	}
	if input.IsArray() {
		items := make([]json.RawMessage, 0)
		indices := make([]int, 0)
		for index, item := range input.Array() {
			if cleaned, keep := scrub(item, 0, "input["+strconv.Itoa(index)+"]"); keep {
				if identifier := item.Get("id").String(); identifier != "" && item.Get("type").String() != "item_reference" && !allowed("item_reference", identifier) {
					var fullItem map[string]json.RawMessage
					if json.Unmarshal(cleaned, &fullItem) == nil && fullItem != nil {
						delete(fullItem, "id")
						cleaned, _ = json.Marshal(fullItem)
						remove("item_id")
					}
				}
				items = append(items, cleaned)
				indices = append(indices, index)
			}
		}
		if gjson.ParseBytes(payload["previous_response_id"]).String() == "" {
			calls := make(map[string]bool)
			for _, item := range items {
				if strings.HasSuffix(gjson.GetBytes(item, "type").String(), "_call") {
					calls[gjson.GetBytes(item, "call_id").String()] = true
				}
			}
			paired := items[:0]
			for index, item := range items {
				currentPath, currentType = "input["+strconv.Itoa(indices[index])+"]", responseContextSafeType(gjson.ParseBytes(item))
				if strings.HasSuffix(gjson.GetBytes(item, "type").String(), "_call_output") && !calls[gjson.GetBytes(item, "call_id").String()] {
					remove("orphan_tool_output")
					continue
				}
				paired = append(paired, item)
			}
			items = paired
		}
		payload["input"], _ = json.Marshal(items)
	}
	input = gjson.ParseBytes(payload["input"])
	if gjson.ParseBytes(payload["previous_response_id"]).String() == "" && missingToolSearchCall(input) >= 0 {
		return nil, headers, report, &Error{Code: "codex_session_failover_context_required", Type: ErrorTypeInvalidRequest, HTTPStatus: http.StatusBadRequest, Message: "重建会话缺少动态工具搜索结果对应的调用，请恢复完整上下文后重试。"}
	}
	if !responseContextHasInput(input) && gjson.ParseBytes(payload["previous_response_id"]).String() == "" {
		return nil, headers, report, &Error{Code: "codex_session_failover_context_required", Type: ErrorTypeInvalidRequest, HTTPStatus: http.StatusBadRequest, Message: "清理旧账号上下文后没有可用输入，请补充当前问题和必要资料，或新开对话。"}
	}
	cleaned, err := json.Marshal(payload)
	if err == nil {
		err = checkSessionToolPreservation(body, cleaned, report)
	}
	return cleaned, headers, report, err
}

func (epoch *sessionOutboundEpoch) restartContextVerifier(ctx context.Context) (sessionContextTokenVerifier, context.CancelFunc) {
	known, cancel := epoch.handler.sessionContextVerifierForScope(ctx, sessionContextScope(epoch.owner, epoch.key, epoch.upstreamAccount, epoch.record))
	return func(kind, value string) bool {
		if kind != "previous_response_id" {
			return known(kind, value)
		}
		lookup, stop := context.WithTimeout(ctx, time.Second)
		defer stop()
		affinity, found := lookupResponseAccountAffinity(lookup, epoch.handler.cache, epoch.owner, value)
		return found && affinity.AccountID == epoch.record.AccountID && affinity.OutboundSegment == epoch.identityKey()
	}, cancel
}

func PrepareSessionRestartOutbound(ctx context.Context, account *auth.Account, body []byte, headers http.Header) ([]byte, http.Header, error) {
	var initialErr error
	body, headers, initialErr = PrepareInitialSessionOutbound(ctx, account, body, headers)
	if initialErr != nil {
		return nil, nil, initialErr
	}

	epoch := outboundEpochFromContext(ctx)
	if epoch == nil || !epoch.record.LossyContextRestart || account == nil || account.IsRelayStyle() {
		return body, headers, nil
	}
	if err := validateSessionOutboundEpoch(ctx, account); err != nil {
		return nil, nil, err
	}
	known, cancel := epoch.restartContextVerifier(ctx)
	defer cancel()
	cleaned, outgoingHeaders, report, err := cleanSessionRestartContext(headers, body, known, epoch.record.PreserveRestartInput)
	if epoch.diagnostic != nil {
		report.Phase = "outbound"
		report.Pass, report.DetailsPass = 1, 1
		if previous := epoch.diagnostic.ContextCleanup; previous != nil && previous.Phase == "outbound" {
			report.Pass = previous.Pass + 1
			report.DetailsPass = report.Pass
			// A second executor boundary may receive an already-cleaned body.
			// Preserve the last modifying pass's evidence without adding its
			// counts twice. Pass and DetailsPass make the distinction explicit.
			if err == nil && len(report.Removed) == 0 && previous.ToolsAfter != nil && report.ToolsBefore != nil && *previous.ToolsAfter == *report.ToolsBefore {
				retained := *previous
				retained.Pass = report.Pass
				report = &retained
			}
		}
		epoch.diagnostic.ContextCleanup = report
	}
	return cleaned, outgoingHeaders, err
}

func sessionRestartRoutingContext(request *gin.Context, body []byte) ([]byte, http.Header) {
	plan, _ := request.Request.Context().Value(sessionAccountFailoverContextKey{}).(*sessionAccountFailoverPlan)
	epoch := outboundEpochFromContext(request.Request.Context())
	state := continuityRequest(request)
	continuityRestart := state != nil && state.RestartReason != "" && !state.Admitted
	if plan == nil && !continuityRestart && (epoch == nil || !epoch.record.LossyContextRestart) {
		return body, sessionFailoverRequestHeaders(request)
	}
	var known sessionContextTokenVerifier
	if plan == nil && !continuityRestart {
		var cancel context.CancelFunc
		known, cancel = epoch.restartContextVerifier(request.Request.Context())
		defer cancel()
	}
	cleaned, headers, _, err := cleanSessionRestartContext(sessionFailoverRequestHeaders(request), body, known, PreserveSessionInput(request.Request.Context()))
	if err != nil {
		return body, sessionFailoverRequestHeaders(request)
	}
	return cleaned, headers
}
