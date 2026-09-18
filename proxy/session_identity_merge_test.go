package proxy

import (
	"net/http"
	"testing"

	"github.com/codex2api/api"
	"github.com/codex2api/auth"
	"github.com/stretchr/testify/require"
)

func TestClaudeSessionHintPreservesNewAPIRootRouting(test *testing.T) {
	for _, state := range []string{newAPIPolicyRootSessionResolved, newAPIPolicyRootSessionUnavailable, newAPIPolicyRootSessionConflict, "legacy"} {
		test.Run(state, func(test *testing.T) {
			handler := newRootlessPassiveModelTestHandler(test)
			body := []byte(`{"model":"claude-sonnet-4-5","messages":[{"role":"user","content":"hello"}]}`)
			fingerprint := promptSessionTestFingerprint("merge-root")
			metadata := newAPIPolicyMeta{
				RootSessionVersion: 1, RootSessionState: state,
				ThreadSource: "user", RequestKind: "turn",
			}
			if state == newAPIPolicyRootSessionResolved {
				metadata.RootSessionRelation = newAPIPolicyRootSessionRelationRoot
				metadata.RootSessionFingerprint = fingerprint
			} else if state == "legacy" {
				metadata.RootSessionVersion, metadata.RootSessionState = 0, ""
			} else {
				metadata.ThreadSource = "future_internal_kind"
			}
			ctx, _ := signedRootlessPassiveModelContext(test, http.MethodPost, "/v1/messages", body, metadata)
			ctx.Request.Header.Set("X-Claude-Code-Session-Id", "11111111-1111-4111-8111-111111111111")
			handler.primeNewAPIPolicyContext(ctx, body)
			if status, verified := handler.cachedNewAPIPolicyAuditState(ctx); status != "verified" || !verified.MetaVerified {
				test.Fatal("test requires verified NewAPI metadata")
			}
			base := resolveClaudeRequestSessionIdentity(ctx.Request.Header, body)
			if base.affinityID == "" {
				test.Fatal("test requires a usable Claude session hint")
			}
			unresolved := state == newAPIPolicyRootSessionUnavailable || state == newAPIPolicyRootSessionConflict
			if unresolved {
				base.relatedToRoot, base.ownsRootBinding, base.protectedRelatedLease = true, true, true
				base.relatedSource = auth.AccountSessionRelatedSource{ThreadSource: "user"}
				base.relatedRequestID, base.forkSourceAffinityID = "stale-request", "stale-fork"
			}
			identity := handler.resolveRequestSessionIdentityWithBase(ctx, body, base)
			if state == newAPIPolicyRootSessionResolved {
				if identity.affinityID != "newapi-root-session:"+fingerprint {
					test.Fatalf("Claude hint replaced the verified NewAPI root: %+v", identity)
				}
			} else if state == "legacy" {
				// Legacy signed leaf metadata still supplies its existing root fallback.
				require.Equal(test, "newapi-root-session:"+promptSessionTestFingerprint(test.Name()), identity.affinityID)
				require.True(test, identity.stableIdentity)
			} else if identity.affinityID != "" || identity.unlinkedFallbackOnly || !identity.requiresRootAccount {
				test.Fatalf("Claude hint bypassed the main-root requirement: %+v", identity)
			}
			require.Equal(test, base.upstreamSeed, identity.upstreamSeed)
			require.Equal(test, base.explicitUpstreamID, identity.explicitUpstreamID)
			if unresolved {
				require.False(test, identity.stableIdentity)
				require.False(test, identity.hasDownstreamAffinity)
				require.False(test, identity.hasRequestFingerprint)
				require.False(test, identity.relatedToRoot)
				require.False(test, identity.ownsRootBinding)
				require.False(test, identity.protectedRelatedLease)
				require.Empty(test, identity.relatedSource)
				require.Empty(test, identity.relatedRequestID)
				require.Empty(test, identity.forkSourceAffinityID)
				blocked := handler.waitForBackgroundRootAccount(ctx, identity)
				require.NotNil(test, blocked)
				require.Equal(test, api.ErrCodeBackgroundRootUnavailable, blocked.Code)
			}
		})
	}
}
