package proxy

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/stretchr/testify/require"
)

func TestSessionAccountFailoverRequiresExactGroups(test *testing.T) {
	for _, reason := range []string{"disabled", "quota", "auto_pause", "capacity"} {
		for _, scenario := range []struct {
			name    string
			groups  []int64
			allowed bool
		}{
			{"same", []int64{11, 22}, true},
			{"reordered", []int64{22, 11}, true},
			{"subset", []int64{11}, false},
			{"superset", []int64{11, 22, 33}, false},
			{"overlap", []int64{11, 33}, false},
			{"disjoint", []int64{33, 44}, false},
			{"ungrouped", nil, false},
		} {
			test.Run(reason+"/"+scenario.name, func(test *testing.T) {
				handler, owner, target, key := failoverTestSetup(test, true)
				require.True(test, handler.store.ApplyAccountGroups(owner.ID(), []int64{11, 22}))
				require.True(test, handler.store.ApplyAccountGroups(target.ID(), scenario.groups))
				switch reason {
				case "disabled":
					atomic.StoreInt32(&owner.Disabled, 1)
				case "quota":
					owner.UsagePercent7d, owner.UsagePercent7dValid, owner.PlanType, owner.Reset7dAt = 100, true, "free", time.Now().Add(time.Hour)
				case "auto_pause":
					handler.store.SetGlobalAutoPauseThresholds(0, .8)
					owner.UsagePercent7d, owner.UsagePercent7dValid, owner.Reset7dAt = 85, true, time.Now().Add(time.Hour)
				case "capacity":
					owner.SessionCapacityEnabled, owner.SessionCapacityMax = true, 1
					require.True(test, handler.store.AdmitAccountSession(owner, "another-root", time.Now()))
				}
				before, found, err := handler.db.ReadSessionContinuity(context.Background(), hashRiskIdentity(key))
				require.NoError(test, err)
				require.True(test, found)
				request, body := failoverTestRequest(test, handler)
				require.Nil(test, handler.configureSessionModelAffinity(request, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
				selected, _, handled := handler.takeSessionAccountFailover(request.Request.Context(), key, 0, nil, nil, auth.DispatchPolicyStandard)
				require.True(test, handled)
				expected := owner
				if scenario.allowed {
					require.Same(test, target, selected)
					handler.store.Release(selected)
					expected = target
				} else {
					require.Nil(test, selected)
				}
				record, found, err := handler.db.ReadSessionContinuity(context.Background(), hashRiskIdentity(key))
				require.NoError(test, err)
				require.True(test, found)
				require.Equal(test, expected.ID(), record.AccountID)
				require.Equal(test, before.ThreadID, record.ThreadID)
				require.Equal(test, before.Number, record.Number)
				require.True(test, before.LastSeen.Equal(record.LastSeen))
				live, found := handler.store.LiveSessionAccountID(key, time.Now())
				require.True(test, found)
				require.Equal(test, expected.ID(), live)
				if !scenario.allowed {
					require.Zero(test, record.FailoverCount)
					require.Equal(test, "no_safe_candidate", continuityRequest(request).Diagnostic.AccountFailover.Result)
				}
			})
		}
	}
}

func TestSessionAccountFailoverRejectsGroupChangesDuringPreparation(test *testing.T) {
	for _, changeOwner := range []bool{false, true} {
		test.Run(map[bool]string{false: "candidate", true: "owner"}[changeOwner], func(test *testing.T) {
			handler, owner, target, key := failoverTestSetup(test, true)
			require.True(test, handler.store.ApplyAccountGroups(owner.ID(), []int64{11, 22}))
			require.True(test, handler.store.ApplyAccountGroups(target.ID(), []int64{11, 22}))
			atomic.StoreInt32(&owner.Disabled, 1)
			request, body := failoverTestRequest(test, handler)
			require.Nil(test, handler.configureSessionModelAffinity(request, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
			changed := false
			store := &backgroundMatchRaceStore{CodexIdentityStore: handler.db, switchOwner: func() {
				account := target
				if changeOwner {
					account = owner
				}
				require.True(test, handler.store.ApplyAccountGroups(account.ID(), []int64{11}))
				changed = true
			}}
			ctx := WithCodexIdentityStore(request.Request.Context(), store)
			selected, _, handled := handler.takeSessionAccountFailover(ctx, key, 0, nil, nil, auth.DispatchPolicyStandard)
			require.True(test, changed)
			require.True(test, handled)
			require.Nil(test, selected)
			require.Equal(test, "account_groups_changed", continuityRequest(request).Diagnostic.AccountFailover.Reason)
			record, found, err := handler.db.ReadSessionContinuity(context.Background(), hashRiskIdentity(key))
			require.NoError(test, err)
			require.True(test, found)
			require.Equal(test, owner.ID(), record.AccountID)
			require.Zero(test, record.FailoverCount)
			live, found := handler.store.LiveSessionAccountID(key, time.Now())
			require.True(test, found)
			require.Equal(test, owner.ID(), live)
		})
	}
}
