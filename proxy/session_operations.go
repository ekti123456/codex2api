package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

const sessionOperationsContextKey = "session_operations_identity"

func sessionOperationKey(kind, platform, userID, root string) string {
	digest := sha256.Sum256([]byte(strings.Join([]string{"session-operations-v1", kind, platform, userID, root}, "\x00")))
	return hex.EncodeToString(digest[:])
}

func (handler *Handler) captureSessionOperationsIdentity(ctx *gin.Context, body []byte, root requestRootSessionIdentity, policy verifiedNewAPIPolicyContext, verified bool) {
	ctx.Set(sessionOperationsContextKey, nil)
	if !root.stable || root.conflict || root.sessionID == "" {
		return
	}
	identity := database.SessionErrorIdentity{}
	parent := ""
	if verified && policy.Identity.UserID != "" {
		identity.Kind, identity.Platform, identity.UserID = "newapi", policy.Platform, policy.Identity.UserID
		identity.UserLabel = serviceErrorSafeText(ctx, policy.Meta.UserName, 160)
		identity.Fingerprint = root.fingerprint
		if canonical := strings.TrimSpace(policy.Meta.RootSessionID); canonical != "" && equalRootSessionFingerprint(root.fingerprint, newAPIRootSessionFingerprint(policy.Platform, policy.Identity.UserID, canonical)) {
			identity.SessionID = diagnosticIdentifier(canonical)
		}
		if identity.SessionID == "" {
			headers := ctx.Request.Header
			if isResponsesWebSocketUpgradeRequest(ctx.Request) {
				headers = nil
			}
			original := resolveRequestRootSessionIdentity(headers, body)
			if original.stable && !original.conflict && equalRootSessionFingerprint(root.fingerprint, newAPIRootSessionFingerprint(policy.Platform, policy.Identity.UserID, original.sessionID)) {
				identity.SessionID = diagnosticIdentifier(original.sessionID)
			}
		}
		parent = policy.Meta.ForkedFromSessionFingerprint
		if parent == "" && root.forkedFromSessionID != "" {
			parent = newAPIRootSessionFingerprint(policy.Platform, policy.Identity.UserID, root.forkedFromSessionID)
		}
	} else {
		keyID := requestAPIKeyID(ctx)
		if keyID <= 0 {
			return
		}
		identity.Kind, identity.Platform, identity.UserID = "api_key", "codex-local", strconv.FormatInt(keyID, 10)
		identity.UserLabel = serviceErrorSafeText(ctx, ctx.GetString("apiKeyName"), 160)
		identity.SessionID, identity.Fingerprint = diagnosticIdentifier(root.sessionID), diagnosticIdentifier(root.sessionID)
		parent = root.forkedFromSessionID
	}
	identity.Key = sessionOperationKey(identity.Kind, identity.Platform, identity.UserID, root.sessionID)
	if parent != "" && parent != root.sessionID {
		identity.ParentKey = sessionOperationKey(identity.Kind, identity.Platform, identity.UserID, parent)
	}
	ctx.Set(sessionOperationsContextKey, identity)
}

func sessionOperationsIdentity(ctx *gin.Context) (database.SessionErrorIdentity, bool) {
	value, _ := ctx.Get(sessionOperationsContextKey)
	identity, ok := value.(database.SessionErrorIdentity)
	return identity, ok && database.ValidSessionOperationKey(identity.Key)
}

func (handler *Handler) sessionBlacklistError(ctx *gin.Context) *api.APIError {
	identity, known := sessionOperationsIdentity(ctx)
	if !known || handler.db == nil {
		return nil
	}
	lookup, cancel := context.WithTimeout(ctx.Request.Context(), time.Second)
	defer cancel()
	lockedBy, err := handler.db.SessionBlacklistStatus(lookup, identity.Key, identity.ParentKey)
	if err != nil {
		code, message := "session_blacklist_unavailable", "暂时无法确认会话黑名单状态，请稍后重试。"
		if errors.Is(err, database.ErrSessionLineageConflict) {
			code, message = "session_lineage_invalid", "会话派生关系冲突或过深，请联系管理员。"
		}
		return api.NewAPIError(api.ErrorCode(code), message, api.ErrorTypeInvalidRequest)
	}
	if lockedBy == "" {
		return nil
	}
	ctx.Header("X-Should-Retry", "false")
	result := api.NewAPIError("session_blacklisted", "当前会话或其父会话已被管理员加入黑名单，长期禁止继续请求及派生会话调用，请联系管理员解锁。", api.ErrorTypeInvalidRequest)
	result.Details = gin.H{"retry": "stop", "inherited": lockedBy != identity.Key}
	return result
}

