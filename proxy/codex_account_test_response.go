package proxy

import (
	"context"

	"github.com/codex2api/auth"
)

type codexAccountTestRawResponseKey struct{}

// WithCodexAccountTestRawResponse lets the account connection-test recorder
// inspect the original upstream response. Its existing credential redaction
// still applies to diagnostics. This is an internal, account-bound capability;
// no request header, body field or runtime setting enables it.
func WithCodexAccountTestRawResponse(ctx context.Context, account *auth.Account) context.Context {
	if ctx == nil {
		ctx = context.Background()
	}
	if account == nil || account.ID() <= 0 {
		return ctx
	}
	return context.WithValue(ctx, codexAccountTestRawResponseKey{}, account)
}

func preserveCodexAccountTestResponse(ctx context.Context, account *auth.Account) bool {
	if ctx == nil || account == nil {
		return false
	}
	selected, _ := ctx.Value(codexAccountTestRawResponseKey{}).(*auth.Account)
	// An ordinary user request must retain its privacy handling even if an
	// internal caller accidentally carries the connection-test context into it.
	return selected == account && turnStateSessionFrom(ctx) == nil && responseIdentityFrom(ctx) == nil
}
