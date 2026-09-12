package proxy

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type sessionAccountFailoverContextKey struct{}

type sessionAccountFailoverDiagnostic struct {
	Result            string `json:"result"`
	Reason            string `json:"reason,omitempty"`
	PreviousAccountID int64  `json:"previous_account_id,omitempty"`
	AccountID         int64  `json:"account_id,omitempty"`
	Generation        uint64 `json:"generation,omitempty"`
}

type sessionAccountFailoverPlan struct {
	Request    *gin.Context
	Key        string
	Body       []byte
	Checked    bool
	Diagnostic *sessionAccountFailoverDiagnostic
}

func (handler *Handler) validateMigratedSessionContext(request *gin.Context, body []byte, record database.SessionContinuityRecord, rootKeys ...string) *api.APIError {
	blocked := sessionFailoverContextBlock(sessionFailoverRequestHeaders(request), body)
	if blocked == "missing_request_context" {
		return nil
	}
	if blocked == "upstream_continuation" && gjson.GetBytes(body, "previous_response_id").String() != "" && !gjson.GetBytes(body, "conversation").Exists() && !gjson.GetBytes(body, "conversation_id").Exists() {
		owner := responseCacheOwnerForRequest(request, requestAPIKeyID(request))
		lookup, cancel := context.WithTimeout(request.Request.Context(), time.Second)
		defer cancel()
		affinity, found := lookupResponseAccountAffinity(lookup, handler.cache, owner, gjson.GetBytes(body, "previous_response_id").String())
		segmentMatches := !record.OutboundWindowReset
		if record.OutboundWindowReset && len(rootKeys) > 0 {
			segmentMatches = affinity.OutboundSegment == codexIdentityDigest("codex-outbound-segment-v1", hashRiskIdentity(rootKeys[0]), strconv.FormatUint(record.FailoverCount, 10))
		}
		if found && affinity.AccountID == record.AccountID && segmentMatches {
			withoutPrevious, _ := sjson.DeleteBytes(body, "previous_response_id")
			blocked = sessionFailoverContextBlock(sessionFailoverRequestHeaders(request), withoutPrevious)
			if blocked == "missing_request_context" || blocked == "incomplete_tool_context" {
				blocked = ""
			}
		}
	}
	if blocked != "" {
		return api.NewAPIError("codex_session_failover_context_required", "会话已更换绑定账号；当前旧续链或加密上下文无法确认属于新账号，请恢复完整未加密上下文或新开对话。", api.ErrorTypeInvalidRequest)
	}
	return nil
}

func sessionFailoverRequestHeaders(request *gin.Context) http.Header {
	if isResponsesWebSocketUpgradeRequest(request.Request) {
		return nil
	}
	return request.Request.Header
}

func (handler *Handler) restoreMigratedSessionOwner(request *gin.Context, key string, body []byte) *api.APIError {
	handler.attachSessionOutboundEpoch(request, "", database.SessionContinuityRecord{})
	request.Request = request.Request.WithContext(context.WithValue(request.Request.Context(), backgroundAccountMatchContextKey{}, (*backgroundAccountMatch)(nil)))
	usageRequestDiagnosticState(request).BackgroundAccountMatch = nil
	if root, related := auth.RelatedSessionRootKey(key); related {
		return handler.prepareBackgroundAccountMatch(request, root, body)
	}
	if handler.db == nil || key == "" {
		return nil
	}
	entry, found, err := handler.readSessionContinuity(request.Request.Context(), hashRiskIdentity(key))
	if err != nil {
		return sessionContinuityError("ownership_unavailable")
	}
	if !found || entry.Record.FailoverCount == 0 {
		return nil
	}
	handler.attachSessionOutboundEpoch(request, hashRiskIdentity(key), entry.Record)
	if err := handler.validateMigratedSessionContext(request, body, entry.Record, key); err != nil {
		return err
	}
	selectionTraceForRequest(request).PinAccount(entry.Record.AccountID)
	recordUsageRootAccount(request, entry.Record.AccountID, true)
	return nil
}