func (handler *Handler) recordObservedError(ctx *gin.Context, status int, apiError *api.APIError) {
	handler.recordSessionError(ctx, status, apiError)
	handler.recordServiceError(ctx, status, apiError)
}

func (handler *Handler) recordSessionError(ctx *gin.Context, status int, apiError *api.APIError) {
	state := serviceErrorAuditForRequest(ctx)
	if status != http.StatusInternalServerError || state == nil || handler.db == nil || apiError == nil || string(apiError.Code) != overloadErrorCode {
		return
	}
	state.usageMu.Lock()
	statusMismatch := state.usageStatus != 0 && state.usageStatus != http.StatusInternalServerError
	state.usageMu.Unlock()
	if statusMismatch {
		return
	}
	identity, known := sessionOperationsIdentity(ctx)
	if !known || !state.sessionRecorded.CompareAndSwap(false, true) {
		return
	}
	trace := snapshotUpstreamTrace(ctx.Request.Context())
	event := database.SessionErrorEvent{Identity: identity, CreatedAt: time.Now().UTC(), RequestID: diagnosticRequestID(trace.RequestID), AccountID: trace.accountID,
		Code: diagnosticLabel(string(apiError.Code)), Message: serviceErrorSafeText(ctx, apiError.Message, 2048), Endpoint: serviceErrorSafeText(ctx, ctx.Request.URL.Path, 256), Model: diagnosticLabel(ctx.GetString("x-model")), Transport: "http"}
	if state.websocket {
		event.Transport = "websocket"
	} else if strings.Contains(ctx.Writer.Header().Get("Content-Type"), "text/event-stream") {
		event.Transport = "sse"
	}
	if value, exists := ctx.Get(usageRequestDiagnosticsContextKey); exists {
		if diagnostics, valid := value.(*usageRequestDiagnostics); valid && diagnostics != nil {
			event.NewAPIRequestID = diagnostics.NewAPIRequestID
			if diagnostics.Resolved != nil {
				event.RequestType = diagnostics.Resolved.ThreadSource
			}
		}
	}
	handler.db.EnqueueSessionError(event)
}

func rememberSessionErrorUsage(ctx *gin.Context, input *database.UsageLogInput) {
	if state := serviceErrorAuditForRequest(ctx); state != nil && input != nil {
		state.usageMu.Lock()
		defer state.usageMu.Unlock()
		state.usageStatus = input.StatusCode
		state.usageError = ""
		if input.StatusCode == http.StatusInternalServerError && !input.IsRetryAttempt && isSessionOverloadUsageMessage(input.ErrorMessage) {
			state.usageError = serviceErrorSafeText(ctx, input.ErrorMessage, 2048)
		}
	}
}

func isSessionOverloadUsageMessage(message string) bool {
	code, _, _ := strings.Cut(strings.TrimSpace(message), " · ")
	return code == overloadErrorCode
}

func (handler *Handler) finishSessionErrorAudit(ctx *gin.Context) {
	state := serviceErrorAuditForRequest(ctx)
	if state != nil {
		state.usageMu.Lock()
		status, message := state.usageStatus, state.usageError
		state.usageMu.Unlock()
		if status == http.StatusInternalServerError && isSessionOverloadUsageMessage(message) {
			handler.recordSessionError(ctx, status, api.NewAPIError(api.ErrorCode(overloadErrorCode), message, api.ErrorTypeUpstream))
		}
	}
}
