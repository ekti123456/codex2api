package proxy

import (
	"strings"

	"github.com/codex2api/api"
	"github.com/gin-gonic/gin"
)

func (handler *Handler) validateRequestNamingRoot(request *gin.Context, body []byte) *api.APIError {
	identity := handler.resolveRequestRootSessionIdentityForContext(request, body)
	if strings.EqualFold(strings.TrimSpace(identity.requestKind), "compaction") || (request.Request != nil && request.Request.URL != nil && isCompactUsageEndpoint(request.Request.URL.Path)) {
		return nil
	}
	switch strings.ToLower(strings.TrimSpace(identity.threadSource)) {
	case "thread_title", "thread_title_reconsideration", "thread_description":
	default:
		return nil
	}
	if !identity.stable || identity.conflict || identity.sessionID == "" {
		return api.NewAPIError(api.ErrCodeBackgroundRootUnavailable, "命名请求没有有效主根，已停止请求。", api.ErrorTypeInvalidRequest)
	}
	return nil
}
