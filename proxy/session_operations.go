package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"errors"
	"log"
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
	handler.beginSessionActivity(ctx, identity)
}

func (handler *Handler) beginSessionActivity(ctx *gin.Context, identity database.SessionErrorIdentity) {
	state := serviceErrorAuditForRequest(ctx)
	if state == nil || handler.db == nil {
		return
	}
	state.usageMu.Lock()
	defer state.usageMu.Unlock()
	if state.activityStarted {
		return
	}
	state.activityStarted = true
	state.autoLockSettings = handler.db.GetSessionAutoLockSettings()
	auxiliary := false
	if value, exists := ctx.Get(usageRequestDiagnosticsContextKey); exists {
		if diagnostics, ok := value.(*usageRequestDiagnostics); ok && diagnostics != nil && diagnostics.Resolved != nil {
			resolved := diagnostics.Resolved
			auxiliary = resolved.ThreadSource != "" && resolved.ThreadSource != "user" || resolved.RequestKind != "" && resolved.RequestKind != "turn"
		}
	}
	state.activityLease = handler.db.BeginSessionActivity(identity.Key, auxiliary, time.Now())
}

func (handler *Handler) finishSessionActivity(ctx *gin.Context) {
	state := serviceErrorAuditForRequest(ctx)
	if state == nil {
		return
	}
	state.usageMu.Lock()
	lease, success := state.activityLease, state.usageSucceeded
	state.usageMu.Unlock()
	// A 200 SSE/WS handshake is not proof of a successful model response.
	// Use the terminal usage result, and never mark a canceled request successful.
	lease.Finish(success && ctx.Request.Context().Err() == nil && (state.websocket || ctx.Writer.Status() < 400), time.Now())
}

func sessionOperationsIdentity(ctx *gin.Context) (database.SessionErrorIdentity, bool) {
	value, _ := ctx.Get(sessionOperationsContextKey)
	identity, ok := value.(database.SessionErrorIdentity)
	return identity, ok && database.ValidSessionOperationKey(identity.Key)
}

func (handler *Handler) sessionBlacklistError(ctx *gin.Context) *api.APIError {
	if apiRelaySessionExempt(ctx) {
		return nil
	}
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
	result := api.NewAPIError("session_blacklisted", "会话因为持续过载临时暂停调用，请联系管理员恢复。", api.ErrorTypeInvalidRequest)
	result.Details = gin.H{"retry": "stop", "inherited": lockedBy != identity.Key}
	return result
}

func (handler *Handler) recordObservedError(ctx *gin.Context, status int, apiError *api.APIError) {
	if state := serviceErrorAuditForRequest(ctx); state != nil && status >= 400 {
		state.usageMu.Lock()
		state.usageSucceeded = false
		state.observedStatus = status
		state.observedError = apiError
		state.usageMu.Unlock()
	}
	handler.recordSessionError(ctx, status, apiError)
	handler.recordServiceError(ctx, status, apiError)
}

func (handler *Handler) recordSessionError(ctx *gin.Context, status int, apiError *api.APIError) {
	handler.recordSessionErrorResult(ctx, status, apiError, false)
}

func (handler *Handler) recordSessionErrorResult(ctx *gin.Context, status int, apiError *api.APIError, includeOther500 bool) {
	state := serviceErrorAuditForRequest(ctx)
	if status != http.StatusInternalServerError || state == nil || handler.db == nil || apiError == nil || !includeOther500 && string(apiError.Code) != overloadErrorCode {
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
	apiError, diagnostics := sessionErrorDetails(ctx, state, status, apiError)
	diagnostics.UsageLogMode = string(handler.db.GetUsageLogMode())
	trace := snapshotUpstreamTrace(ctx.Request.Context())
	event := database.SessionErrorEvent{Identity: identity, CreatedAt: time.Now().UTC(), RequestID: diagnosticRequestID(trace.RequestID), AccountID: trace.accountID,
		Code: serviceErrorSafeText(ctx, string(apiError.Code), 128), Message: serviceErrorSafeText(ctx, apiError.Message, 2048), Endpoint: serviceErrorSafeText(ctx, ctx.Request.URL.Path, 256), Model: diagnosticLabel(ctx.GetString("x-model")), Transport: "http",
		ErrorType: serviceErrorSafeText(ctx, string(apiError.Type), 128), Diagnostics: diagnostics}
	if diagnostics.UsageRequestID != "" {
		event.RequestID = diagnostics.UsageRequestID
	}
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
		if !input.IsRetryAttempt {
			state.finalUsageStatus = input.StatusCode
			state.finalUsageMessage = serviceErrorSafeText(ctx, input.ErrorMessage, 2048)
			state.finalUsageRequestID = diagnosticRequestID(input.RequestID)
			state.finalUsageErrorKind = serviceErrorSafeText(ctx, input.UpstreamErrorKind, 128)
		}
		state.usageSucceeded = !input.IsRetryAttempt && input.StatusCode >= 200 && input.StatusCode < 300 && strings.TrimSpace(input.ErrorMessage) == ""
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
	defer handler.finishSessionAutoLock(ctx)
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

func (handler *Handler) finishSessionAutoLock(ctx *gin.Context) {
	state := serviceErrorAuditForRequest(ctx)
	if state == nil || handler.db == nil || !state.autoLockFinished.CompareAndSwap(false, true) {
		return
	}
	if apiRelaySessionExempt(ctx) {
		return
	}
	// A fresh mixed-pool request may select an API relay before its route has
	// an affinity owner. Honor the actual selected account in that case too.
	if handler.store != nil {
		if account := handler.store.FindByID(snapshotUpstreamTrace(ctx.Request.Context()).accountID); account != nil && account.IsOpenAIResponsesAPI() {
			return
		}
	}
	state.usageMu.Lock()
	settings, status, failure := state.autoLockSettings, state.finalUsageStatus, state.observedError
	hasFinalUsage := status != 0
	if status == 0 {
		status = state.observedStatus
	}
	state.usageMu.Unlock()
	if !settings.Enabled {
		return
	}
	identity, known := sessionOperationsIdentity(ctx)
	if !known {
		return
	}
	// A caller may close immediately after receiving a terminal failure. Once
	// usage has recorded the final result, later cancellation must not rewrite
	// that result to 499 and silently reset a genuine 500 streak.
	if !hasFinalUsage && ctx.Request.Context().Err() != nil {
		status = 499
	}
	if status == 0 && !state.websocket {
		status = ctx.Writer.Status()
	}
	if status == 0 {
		return
	} // An upgraded connection alone is not a result.
	if status == 500 {
		if failure == nil {
			failure = api.NewAPIError("http_500", "请求返回 HTTP 500", api.ErrorTypeServer)
		}
		handler.recordSessionErrorResult(ctx, status, failure, true)
	}
	operation, cancel := context.WithTimeout(context.Background(), time.Second)
	defer cancel()
	if _, err := handler.db.ObserveSessionFinalStatus(operation, identity, status, state.started, settings); err != nil {
		log.Printf("session_auto_lock persistence_failed: %v", err)
	}
}
