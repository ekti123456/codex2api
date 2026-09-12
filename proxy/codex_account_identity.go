package proxy

import (
	"context"
	"crypto/hmac"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"net/http"
	"sort"
	"strconv"
	"strings"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/google/uuid"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type CodexIdentityStore interface {
	ClaimCodexIdentities(context.Context, []string, string) error
	ResolveCodexIdentityMapping(context.Context, string, []string, bool) (database.CodexIdentityMappingPolicy, error)
	ClaimCodexIdentityAliases(context.Context, []database.CodexIdentityAliasClaim) error
}

type codexAnonymousIdentityContextKey struct{}

func WithCodexIdentityStore(ctx context.Context, store CodexIdentityStore) context.Context {
	if ctx.Value(codexAnonymousIdentityContextKey{}) == nil {
		ctx = context.WithValue(ctx, codexAnonymousIdentityContextKey{}, "anonymous:"+NewUpstreamSessionUUID())
	}
	return context.WithValue(ctx, codexIdentityClaimerContextKey{}, store)
}

type codexAccountIdentityChange struct {
	Original string `json:"original"`
	Outbound string `json:"outbound"`
}

type codexAccountIdentityDiagnosticKey struct{}

type codexAccountIdentityDiagnostic struct {
	Version          string                       `json:"version,omitempty"`
	Status           string                       `json:"status"`
	ScopeHash        string                       `json:"scope_hash,omitempty"`
	UpstreamAccount  string                       `json:"chatgpt_account_id,omitempty"`
	CachePartitioned bool                         `json:"cache_partitioned,omitempty"`
	Changes          []codexAccountIdentityChange `json:"changes,omitempty"`
	PreservedIDs     []string                     `json:"preserved_ids,omitempty"`
	Generation       uint64                       `json:"generation,omitempty"`
	SegmentHash      string                       `json:"segment_hash,omitempty"`
	Windows          []codexAccountWindowChange   `json:"windows,omitempty"`
}

type codexAccountIdentity struct {
	secret      []byte
	owner       string
	account     string
	epoch       string
	windowBases map[string]uint64
	aliases     map[string]string
	diagnostic  codexAccountIdentityDiagnostic
}

var codexAccountIdentityFields = []string{
	"session_id", "thread_id", "parent_thread_id", "forked_from_thread_id", "context_window_id",
	"x-codex-parent-thread-id", "x_codex_parent_thread_id", "x-codex-forked-from-thread-id", "x_codex_forked_from_thread_id",
	"x-codex-context-window-id", "x_codex_context_window_id",
}

func codexAccountIdentityInputs(headers http.Header, body []byte) []string {
	values := codexTransportIdentityValues(headers, body)
	metadata := gjson.GetBytes(body, "client_metadata")
	for _, source := range []gjson.Result{metadata, diagnosticMetadataObject(metadata.Get("x-codex-turn-metadata")), gjson.Parse(headers.Get(codexTurnMetadataHeader))} {
		for _, field := range codexAccountIdentityFields {
			if value := source.Get(field); value.Type == gjson.String && strings.TrimSpace(value.String()) != "" {
				values = append(values, value.String())
			}
		}
		for _, field := range []string{"window_id", "x-codex-window-id", "x_codex_window_id"} {
			if value := source.Get(field); value.Type == gjson.String {
				if separator := strings.LastIndexByte(value.String(), ':'); separator > 0 {
					values = append(values, value.String()[:separator])
				}
			}
		}
	}
	return values
}

