package proxy

import (
	"context"
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"errors"
	"fmt"
	"net/http"
	"strings"
	"sync"
	"time"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

type codexIdentityClaimer interface {
	ClaimCodexIdentities(context.Context, []string, string) error
}

type codexIdentityClaimerContextKey struct{}

func codexIdentityRequestError(err error) *api.APIError {
	var requestError *Error
	if !errors.As(err, &requestError) {
		return nil
	}
	switch requestError.Code {
	case "codex_session_identity_invalid", "codex_session_identity_conflict", "codex_session_identity_unavailable":
		return api.NewAPIError(api.ErrorCode(requestError.Code), requestError.Message, api.ErrorTypeInvalidRequest)
	default:
		return nil
	}
}

type localCodexIdentityClaims struct {
	mu     sync.Mutex
	owners map[string]string
}

func (claims *localCodexIdentityClaims) ClaimCodexIdentities(ctx context.Context, keys []string, owner string) error {
	claims.mu.Lock()
	defer claims.mu.Unlock()
	if err := ctx.Err(); err != nil {
		return err
	}
	newKeys := make(map[string]bool)
	for _, key := range keys {
		if existing := claims.owners[key]; existing != "" && existing != owner {
			return database.ErrCodexIdentityConflict
		} else if existing == "" {
			newKeys[key] = true
		}
	}
	if len(claims.owners)+len(newKeys) > 65536 {
		return errors.New("codex identity registry capacity exhausted")
	}
	if claims.owners == nil {
		claims.owners = make(map[string]string)
	}
	for key := range newKeys {
		claims.owners[key] = owner
	}
	return nil
}

func (handler *Handler) bindCodexIdentityClaims(ctx *gin.Context) {
	var claimer codexIdentityClaimer = &handler.codexIdentityClaims
	if handler.db != nil {
		claimer = handler.db
	}
	ctx.Request = ctx.Request.WithContext(context.WithValue(ctx.Request.Context(), codexIdentityClaimerContextKey{}, claimer))
}

func codexIdentityDigest(parts ...string) string {
	encoded, _ := json.Marshal(parts)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:])
}

func (fingerprint *CodexFingerprint) ClaimSessionIdentity(ctx context.Context, account *auth.Account, apiKey string) (requestErr error) {
	defer func() {
		if requestErr != nil {
			UpstreamTransportObserver(ctx).Failure("gateway", "identity_validation", 0)
		}
	}()
	if !fingerprint.PreservesSessionIdentity() || ctx == nil || account == nil {
		return nil
	}
	claimer, _ := ctx.Value(codexIdentityClaimerContextKey{}).(codexIdentityClaimer)
	if claimer == nil {
		return nil
	}
	owner := verifiedTransportUser(ctx)
	if owner == "" {
		owner = "credential:" + strings.TrimSpace(apiKey)
		if strings.TrimSpace(apiKey) == "" {
			if connectionID := DownstreamWebsocketConnectionID(ctx); connectionID != "" {
				owner = "anonymous-connection:" + connectionID
			} else {
				owner = "anonymous:" + NewUpstreamSessionUUID()
			}
		}
	}
	owner = codexIdentityDigest("codex-owner-v1", owner)
	account.Mu().RLock()
	upstreamAccount := strings.TrimSpace(account.AccountID)
	account.Mu().RUnlock()
	accountScopes := []string{fmt.Sprintf("account:%d", account.ID())}
	if upstreamAccount != "" {
		accountScopes = append(accountScopes, "chatgpt:"+upstreamAccount)
	}
	keys := make([]string, 0, 16)
	seen := make(map[string]bool)
	for _, value := range fingerprint.identityValues {
		value = strings.TrimSpace(value)
		if value == "" || seen[value] {
			continue
		}
		seen[value] = true
		if len(value) > 512 {
			return &Error{Code: "codex_session_identity_invalid", Type: ErrorTypeInvalidRequest, HTTPStatus: http.StatusBadRequest, Message: "会话标识过长，请检查客户端请求。"}
		}
		for _, scope := range accountScopes {
			keys = append(keys, codexIdentityDigest("codex-session-v1", scope, value))
		}
	}
	if len(keys) == 0 {
		return nil
	}
	claimCtx, cancel := context.WithTimeout(ctx, 3*time.Second)
	defer cancel()
	if err := claimer.ClaimCodexIdentities(claimCtx, keys, owner); err != nil {
		if errors.Is(err, database.ErrCodexIdentityConflict) {
			return &Error{Code: "codex_session_identity_conflict", Type: ErrorTypeInvalidRequest, HTTPStatus: http.StatusBadRequest, Message: "当前会话标识已归属其他用户，请新建会话。"}
		}
		return &Error{Code: "codex_session_identity_unavailable", Type: ErrorTypeInvalidRequest, HTTPStatus: http.StatusBadRequest, Message: "暂时无法核实会话归属，请稍后重试。", Cause: err}
	}
	return nil
}