func (handler *Handler) recoverSessionFailoverGrant(request *gin.Context, key string) error {
	if windowGrantForRequest(request) != nil || handler.db == nil {
		return nil
	}
	status, identity := handler.cachedNewAPIPolicyAuditState(request)
	if (status != "verified" && status != "signed_response") || !identity.MetaVerified || identity.Identity.UserID == "" {
		return nil
	}
	subject := cache.PromptSessionLimitSubject(identity.Platform, identity.Identity.UserID)
	state, err := handler.db.ReadUserWindowAdmissions(request.Request.Context(), subject)
	if err != nil {
		return err
	}
	var selected *database.UserWindowGrant
	for _, grant := range state.Windows {
		if grant != nil && grant.OwnerKey == key {
			if selected != nil || grant.Expanded || !grant.Confirmed || !grant.ExpiresAt.After(time.Now()) {
				return errWindowGrantRefresh
			}
			selected = grant
		}
	}
	if selected != nil {
		request.Set(windowGrantContextKey, &signedWindowGrant{Version: 1, Platform: identity.Platform, UserID: identity.Identity.UserID, APIKeyID: identity.APIKeyID, Fingerprint: identity.Meta.RootSessionFingerprint, Grant: *selected})
	}
	return nil
}

func sessionAccountFailoverReason(account *auth.Account, policy auth.DispatchPolicy) string {
	if account == nil {
		return ""
	}
	if policy == auth.DispatchPolicySpark {
		if account.SparkDispatchEligible() {
			return ""
		}
		if account.SparkDispatchUsageLimited() {
			return "account_spark_usage_exhausted"
		}
	}
	return account.SessionAccountFailoverReason()
}

func (handler *Handler) sessionFailoverReasonForRequest(request *gin.Context, account *auth.Account, key string, policy auth.DispatchPolicy) string {
	if reason := sessionAccountFailoverReason(account, policy); reason != "" {
		return reason
	}
	if account != nil && account.SessionCapacityLimits().Enabled && !handler.store.CanAdmitAccountSession(account, key, time.Now(), selectionTraceForRequest(request)) {
		return "account_session_capacity_full"
	}
	return ""
}

func sessionFailoverContextBlock(headers http.Header, body []byte) string {
	for _, path := range []string{"previous_response_id", "conversation", "conversation_id"} {
		if value := gjson.GetBytes(body, path); value.Exists() && value.Type != gjson.Null && value.String() != "" {
			return "upstream_continuation"
		}
	}
	metadata := gjson.GetBytes(body, "client_metadata")
	if headers.Get("X-Codex-Turn-State") != "" || metadata.Get("x-codex-turn-state").Exists() {
		return "connection_turn_state"
	}
	input := gjson.GetBytes(body, "input")
	if !input.Exists() || input.Type == gjson.Null || input.IsArray() && len(input.Array()) == 0 {
		return "missing_request_context"
	}
	blocked := false
	var inspect func(gjson.Result)
	inspect = func(value gjson.Result) {
		if blocked || !value.IsArray() && !value.IsObject() {
			return
		}
		value.ForEach(func(key, item gjson.Result) bool {
			if (key.String() == "encrypted_content" || key.String() == "file_id") && item.Type != gjson.Null && item.String() != "" || key.String() == "type" && item.String() == "item_reference" {
				blocked = true
				return false
			}
			inspect(item)
			return !blocked
		})
	}
	inspect(input)
	if blocked {
		return "opaque_upstream_context"
	}
	calls := make(map[string]bool)
	for _, item := range input.Array() {
		if strings.HasSuffix(item.Get("type").String(), "_call") {
			calls[item.Get("call_id").String()] = true
		}
	}
	for _, item := range input.Array() {
		if strings.HasSuffix(item.Get("type").String(), "_call_output") && !calls[item.Get("call_id").String()] {
			return "incomplete_tool_context"
		}
	}
	return ""
}

