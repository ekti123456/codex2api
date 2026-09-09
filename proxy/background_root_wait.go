package proxy

import (
	"context"
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/gin-gonic/gin"
)

const backgroundRootAccountWaitTimeout = 30 * time.Second

func requiresBackgroundRootAccount(source string) bool {
	switch strings.ToLower(strings.TrimSpace(source)) {
	case "thread_title", "ambient_suggestions":
		return true
	default:
		return false
	}
}

func (handler *Handler) waitForBackgroundRootAccount(requestContext *gin.Context, identity requestSessionIdentity) *api.APIError {
	if !identity.requiresRootAccount {
		return nil
	}
	if !identity.relatedToRoot || !identity.stableIdentity || identity.unlinkedFallbackOnly || strings.TrimSpace(identity.affinityID) == "" {
		return api.NewAPIError(api.ErrCodeInvalidRequest, "Background request has no valid main conversation root.", api.ErrorTypeInvalidRequest)
	}
	state := usageRequestDiagnosticState(requestContext)
	deadline := state.StartedAt.Add(backgroundRootAccountWaitTimeout)
	status, policy := handler.cachedNewAPIPolicyAuditState(requestContext)
	if (status == "verified" || status == "signed_response") && policy.MetaVerified && policy.Meta.RootAccountWaitMillis != nil {
		remaining := min(max(*policy.Meta.RootAccountWaitMillis, 0), backgroundRootAccountWaitTimeout.Milliseconds())
		if sharedDeadline := state.StartedAt.Add(time.Duration(remaining) * time.Millisecond); sharedDeadline.Before(deadline) {
			deadline = sharedDeadline
		}
	}
	waitContext, cancelWait := context.WithDeadline(requestContext.Request.Context(), deadline)
	defer cancelWait()
	started := time.Now()
	accountID, waitErr := handler.store.WaitForRootAccount(waitContext, sessionAffinityKey(identity.affinityID, requestAPIKeyID(requestContext)))
	state.RootAccountWaitMillis = time.Since(started).Milliseconds()
	if waitErr == nil {
		state.RootAccountWait = "found"
		recordUsageRootAccount(requestContext, accountID, true)
		return nil
	}
	state.RootAccountWait = "timeout"
	if requestContext.Request.Context().Err() != nil {
		state.RootAccountWait = "canceled"
		return api.NewAPIError(api.ErrCodeInvalidRequest, "Background request was canceled while waiting for its main conversation.", api.ErrorTypeInvalidRequest)
	}
	return api.NewAPIError(api.ErrCodeRootAccountWaitTimeout, "Main conversation account was not bound within the 30-second wait limit. Background request stopped; no other account was selected.", api.ErrorTypeInvalidRequest)
}