func (fingerprint *CodexFingerprint) prepareAccountIdentity(ctx context.Context, account *auth.Account, owner string, accountScopes []string) error {
	diagnostic := codexAccountIdentityDiagnostic{Status: "not_applicable"}
	defer func() {
		fingerprint.accountIdentityDiagnostic = &diagnostic
		UpstreamTransportObserver(ctx).updateOutboundIdentity(func(identity *outboundIdentityDiagnostic) {
			identity.AccountMapping = &diagnostic
		})
	}()
	root := strings.TrimSpace(fingerprint.headers.Get(codexSessionIDHeader))
	if root == "" {
		root = strings.TrimSpace(fingerprint.headers.Get(codexLegacySessionIDHeader))
	}
	if root == "" {
		if fingerprint.accountIdentityRequested && len(fingerprint.identityValues) > 0 {
			diagnostic.Status = "missing_root"
			return codexAccountIdentityError("缺少明确的根会话标识，无法生成账号级出站身份。")
		}
		return nil
	}
	store, available := ctx.Value(codexIdentityClaimerContextKey{}).(CodexIdentityStore)
	if !available {
		if fingerprint.accountIdentityRequested {
			diagnostic.Status = "store_unavailable"
			return codexAccountIdentityError("账号级出站身份需要持久化存储，当前无法使用。")
		}
		return nil
	}
	diagnostic.Status = "failed"
	upstreamAccount := strings.TrimSpace(account.EffectiveAccountID())
	if upstreamAccount == "" {
		if fingerprint.accountIdentityRequested {
			return codexAccountIdentityError("缺少实际 Chatgpt-Account-Id，无法生成账号级出站身份。")
		}
		diagnostic.Status = "not_applicable"
		return nil
	}
	rootKey := codexIdentityDigest("codex-account-root-v1", owner, upstreamAccount, strings.ToLower(root))
	epoch := outboundEpochFromContext(ctx)
	epochKey := epoch.identityKey()
	legacyKeys := make([]string, 0, len(accountScopes))
	for _, scope := range accountScopes {
		legacyKeys = append(legacyKeys, codexIdentityDigest("codex-session-v1", scope, root))
	}
	if epochKey != "" {
		rootKey = codexIdentityDigest("codex-account-segment-root-v1", owner, upstreamAccount, epochKey, strings.ToLower(root))
		legacyKeys = nil
		diagnostic.Generation, diagnostic.SegmentHash = epoch.record.FailoverCount, epochKey[:24]
	}
	policy, err := store.ResolveCodexIdentityMapping(ctx, rootKey, legacyKeys, fingerprint.accountIdentityRequested || epochKey != "")
	if err != nil {
		return codexAccountIdentityError("暂时无法核实出站身份映射，请稍后重试。")
	}
	diagnostic.UpstreamAccount = diagnosticIdentifier(upstreamAccount)
	diagnostic.ScopeHash = rootKey[:24]
	if policy.Mode == "preserve" {
		diagnostic.Status = "preserved_existing"
		return nil
	}
	if policy.Mode != "account-suffix-v1" {
		return codexAccountIdentityError("出站身份映射版本不受支持，请检查服务版本。")
	}
	secret, err := hex.DecodeString(policy.Secret)
	if err != nil || len(secret) != 32 {
		return codexAccountIdentityError("出站身份映射密钥不可用，请恢复完整数据库。")
	}
	mapping := &codexAccountIdentity{secret: secret, owner: owner, account: upstreamAccount, epoch: epochKey, aliases: make(map[string]string)}
	if err := fingerprint.prepareAccountWindows(ctx, mapping, epoch); err != nil {
		return err
	}
	diagnostic.Windows = mapping.diagnostic.Windows
	sort.Slice(diagnostic.Windows, func(left, right int) bool {
		return diagnostic.Windows[left].ThreadID < diagnostic.Windows[right].ThreadID
	})
	values := make(map[string]bool)
	for _, original := range fingerprint.accountIdentityInputs {
		if original = strings.TrimSpace(original); original == "" {
			continue
		}
		parsed, err := uuid.Parse(original)
		if err != nil || parsed.Version() != 7 || parsed.Variant() != uuid.RFC4122 {
			return codexAccountIdentityError("账号级出站映射仅支持 UUIDv7 会话及上下文标识，请检查客户端元数据。")
		}
		values[parsed.String()] = true
	}
	if len(values) == 0 || len(values) > 32 {
		return codexAccountIdentityError("出站会话身份数量无效，请检查客户端元数据。")
	}
	ordered := make([]string, 0, len(values))
	for original := range values {
		ordered = append(ordered, original)
	}
	sort.Strings(ordered)
	claims := make([]database.CodexIdentityAliasClaim, 0, len(ordered))
	for _, original := range ordered {
		identityKey := codexIdentityDigest("codex-account-root-v1", owner, upstreamAccount, original)
		identityLegacyKeys := make([]string, 0, len(accountScopes))
		for _, scope := range accountScopes {
			identityLegacyKeys = append(identityLegacyKeys, codexIdentityDigest("codex-session-v1", scope, original))
		}
		if epochKey != "" {
			identityKey = codexIdentityDigest("codex-account-segment-root-v1", owner, upstreamAccount, epochKey, original)
			identityLegacyKeys = nil
		}
		identityPolicy, err := store.ResolveCodexIdentityMapping(ctx, identityKey, identityLegacyKeys, true)
		if err != nil {
			return codexAccountIdentityError("暂时无法核实关联会话身份映射，请稍后重试。")
		}
		if identityPolicy.Mode == "preserve" {
			diagnostic.PreservedIDs = append(diagnostic.PreservedIDs, original)
			continue
		}
		if identityPolicy.Mode != policy.Mode || identityPolicy.Secret != policy.Secret {
			return codexAccountIdentityError("关联会话身份映射不一致，请检查数据库完整性。")
		}
		outbound := original[:len(original)-9] + mapping.digest("identity", original)[:9]
		if outbound == original {
			return codexAccountIdentityError("出站会话标识发生冲突，已停止请求，请联系管理员。")
		}
		mapping.aliases[original] = outbound
		diagnostic.Changes = append(diagnostic.Changes, codexAccountIdentityChange{Original: original, Outbound: outbound})
		sourceKey := codexIdentityDigest("codex-account-alias-source-v1", owner, upstreamAccount, original)
		if epochKey != "" {
			sourceKey = codexIdentityDigest("codex-account-segment-source-v1", owner, upstreamAccount, epochKey, original)
		}
		claims = append(claims, database.CodexIdentityAliasClaim{
			AliasKey:  codexIdentityDigest("codex-account-alias-v1", outbound),
			SourceKey: sourceKey,
		})
	}
	if err := store.ClaimCodexIdentityAliases(ctx, claims); err != nil {
		if errors.Is(err, database.ErrCodexIdentityAliasCollision) {
			return codexAccountIdentityError("出站会话标识发生冲突，已停止请求，请联系管理员。")
		}
		return codexAccountIdentityError("暂时无法登记出站身份映射，请稍后重试。")
	}
	diagnostic.Version = policy.Mode
	diagnostic.Status = "mapped"
	mapping.diagnostic = diagnostic
	fingerprint.accountIdentity = mapping
	fingerprint.headers = mapping.rewriteHeaders(fingerprint.headers)
	return nil
}