func (handler *Handler) prepareSessionAccountFailover(request *gin.Context, key string, body []byte, policy auth.DispatchPolicy) (bool, *api.APIError) {
	if !CurrentRuntimeSettings().CodexSessionFailoverEnabled {
		return false, nil
	}
	state := continuityRequest(request)
	if state == nil || state.Diagnostic == nil || state.Diagnostic.OwnerAccount <= 0 {
		return false, nil
	}
	owner := handler.store.FindByID(state.Diagnostic.OwnerAccount)
	reason := handler.sessionFailoverReasonForRequest(request, owner, key, policy)
	if reason == "" {
		return false, nil
	}
	diagnostic := &sessionAccountFailoverDiagnostic{Result: "blocked", Reason: reason, PreviousAccountID: owner.ID()}
	state.Diagnostic.AccountFailover = diagnostic
	block := sessionFailoverContextBlock(sessionFailoverRequestHeaders(request), body)
	if handler.db == nil || state.Record.AccountID != owner.ID() {
		block = "persistent_owner_required"
	} else if state.Diagnostic.WouldBlock {
		block = "invalid_session_continuity"
	} else if !state.Known || state.ThreadID == "" {
		block = "window_identity_required"
	}
	if block != "" {
		diagnostic.Reason = block
		return false, api.NewAPIError("codex_session_failover_context_required", "绑定账号不可用，但当前请求的旧续链、加密上下文或会话归属不能安全迁移。请恢复完整未加密上下文，或新开对话。", api.ErrorTypeInvalidRequest)
	}
	if err := handler.recoverSessionFailoverGrant(request, key); err != nil {
		diagnostic.Reason = "window_grant_unavailable"
		return false, requestWindowGrantAPIError(err)
	}
	diagnostic.Result = "pending"
	plan := &sessionAccountFailoverPlan{Request: request, Key: key, Body: body, Diagnostic: diagnostic}
	request.Request = request.Request.WithContext(context.WithValue(request.Request.Context(), sessionAccountFailoverContextKey{}, plan))
	return true, nil
}

