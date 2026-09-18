package proxy

import (
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/stretchr/testify/require"
)

func TestSessionAccountFailoverIgnoresTags(test *testing.T) {
	for _, scenario := range []struct {
		name string
		tags []string
	}{
		{"same", []string{"pool", "pro"}},
		{"set_order", []string{"pro", "pool", "pro"}},
		{"subset", []string{"pool"}},
		{"superset", []string{"pool", "pro", "extra"}},
		{"different", []string{"other", "pro"}},
		{"untagged", nil},
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
			require.Same(test, target, selected)
			handler.store.Release(selected)
			require.NotContains(test, selectionTraceForRequest(request).Snapshot().Reasons, "account_tags_mismatch")
			diagnostic := usageRequestDiagnosticState(request).AccountFailover
			require.Equal(test, "exact_groups", diagnostic.Selection.MatchMode)
			require.Empty(test, diagnostic.Selection.RequiredTags)
			record, _, err := handler.db.ReadSessionContinuity(test.Context(), hashRiskIdentity(key))
			require.NoError(test, err)
			require.Equal(test, target.ID(), record.AccountID)
			require.EqualValues(test, 1, record.FailoverCount)
		})
	}
}

func TestSessionAccountFailoverAllowsTagChangesBeforeCommit(test *testing.T) {
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
			changed := false
			store := &backgroundMatchRaceStore{CodexIdentityStore: handler.db, switchOwner: func() {
				account := target
				if changeOwner {
					account = owner
				}
				require.True(test, handler.store.ApplyAccountTags(account.ID(), []string{"changed"}))
				changed = true
			}}
			selected, _, handled := handler.takeSessionAccountFailover(WithCodexIdentityStore(request.Request.Context(), store), key, 0, nil, nil, auth.DispatchPolicyStandard)
			require.True(test, handled)
			require.True(test, changed)
			require.Same(test, target, selected)
			handler.store.Release(selected)
			require.Equal(test, "switched", usageRequestDiagnosticState(request).AccountFailover.Result)
			record, _, err := handler.db.ReadSessionContinuity(test.Context(), hashRiskIdentity(key))
			require.NoError(test, err)
			require.Equal(test, target.ID(), record.AccountID)
			require.EqualValues(test, 1, record.FailoverCount)
		})
	}
}