func codexAccountIdentityError(message string) *Error {
	return &Error{Code: "codex_session_identity_unavailable", Type: ErrorTypeInvalidRequest, HTTPStatus: http.StatusBadRequest, Message: message}
}

func (mapping *codexAccountIdentity) digest(domain, original string) string {
	parts := []string{"account-suffix-v1", domain, mapping.owner, mapping.account, original}
	if mapping.epoch != "" {
		parts = append(parts, mapping.epoch)
	}
	encoded, _ := json.Marshal(parts)
	mac := hmac.New(sha256.New, mapping.secret)
	_, _ = mac.Write(encoded)
	return hex.EncodeToString(mac.Sum(nil))
}

func (mapping *codexAccountIdentity) rewriteValue(original string) string {
	if parsed, err := uuid.Parse(strings.TrimSpace(original)); err == nil {
		if alias := mapping.aliases[parsed.String()]; alias != "" {
			return alias
		}
	}
	return original
}

func (mapping *codexAccountIdentity) rewriteWindow(original string) string {
	if separator := strings.LastIndexByte(original, ':'); separator >= 0 {
		thread := strings.ToLower(original[:separator])
		suffix := original[separator:]
		if base, found := mapping.windowBases[thread]; found {
			if number, err := strconv.ParseUint(suffix[1:], 10, 64); err == nil && number >= base {
				suffix = ":" + strconv.FormatUint(number-base, 10)
			}
		}
		return mapping.rewriteValue(original[:separator]) + suffix
	}
	return original
}

