package proxy

import (
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

const sessionModelUnavailableMessage = "当前会话绑定的上游账号不支持所选模型，请新开对话后使用该模型。"

func sessionModelUnavailableError() *api.APIError {
	return api.NewAPIError(api.ErrCodeSessionModelUnavailable, sessionModelUnavailableMessage, api.ErrorTypeInvalidRequest)
}

func sessionModelErrorForRequest(requestContext *gin.Context) *api.APIError {
	if selectionTraceForRequest(requestContext).SessionModelDenied() {
		return sessionModelUnavailableError()
	}
	return nil
}

func (handler *Handler) configureSessionModelAffinity(requestContext *gin.Context, identity requestSessionIdentity, key, originalModel, effectiveModel string, compact bool, bodies ...[]byte) (apiError *api.APIError) {
	if blocked := handler.sessionBlacklistError(requestContext); blocked != nil {
		return blocked
	}
	defer func() {
		if apiError != nil && handler.db != nil && len(bodies) > 0 && (usageRequestDiagnosticState(requestContext).Continuity != nil || usageRequestDiagnosticState(requestContext).BackgroundAccountMatch != nil) {
			handler.logUsageForRequest(requestContext, &database.UsageLogInput{Endpoint: requestContext.Request.URL.Path, Model: originalModel, EffectiveModel: effectiveModel, StatusCode: 400, ErrorMessage: apiError.Message, Stream: gjson.GetBytes(bodies[0], "stream").Bool(), Compact: compact})
		}
	}()
	if len(bodies) > 0 {
		if failoverError := handler.restoreMigratedSessionOwner(requestContext, key, bodies[0]); failoverError != nil {
			return failoverError
		}
		if continuityError := handler.prepareSessionContinuity(requestContext, identity, key, bodies[0]); continuityError != nil {
			return continuityError
		}
	}
	if !identity.stableIdentity || identity.unlinkedFallbackOnly || strings.TrimSpace(key) == "" || identity.bypassWindowAccounting {
		return nil
	}
	if handler.passiveInternalModelsAllowed(requestContext) && !identity.ownsRootBinding {
		return nil
	}
	trace := selectionTraceForRequest(requestContext)
	trace.SetSessionModelFilter(sessionModelSupportFilter(originalModel, effectiveModel, compact))
	if owner := trace.PinnedAccount(); owner > 0 {
		if len(bodies) > 0 {
			pending, failoverError := handler.prepareSessionAccountFailover(requestContext, key, bodies[0], dispatchPolicyForModel(effectiveModel))
			if failoverError != nil || pending {
				return failoverError
			}
		}
		if !trace.CheckSessionModel(handler.store.FindByID(owner)) {
			return sessionModelUnavailableError()
		}
		return handler.pinnedSessionCapacityError(requestContext, key)
	}
	if accountID, found := handler.store.LiveSessionAccountID(key, time.Now()); found {
		if !trace.CheckSessionModel(handler.store.FindByID(accountID)) {
			recordUsageRootAccount(requestContext, accountID, true)
			return sessionModelUnavailableError()
		}
	}
	return nil
}

func sessionModelSupportFilter(originalModel, effectiveModel string, compact bool) auth.AccountFilter {
	candidates := []string{originalModel, effectiveModel}
	var compactCandidates []compactMappingCandidate
	if compact {
		compactCandidates = compactMappingCandidates(originalModel, effectiveModel)
	}
	return func(account *auth.Account) bool {
		if account == nil {
			return false
		}
		if account.IsAntigravityAPI() {
			_, supported := antigravityResolvePublicModelForAccount(account, effectiveModel)
			return supported
		}
		if !account.IsRelayStyle() {
			return account.SupportsCodexModel(effectiveModel)
		}
		routedModel, mapped := resolveAccountModelMappingForCandidates(account, candidates...)
		if compact {
			routedModel, mapped = resolveAccountCompactModelMappingForCandidates(account, compactCandidates)
		}
		if !mapped || routedModel == "" {
			routedModel = effectiveModel
		}
		if account.IsClaudeOAuth() {
			return claudeAccountSupportsModel(account, routedModel)
		}
		if account.IsGrokAPI() {
			return account.GrokChannelSupportsModel(routedModel)
		}
		return account.SupportsOpenAIResponsesModel(routedModel)
	}
}
