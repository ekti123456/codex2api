package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

func (h *Handler) sessionModelGuidance(c *gin.Context, key, original, effective string, compact bool) string {
	// Only repeat the client's public model name, never a mapped provider ID.
	model := strings.TrimSpace(original)
	if model == "" || len(model) > 80 || strings.ContainsAny(model, "\r\n\t") {
		return sessionModelUnavailableMessage
	}
	next := "请选择其他可用模型继续当前任务"
	if alternative := h.sessionModelAlternative(c, key, model, effective, compact); alternative != "" {
		next = "可尝试切换至 " + alternative + " 继续当前任务"
	}
	return fmt.Sprintf("当前对话无法继续使用 %s。%s；如需使用 %s，请新建对话后重试。", model, next, model)
}

func (h *Handler) sessionModelAlternative(c *gin.Context, key, original, effective string, compact bool) string {
	const candidate = "gpt-5.6-sol"
	row := apiKeyRowFromContext(c)
	if h == nil || h.store == nil || row == nil || !strings.HasPrefix(strings.ToLower(original), "gpt-") || strings.EqualFold(original, candidate) {
		return ""
	}
	channel := requestUpstreamChannel(c)
	if channel != database.UpstreamChannelAuto && channel != database.UpstreamChannelCodex {
		return ""
	}
	trace := selectionTraceForRequest(c)
	if trace == nil {
		return ""
	}
	owner := h.store.FindByID(trace.Snapshot().RootAccount)
	if owner == nil || owner.IsRelayStyle() || !owner.IsAvailable() || !h.accountVisibleToAPIKey(owner, row.ID, time.Now()) {
		return ""
	}
	// An explicit account whitelist is evidence of support. An empty list means
	// permissive routing, not proof that an alternative is actually supported.
	if len(owner.CodexModels()) == 0 || checkAPIKeyModel(candidate, row.Limits) != "" {
		return ""
	}
	supported := h.supportedModelIDs(c.Request.Context())
	mapped, _ := h.resolveConfiguredRequestModel(candidate, supported)
	if strings.EqualFold(mapped, effective) || !modelIDInList(mapped, supported) || checkAPIKeyModel(mapped, row.Limits) != "" {
		return ""
	}
	if !h.withModelCooldownFilter(mapped, accountFilterForModel(mapped))(owner) || !sessionModelSupportFilter(candidate, mapped, compact)(owner) {
		return ""
	}
	if key == "" || !h.store.CanAdmitAccountSession(owner, key, time.Now()) {
		return ""
	}
	return candidate
}

// Gateway-owned session guidance must survive committed SSE without going
// through the sanitizer for untrusted provider error prose.
func sendSessionModelError(c *gin.Context, failure *api.APIError, protocol continuousRetryHTTPProtocol) {
	if !claimContinuousRetryTerminal(c, protocol) {
		return
	}
	api.ObserveError(c, http.StatusBadRequest, failure)
	payload := gin.H{"error": failure}
	if protocol == continuousRetryProtocolAnthropic {
		payload["type"] = "error"
	}
	if !c.Writer.Written() {
		c.Header("X-Should-Retry", "false")
		c.JSON(http.StatusBadRequest, payload)
		return
	}
	if protocol == continuousRetryProtocolResponses {
		payload = gin.H{"type": "response.failed", "response": gin.H{"status": "failed", "error": failure}}
	}
	encoded, _ := json.Marshal(payload)
	if protocol == continuousRetryProtocolAnthropic {
		_, _ = c.Writer.WriteString("event: error\n")
	}
	_, _ = c.Writer.WriteString("data: " + string(encoded) + "\n\n")
	c.Writer.Flush()
}
