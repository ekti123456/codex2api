package proxy

import (
	"context"
	"fmt"
	"strconv"

	"github.com/codex2api/auth"
)

func codexAccountTestScope(account *auth.Account) string {
	return codexIdentityDigest("codex-account-test-v1", strconv.FormatInt(account.ID(), 10), account.EffectiveAccountID())
}

// ResolveCodexAccountTestSessionID persists the first test root for each account
// row and effective workspace, independently of tokens and device configuration.
func ResolveCodexAccountTestSessionID(ctx context.Context, store CodexIdentityStore, account *auth.Account) (string, error) {
	if account == nil || account.ID() <= 0 {
		return "", fmt.Errorf("测试账号缺少持久化 ID")
	}
	scope := codexAccountTestScope(account)
	if store == nil {
		// Database-free embedded users still get a stable, isolated root.
		return DeriveStableSessionUUIDv7(scope), nil
	}
	key := codexIdentityDigest("codex-account-test-root-key-v1", scope)
	entropy := codexIdentityDigest("codex-account-test-root-entropy-v1", scope)
	session, err := store.ResolveCodexIdentityUUIDv7(ctx, key, entropy)
	if err != nil {
		return "", fmt.Errorf("无法读取或保存账号测试主会话: %w", err)
	}
	return session, nil
}

// WithCodexAccountTestIdentityStore is reserved for administrator tests. Reusing
// a root requires a stable internal owner; anonymous user requests keep their
// existing per-request owners and cannot adopt this namespace.
func WithCodexAccountTestIdentityStore(ctx context.Context, store CodexIdentityStore, account *auth.Account) context.Context {
	if account != nil && account.ID() > 0 {
		owner := "account-test:" + codexAccountTestScope(account)
		ctx = context.WithValue(ctx, codexAnonymousIdentityContextKey{}, owner)
		ctx = context.WithValue(ctx, transportOwnerContextKey{}, owner)
	}
	if store == nil {
		return ctx
	}
	return WithCodexIdentityStore(ctx, store)
}
