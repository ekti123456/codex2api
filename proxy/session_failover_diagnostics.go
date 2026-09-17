package proxy

import (
	"encoding/json"
	"net/http"
	"slices"
	"strings"

	"github.com/codex2api/api"
	"github.com/gin-gonic/gin"
)

const sessionFailoverUnavailableMessage = "暂无可用账号。请稍后手动重试或联系管理员。"

func failoverSelectionLabels(ctx *gin.Context, groups []int64, tags []string) ([]int64, []string, bool) {
	groups = slices.Clone(groups)
	slices.Sort(groups)
	groups = slices.Compact(groups)
	labels := make([]string, 0, len(tags))
	for _, tag := range tags {
		if tag = strings.TrimSpace(tag); tag != "" {
			labels = append(labels, tag)
		}
	}
	slices.Sort(labels)
	labels = slices.Compact(labels)
	truncated := len(groups) > 32 || len(labels) > 32
	groups, labels = groups[:min(len(groups), 32)], labels[:min(len(labels), 32)]
	for i, tag := range labels {
		labels[i] = serviceErrorSafeText(ctx, tag, 128)
		truncated = truncated || len(tag) > 128
	}
	return groups, labels, truncated
}

func sessionFailoverNoCandidate(ctx *gin.Context) bool {
	if ctx == nil || ctx.Request == nil {
		return false
	}
	plan, _ := ctx.Request.Context().Value(sessionAccountFailoverContextKey{}).(*sessionAccountFailoverPlan)
	return plan != nil && plan.Diagnostic != nil && plan.Diagnostic.Result == "no_safe_candidate"
}

func sessionFailoverUnavailableAPIError(ctx *gin.Context) *api.APIError {
	return api.NewAPIErrorWithDetails(api.ErrCodeNoAvailableAccount, sessionFailoverUnavailableMessage, api.ErrorTypeInvalidRequest,
		gin.H{"request_id": diagnosticRequestID(snapshotUpstreamTrace(ctx.Request.Context()).RequestID), "retryable": false})
}

func sendSessionFailoverUnavailable(ctx *gin.Context, stream, chat bool) {
	protocol := continuousRetryProtocolResponses
	if chat {
		protocol = continuousRetryProtocolChat
	}
	if !claimContinuousRetryTerminal(ctx, protocol) {
		return
	}
	failure := sessionFailoverUnavailableAPIError(ctx)
	api.ObserveError(ctx, http.StatusBadRequest, failure)
	if !ctx.Writer.Written() {
		ctx.Header("X-Should-Retry", "false")
		ctx.JSON(http.StatusBadRequest, api.ErrorResponse{Error: *failure})
		return
	}
	if stream {
		payload := gin.H{"error": failure}
		if !chat {
			payload = gin.H{"type": "response.failed", "response": gin.H{"status": "failed", "error": failure}}
		}
		body, _ := json.Marshal(payload)
		_, _ = ctx.Writer.WriteString("data: " + string(body) + "\n\n")
		ctx.Writer.Flush()
	}
}
