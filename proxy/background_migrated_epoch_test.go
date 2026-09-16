package proxy

import (
	"context"
	"testing"
	"time"

	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
)

func TestBackgroundWaitInitializesMigratedEpochBeforeValidation(t *testing.T) {
	for _, source := range []string{"guardian_review", "thread_title", "subagent"} {
		for _, preserve := range []bool{false, true} {
			t.Run(source+map[bool]string{false: "/clean", true: "/preserve"}[preserve], func(t *testing.T) {
				h, old, target, key := failoverTestSetup(t, true)
				record, _, err := h.db.SwitchSessionContinuityAccount(context.Background(), database.SessionAccountFailover{
					RootKey: hashRiskIdentity(key), ExpectedAccountID: old.ID(), AccountID: target.ID(), Reason: "account_usage_exhausted",
					ResetOutboundWindow: true, WindowThreadID: continuityTestThread, LossyContextRestart: true, PreserveRestartInput: preserve,
				})
				require.NoError(t, err)
				h.store.UnbindSessionAffinity(key, old.ID())
				h.store.BindSessionAffinity(key, target, "")
				r, body := failoverTestRequest(t, h)
				r.Set(contextAPIKeyID, int64(101))
				r.Set(preservedInputSnapshotKey, body)
				s := usageRequestDiagnosticState(r)
				s.StartedAt = time.Now()
				s.Resolved.ThreadSource = source
				require.Nil(t, outboundEpochFromContext(r.Request.Context()))
				identity := requestSessionIdentity{stableIdentity: true, relatedToRoot: true, requiresRootAccount: true, affinityID: "failover-root"}
				require.Nil(t, h.waitForBackgroundRootAccount(r, identity))
				epoch := outboundEpochFromContext(r.Request.Context())
				require.NotNil(t, epoch)
				require.Equal(t, record.AccountID, epoch.record.AccountID)
				require.Equal(t, record.FailoverCount, epoch.record.FailoverCount)
				require.Equal(t, "ready", s.BackgroundWindowWait.Result)
				require.NoError(t, ValidateBackgroundAccountMatch(r.Request.Context(), target))
				require.Error(t, ValidateBackgroundAccountMatch(r.Request.Context(), old))
			})
		}
	}
}

func TestBackgroundWaitRejectsExistingConflictingEpoch(t *testing.T) {
	for _, mismatch := range []string{"account", "generation", "root", "window_reset"} {
		t.Run(mismatch, func(t *testing.T) {
			h, old, target, key := failoverTestSetup(t, true)
			record, _, err := h.db.SwitchSessionContinuityAccount(context.Background(), database.SessionAccountFailover{
				RootKey: hashRiskIdentity(key), ExpectedAccountID: old.ID(), AccountID: target.ID(), ResetOutboundWindow: true,
				WindowThreadID: continuityTestThread, LossyContextRestart: true,
			})
			require.NoError(t, err)
			h.store.UnbindSessionAffinity(key, old.ID())
			h.store.BindSessionAffinity(key, target, "")
			r, _ := failoverTestRequest(t, h)
			stale := record
			staleKey := hashRiskIdentity(key)
			switch mismatch {
			case "account":
				stale.AccountID = old.ID()
			case "generation":
				stale.FailoverCount = 0
			case "root":
				staleKey = hashRiskIdentity("other-root")
			case "window_reset":
				stale.OutboundWindowReset = !record.OutboundWindowReset
			}
			h.attachSessionOutboundEpoch(r, staleKey, stale)
			ctx, cancel := context.WithTimeout(r.Request.Context(), time.Second)
			defer cancel()
			failure := h.waitForBackgroundActiveWindow(ctx, r, key, target.ID(), &record)
			require.NotNil(t, failure)
			require.Equal(t, "会话账号归属或切号代次不一致，请重新发起请求。", failure.Message)
			require.Equal(t, "outbound_epoch_mismatch", usageRequestDiagnosticState(r).BackgroundWindowWait.Reason)
			require.Equal(t, staleKey, outboundEpochFromContext(r.Request.Context()).key)
			require.Equal(t, stale.AccountID, outboundEpochFromContext(r.Request.Context()).record.AccountID)
		})
	}
}