func (mapping *codexAccountIdentity) rewriteMetadata(raw string, fallbackThreads ...string) string {
	if !gjson.Valid(raw) || !gjson.Parse(raw).IsObject() {
		return raw
	}
	thread := gjson.Get(raw, "thread_id").String()
	if thread == "" {
		thread = gjson.Get(raw, "session_id").String()
	}
	for _, field := range []string{"window_id", "x-codex-window-id", "x_codex_window_id"} {
		if windowThread, _, err := parseAccountWindow(gjson.Get(raw, field).String()); err == nil {
			thread = windowThread
			break
		}
	}
	if thread == "" && len(fallbackThreads) > 0 {
		thread = fallbackThreads[0]
	}
	if base, found := mapping.windowBases[strings.ToLower(thread)]; found {
		if value := gjson.Get(raw, "window_number"); value.Type == gjson.Number {
			if number, err := strconv.ParseUint(value.Raw, 10, 64); err == nil && number >= base {
				raw, _ = sjson.Set(raw, "window_number", number-base)
			}
		}
	}
	for _, field := range append(append([]string(nil), codexAccountIdentityFields...), "x-client-request-id", "client_request_id", "x_client_request_id", "window_id", "x-codex-window-id", "x_codex_window_id") {
		value := gjson.Get(raw, field)
		if value.Type != gjson.String {
			continue
		}
		updated := mapping.rewriteValue(value.String())
		if field == "window_id" || field == "x-codex-window-id" || field == "x_codex_window_id" {
			updated = mapping.rewriteWindow(value.String())
		}
		if updated != value.String() {
			raw, _ = sjson.Set(raw, field, updated)
		}
	}
	return raw
}

func (mapping *codexAccountIdentity) rewriteHeaders(headers http.Header) http.Header {
	headers = headers.Clone()
	originalThread := headers.Get(codexThreadIDHeader)
	for _, name := range []string{codexSessionIDHeader, codexLegacySessionIDHeader, codexThreadIDHeader, codexClientRequestIDHeader, codexParentThreadIDHeader, "X-Codex-Forked-From-Thread-Id"} {
		if value := headers.Get(name); value != "" {
			headers.Set(name, mapping.rewriteValue(value))
		}
	}
	if window := headers.Get(codexWindowIDHeader); window != "" {
		headers.Set(codexWindowIDHeader, mapping.rewriteWindow(window))
	}
	if metadata := headers.Get(codexTurnMetadataHeader); metadata != "" {
		headers.Set(codexTurnMetadataHeader, mapping.rewriteMetadata(metadata, originalThread))
	}
	return headers
}

func (mapping *codexAccountIdentity) rewriteBody(body []byte) []byte {
	metadata := gjson.GetBytes(body, "client_metadata")
	if !metadata.IsObject() {
		return body
	}
	outerThread := accountMetadataThread(metadata)
	innerThread := accountMetadataThread(diagnosticMetadataObject(metadata.Get("x-codex-turn-metadata")))
	raw := mapping.rewriteMetadata(metadata.Raw, innerThread)
	embedded := gjson.Get(raw, "x-codex-turn-metadata")
	if embedded.Type == gjson.String {
		raw, _ = sjson.Set(raw, "x-codex-turn-metadata", mapping.rewriteMetadata(embedded.String(), outerThread))
	} else if embedded.IsObject() {
		raw, _ = sjson.SetRaw(raw, "x-codex-turn-metadata", mapping.rewriteMetadata(embedded.Raw, outerThread))
	}
	updated, err := sjson.SetRawBytes(body, "client_metadata", []byte(raw))
	if err != nil {
		return body
	}
	return updated
}

func (fingerprint *CodexFingerprint) ScopeCacheKey(ctx context.Context, cacheKey string) string {
	if fingerprint.accountIdentity == nil || strings.TrimSpace(cacheKey) == "" {
		return cacheKey
	}
	diagnostic := fingerprint.accountIdentity.diagnostic
	diagnostic.CachePartitioned = true
	fingerprint.accountIdentityDiagnostic = &diagnostic
	UpstreamTransportObserver(ctx).updateOutboundIdentity(func(identity *outboundIdentityDiagnostic) {
		identity.AccountMapping = &diagnostic
	})
	return fingerprint.accountIdentity.digest("prompt-cache", cacheKey)
}

func (fingerprint *CodexFingerprint) withAccountIdentityDiagnostic(ctx context.Context) context.Context {
	if fingerprint.accountIdentityDiagnostic == nil {
		return ctx
	}
	return context.WithValue(ctx, codexAccountIdentityDiagnosticKey{}, fingerprint.accountIdentityDiagnostic)
}
