package proxy

import (
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/stretchr/testify/require"
)

func TestSessionAccountFailoverRequiresExactTags(test *testing.T) {
	for _, scenario := range []struct {
		name    string
		tags    []string
		allowed bool
	}{
		{"same", []string{"pool", "pro"}, true},
		{"set_order", []string{"pro", "pool", "pro"}, true},
		{"subset", []string{"pool"}, false},
		{"superset", []string{"pool", "pro", "extra"}, false},
		{"different", []string{"other", "pro"}, false},
		{"untagged", nil, false},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			handler, owner, target, key := failoverTestSetup(test, true)
			owner.SessionCapacityEnabled, owner.SessionCapacityMax = true, 1
			require.True(test, handler.store.AdmitAccountSession(owner, "other", time.Now()))
			require.True(test, handler.store.ApplyAccountTags(owner.ID(), []string{"pool", "pro"}))
			require.True(test, handler.store.ApplyAccountTags(target.ID(), scenario.tags))
			request, body := failoverTestRequest(test, handler)
			require.Nil(test, handler.configureSessionModelAffinity(request, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
			selected, _, handled := handler.takeSessionAccountFailover(request.Request.Context(), key, 0, nil, nil, auth.DispatchPolicyStandard)
			require.True(test, handled)
			if scenario.allowed {
				require.Same(test, target, selected)
				handler.store.Release(selected)
			} else {
				require.Nil(test, selected)
				require.Contains(test, selectionTraceForRequest(request).Snapshot().Reasons, "account_tags_mismatch")
			}
		})
	}
}

func TestSessionAccountFailoverRejectsTagChangesBeforeCommit(test *testing.T) {
	for _, changeOwner := range []bool{false, true} {
		test.Run(map[bool]string{false: "target", true: "owner"}[changeOwner], func(test *testing.T) {
			handler, owner, target, key := failoverTestSetup(test, true)
			owner.SessionCapacityEnabled, owner.SessionCapacityMax = true, 1
			require.True(test, handler.store.AdmitAccountSession(owner, "other", time.Now()))
			for _, account := range []*auth.Account{owner, target} {
				require.True(test, handler.store.ApplyAccountTags(account.ID(), []string{"pool"}))
			}
			request, body := failoverTestRequest(test, handler)
			require.Nil(test, handler.configureSessionModelAffinity(request, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
			store := &backgroundMatchRaceStore{CodexIdentityStore: handler.db, switchOwner: func() {
				account := target
				if changeOwner {
					account = owner
				}
				require.True(test, handler.store.ApplyAccountTags(account.ID(), []string{"changed"}))
			}}
			selected, _, handled := handler.takeSessionAccountFailover(WithCodexIdentityStore(request.Request.Context(), store), key, 0, nil, nil, auth.DispatchPolicyStandard)
			require.True(test, handled)
			require.Nil(test, selected)
			require.Equal(test, "account_tags_changed", usageRequestDiagnosticState(request).AccountFailover.Reason)
			record, _, err := handler.db.ReadSessionContinuity(test.Context(), hashRiskIdentity(key))
			require.NoError(test, err)
			require.Equal(test, owner.ID(), record.AccountID)
			require.Zero(test, record.FailoverCount)
		})
	}
}
