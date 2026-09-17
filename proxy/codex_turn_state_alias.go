package proxy

import (
	"context"
	"net/http"
	"strconv"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type turnStateSessionKey struct{}
type turnStateSession struct {
	handler        *Handler
	scope, rootKey string
	mu             sync.Mutex
	incoming       map[string]database.CodexTurnStateRecord
	issued         map[string]database.CodexTurnStateRecord
	events         []database.TurnStateEvent
	omitted        int
}

func turnStateSessionFrom(ctx context.Context) *turnStateSession {
	if ctx == nil {
		return nil
	}
	s, _ := ctx.Value(turnStateSessionKey{}).(*turnStateSession)
	return s
}

func (s *turnStateSession) log(action, carrier, alias, real string, account int64, generation uint64, expires *time.Time) {
	if s == nil {
		return
	}
	e := database.TurnStateEvent{Action: action, Carrier: carrier, AccountID: account, Generation: generation, At: time.Now().UTC(), ExpiresAt: expires}
	e.Alias = turnStateDiagnosticValue(alias, &e.ValueTruncated)
	if real != "" {
		e.RealHash = codexIdentityDigest("turn-state-log-v1", real)
		if action != "cleared_unmanaged" && action != "cleared_unverified_outbound" {
			e.Real = turnStateDiagnosticValue(real, &e.ValueTruncated)
		}
	}
	if strings.HasPrefix(carrier, "request_") {
		value := alias
		if value == "" {
			value = real
		}
		e.Received = turnStateDiagnosticValue(value, &e.ValueTruncated)
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.events) >= 24 {
		s.omitted++
		return
	}
	s.events = append(s.events, e)
}

func turnStateDiagnosticValue(value string, truncated *bool) string {
	const limit = 2048
	if len(value) > limit {
		if truncated != nil {
			*truncated = true
		}
		return strings.ToValidUTF8(value[:limit], "") + "…[truncated]"
	}
	return strings.Clone(value)
}

func turnStateDiagnostic(ctx context.Context) *database.TurnStateDiagnostic {
	s := turnStateSessionFrom(ctx)
	if s == nil {
		return nil
	}
	s.mu.Lock()
	defer s.mu.Unlock()
	if len(s.events) == 0 {
		return nil
	}
	return &database.TurnStateDiagnostic{ScopeHash: s.scope, Events: append([]database.TurnStateEvent(nil), s.events...), Omitted: s.omitted}
}

// Bind once per logical request (once per native WS frame), after verified owner
// and original root resolution, before account selection or identity rewriting.
func (h *Handler) bindTurnStateSession(c *gin.Context, body []byte, identity requestSessionIdentity) {
	if h.db == nil {
		return
	}
	headers := c.Request.Header
	if isResponsesWebSocketUpgradeRequest(c.Request) {
		headers = nil
	}
	projected := CodexRequestMetadataHeaders(headers, body)
	metadata := projected.Get(codexTurnMetadataHeader)
	turn := gjson.Get(metadata, "turn_id").String()
	if turn == "" {
		turn = gjson.Get(metadata, "root_turn_id").String()
	}
	// Related/bypass affinity markers are process-private random strings. Never
	// persist those markers: use the original root key across restarts/instances.
	root := sessionAffinityKey(identity.affinityID, requestAPIKeyID(c))
	if root == "" {
		root = "unbound:" + NewUpstreamSessionUUID()
	}
	s := &turnStateSession{handler: h, rootKey: hashRiskIdentity(root), scope: codexIdentityDigest("turn-state-scope-v1", responseCacheOwnerForRequest(c, requestAPIKeyID(c)), root, projected.Get(codexThreadIDHeader), turn), incoming: make(map[string]database.CodexTurnStateRecord), issued: make(map[string]database.CodexTurnStateRecord)}
	c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), turnStateSessionKey{}, s))
	type lookupResult struct {
		record database.CodexTurnStateRecord
		action string
	}
	checked := make(map[string]lookupResult)
	for _, input := range []struct{ carrier, value string }{{"request_header", headers.Get(codexTurnStateHeader)}, {"request_metadata", gjson.GetBytes(body, "client_metadata.x-codex-turn-state").String()}} {
		value := strings.TrimSpace(input.value)
		if value == "" {
			continue
		}
		if !h.db.IsManagedCodexTurnStateAlias(value) {
			s.log("cleared_unmanaged", input.carrier, "", value, 0, 0, nil)
			continue
		}
		if previous, ok := checked[value]; ok {
			s.log(previous.action, input.carrier, value, previous.record.Real, previous.record.AccountID, previous.record.Generation, nil)
			continue
		}
		lookup, cancel := context.WithTimeout(c.Request.Context(), time.Second)
		record, found, err := h.db.ReadCodexTurnState(lookup, value)
		cancel()
		action := "cleared_unknown_or_expired"
		switch {
		case err != nil:
			action = "cleared_store_unavailable"
		case found && record.Scope != s.scope:
			action = "cleared_scope_mismatch"
		case found:
			// An alias is never an account selection instruction. Check the current
			// persistent owner first, including A -> B -> A generation changes.
			entry, known, readErr := h.readSessionContinuity(c.Request.Context(), s.rootKey)
			owner, gen := int64(0), uint64(0)
			if known {
				owner, gen = entry.Record.AccountID, entry.Record.FailoverCount
			} else if readErr == nil && h.store != nil {
				owner, _ = h.store.LiveSessionAccountID(root, time.Now())
			}
			var account *auth.Account
			if h.store != nil {
				account = h.store.FindByID(owner)
			}
			if readErr == nil && account != nil && record.RootKey == s.rootKey && record.AccountID == owner && record.Generation == gen && record.AccountHash == turnStateAccountHash(account) {
				s.incoming[value] = record
				action = "restored"
			} else {
				action = "cleared_owner_changed_or_missing"
			}
		}
		// Do not disclose another user's account/hash even to this request's log.
		if record.Scope != s.scope {
			record = database.CodexTurnStateRecord{}
		}
		checked[value] = lookupResult{record, action}
		s.log(action, input.carrier, value, record.Real, record.AccountID, record.Generation, nil)
	}
}