func (handler *Handler) takeSessionAccountFailover(ctx context.Context, key string, apiKeyID int64, exclude map[int64]bool, filter auth.AccountFilter, policy auth.DispatchPolicy) (*auth.Account, string, bool) {
	plan, _ := ctx.Value(sessionAccountFailoverContextKey{}).(*sessionAccountFailoverPlan)
	if plan == nil || plan.Checked || plan.Key != key {
		return nil, "", false
	}
	plan.Checked = true
	if !CurrentRuntimeSettings().CodexSessionFailoverEnabled {
		plan.Diagnostic.Result = "disabled"
		return nil, "", false
	}
	request := plan.Request
	state := continuityRequest(request)
	old := handler.store.FindByID(plan.Diagnostic.PreviousAccountID)
	if state == nil || old == nil || handler.sessionFailoverReasonForRequest(request, old, key, policy) == "" {
		plan.Diagnostic.Result = "owner_recovered"
		return nil, "", false
	}
	if handler.sessionBlacklistError(request) != nil || ctx.Err() != nil {
		plan.Diagnostic.Result = "blocked"
		return nil, "", true
	}
	shard, _ := strconv.ParseUint(state.Key[:2], 16, 8)
	lock := &handler.continuityLocks[shard%uint64(len(handler.continuityLocks))]
	lock.Lock()
	defer lock.Unlock()
	entry, found, err := handler.readSessionContinuity(ctx, state.Key)
	if err != nil || !found || entry.Record.AccountID != old.ID() || entry.Record.FailoverCount != state.Record.FailoverCount {
		plan.Diagnostic.Result, plan.Diagnostic.Reason = "blocked", "ownership_changed"
		return nil, "", true
	}
	excluded := make(map[int64]bool, len(exclude)+1)
	for accountID, value := range exclude {
		excluded[accountID] = value
	}
	excluded[old.ID()] = true
	trace := &auth.SelectionTrace{}
	trace.SetExpandedWindow(selectionTraceForRequest(request).ExpandedWindow())
	ownerGroups := old.GroupIDSnapshot()
	eligible := func(account *auth.Account) bool {
		if !account.HasExactGroupIDs(ownerGroups) {
			trace.Reject("account_groups_mismatch")
			return false
		}
		grant := windowGrantForRequest(request)
		limits := account.SessionCapacityLimits()
		if grant != nil && (grant.Grant.Expanded && !limits.Enabled || grant.Grant.NoWindow && limits.Enabled) {
			return false
		}
		return !account.IsRelayStyle() && account.EffectiveAccountID() != "" && account.EffectiveAccountID() != old.EffectiveAccountID() && (filter == nil || filter(account)) && handler.store.CanAdmitAccountSession(account, key, time.Now(), trace)
	}
	for range 16 {
		candidate := handler.store.NextExcludingWithDispatch(apiKeyID, excluded, eligible, policy, trace)
		if candidate == nil {
			break
		}
		excluded[candidate.ID()] = true
		preview := entry.Record
		preview.AccountID, preview.FailoverCount = candidate.ID(), entry.Record.FailoverCount+1
		preview.OutboundWindowReset = true
		preview.OutboundWindowBases = map[string]uint64{state.ThreadID: state.Number}
		previewContext := context.WithValue(ctx, sessionOutboundEpochContextKey{}, &sessionOutboundEpoch{handler: handler, key: state.Key, record: preview, preview: true})
		fingerprint := NewCodexTransportFingerprint(candidate, sessionFailoverRequestHeaders(request), plan.Body, "")
		apiKey := strings.TrimSpace(strings.TrimPrefix(request.GetHeader("Authorization"), "Bearer "))
		if err := fingerprint.ClaimSessionIdentity(previewContext, candidate, apiKey); err != nil || fingerprint.accountIdentity == nil {
			handler.store.Release(candidate)
			continue
		}
		if !handler.store.AdmitAccountSession(candidate, key, time.Now(), trace) {
			handler.store.Release(candidate)
			continue
		}
		if !old.HasExactGroupIDs(ownerGroups) || !candidate.HasExactGroupIDs(ownerGroups) {
			handler.store.RemoveAccountSession(candidate.ID(), key)
			handler.store.Release(candidate)
			plan.Diagnostic.Result, plan.Diagnostic.Reason = "blocked", "account_groups_changed"
			return nil, "", true
		}
		input := database.SessionAccountFailover{RootKey: state.Key, AffinityKey: key, ExpectedAccountID: old.ID(), AccountID: candidate.ID(), ExpectedGeneration: entry.Record.FailoverCount, Reason: plan.Diagnostic.Reason, At: time.Now().UTC(), ResetOutboundWindow: true, WindowThreadID: state.ThreadID, WindowNumber: state.Number}
		grant := windowGrantForRequest(request)
		if grant != nil {
			input.WindowSubject = cache.PromptSessionLimitSubject(grant.Platform, grant.UserID)
			input.WindowRoot, input.WindowGrantID = grant.Grant.Root, grant.Grant.ID
		}
		committed, updatedGrant, commitErr := handler.db.SwitchSessionContinuityAccount(ctx, input)
		if commitErr != nil {
			handler.store.RemoveAccountSession(candidate.ID(), key)
			handler.store.Release(candidate)
			plan.Diagnostic.Result, plan.Diagnostic.Reason = "blocked", "ownership_commit_failed"
			return nil, "", true
		}
		handler.store.UnbindSessionAffinity(key, old.ID())
		handler.store.BindSessionAffinity(key, candidate, candidate.GetProxyURL())
		handler.cacheSessionContinuity(state.Key, sessionContinuityCacheEntry{Record: committed, CheckedAt: time.Now(), WrittenAt: time.Now()})
		state.Record = committed
		handler.attachSessionOutboundEpoch(request, state.Key, committed)
		state.Diagnostic.OwnerAccount, state.Diagnostic.OwnerSource = candidate.ID(), "account_failover"
		selectionTraceForRequest(request).PinAccount(candidate.ID())
		plan.Diagnostic.Result, plan.Diagnostic.AccountID, plan.Diagnostic.Generation = "switched", candidate.ID(), committed.FailoverCount
		recordUsageRootAccount(request, candidate.ID(), true)
		if updatedGrant != nil && grant != nil {
			grant.Grant = *updatedGrant
			handler.cacheWindowTariff(input.WindowSubject, *updatedGrant)
			handler.publishRequestWindowGrant(request, grant)
		}
		return candidate, candidate.GetProxyURL(), true
	}
	plan.Diagnostic.Result = "no_safe_candidate"
	return nil, "", true
}