func turnStateAccountHash(account *auth.Account) string {
	return codexIdentityDigest("turn-state-account-v1", strconv.FormatInt(account.ID(), 10), account.EffectiveAccountID())
}

// Normalize before continuity checks and continuation pinning; never use an old
// alias as proof that a request should stay on its former account.
func normalizeTurnStateIngress(c *gin.Context, body []byte) []byte {
	s := turnStateSessionFrom(c.Request.Context())
	if s == nil {
		return body
	}
	headers := c.Request.Header.Clone()
	if isResponsesWebSocketUpgradeRequest(c.Request) {
		deleteTurnStateHeader(headers)
	}
	body, headers = rewriteRequestTurnState(body, headers, func(value, carrier string) string { return s.incoming[strings.TrimSpace(value)].Real })
	c.Request.Header = headers
	return body
}

func rewriteRequestTurnState(body []byte, headers http.Header, replace func(string, string) string) ([]byte, http.Header) {
	headers = headers.Clone()
	value := headers.Get(codexTurnStateHeader)
	deleteTurnStateHeader(headers)
	if value != "" {
		if value = replace(value, "request_header"); value != "" {
			if headers == nil {
				headers = make(http.Header)
			}
			headers.Set(codexTurnStateHeader, value)
		}
	}
	if value := gjson.GetBytes(body, "client_metadata.x-codex-turn-state"); value.Exists() {
		replacement := replace(value.String(), "request_metadata")
		if replacement == "" {
			body, _ = sjson.DeleteBytes(body, "client_metadata.x-codex-turn-state")
		} else {
			body, _ = sjson.SetBytes(body, "client_metadata.x-codex-turn-state", replacement)
		}
	}
	return body, headers
}

func deleteTurnStateHeader(headers http.Header) {
	for key := range headers {
		if strings.EqualFold(key, codexTurnStateHeader) {
			delete(headers, key)
		}
	}
}

// Run at each executor boundary, after new-session/restart cleanup. No alias can
// escape even through payload rules or a caller without a bound request context.
func PrepareCodexTurnStateOutbound(ctx context.Context, account *auth.Account, body []byte, headers http.Header) ([]byte, http.Header) {
	s := turnStateSessionFrom(ctx)
	return rewriteRequestTurnState(body, headers, func(value, carrier string) string {
		if s == nil {
			if strings.HasPrefix(value, "c2ts_v1_") || database.ValidCodexTurnStateAlias(value) {
				return ""
			}
			return value
		}
		generation := uint64(0)
		if epoch := outboundEpochFromContext(ctx); epoch != nil {
			generation = epoch.record.FailoverCount
		}
		for _, record := range s.incoming {
			if value != record.Real && value != record.Alias {
				continue
			}
			if account != nil && !account.IsRelayStyle() && record.AccountID == account.ID() && record.Generation == generation && record.AccountHash == turnStateAccountHash(account) {
				return record.Real
			}
			s.log("cleared_account_or_generation_changed", strings.Replace(carrier, "request_", "outbound_", 1), record.Alias, record.Real, record.AccountID, record.Generation, nil)
			return ""
		}
		s.log("cleared_unverified_outbound", strings.Replace(carrier, "request_", "outbound_", 1), "", value, 0, generation, nil)
		return ""
	})
}

func trustedMappedTurnState(ctx context.Context, record database.SessionContinuityRecord, value string) bool {
	s := turnStateSessionFrom(ctx)
	if s == nil || s.handler.store == nil {
		return false
	}
	account := s.handler.store.FindByID(record.AccountID)
	if account == nil {
		return false
	}
	for _, mapped := range s.incoming {
		if mapped.Real == value && mapped.AccountID == record.AccountID && mapped.Generation == record.FailoverCount && mapped.AccountHash == turnStateAccountHash(account) {
			return true
		}
	}
	return false
}

func (s *turnStateSession) issue(ctx context.Context, account *auth.Account, real, carrier string) (string, error) {
	if real == "" {
		return "", nil
	}
	if s == nil || s.handler == nil || s.handler.db == nil || account == nil {
		return "", nil
	}
	generation := uint64(0)
	if epoch := outboundEpochFromContext(ctx); epoch != nil {
		generation = epoch.record.FailoverCount
	}
	binding := database.CodexTurnStateBinding{Scope: s.scope, RootKey: s.rootKey, AccountID: account.ID(), AccountHash: turnStateAccountHash(account), Generation: generation}
	key := codexIdentityDigest(binding.Scope, binding.AccountHash, strconv.FormatUint(generation, 10), real)
	s.mu.Lock()
	record, found := s.issued[key]
	s.mu.Unlock()
	if !found {
		for _, incoming := range s.incoming {
			if incoming.CodexTurnStateBinding == binding && incoming.Real == real && time.Until(incoming.ExpiresAt) > 24*time.Hour {
				record, found = incoming, true
				break
			}
		}
	}
	if !found {
		saving, cancel := context.WithTimeout(ctx, time.Second)
		var err error
		record, err = s.handler.db.IssueCodexTurnState(saving, binding, real)
		cancel()
		if err != nil {
			s.log("issue_failed", carrier, "", real, account.ID(), generation, nil)
			return "", errTurnStateMapping
		}
		s.mu.Lock()
		if len(s.issued) < 32 {
			s.issued[key] = record
		}
		s.mu.Unlock()
		recordSessionTurnState(ctx, account, real)
	}
	s.log("issued", carrier, record.Alias, real, account.ID(), generation, &record.ExpiresAt)
	return record.Alias, nil
}
