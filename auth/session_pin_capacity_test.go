package auth

import (
	"context"
	"sync/atomic"
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

func TestPinnedSessionWaitDoesNotWaitForAnotherAccount(test *testing.T) {
	for _, reason := range []string{"disabled", "paused", "quota", "excluded", "missing"} {
		test.Run(reason, func(test *testing.T) {
			store, owner, _ := newHardWindowFallbackTestStore()
			bindHardWindowFallbackTestRoot(test, store, owner, "permanent-root")
			excluded := map[int64]bool{}
			switch reason {
			case "disabled":
				atomic.StoreInt32(&owner.Disabled, 1)
			case "paused":
				atomic.StoreInt32(&owner.DispatchPaused, 1)
			case "quota":
				owner.PlanType, owner.UsagePercent7d, owner.UsagePercent7dValid, owner.Reset7dAt = "free", 100, true, time.Now().Add(time.Hour)
			case "excluded":
				excluded[owner.ID()] = true
			case "missing":
				store.RemoveAccount(owner.ID())
			}
			trace := &SelectionTrace{}
			trace.PinAccount(owner.ID())
			requestContext, cancel := context.WithTimeout(context.Background(), time.Second)
			defer cancel()
			selected, _, _ := store.WaitForSessionAvailableWithDispatchGuard(requestContext, "permanent-root", time.Minute, 0, excluded, nil, DispatchPolicyStandard, trace)
			require.Nil(test, selected)
			require.NoError(test, requestContext.Err())
			require.Equal(test, owner.ID(), trace.PinnedAccount())
		})
	}
}

func TestPinnedSessionWaitCanRestoreOwnerAfterRuntimeBindingWasLost(test *testing.T) {
	for _, continuation := range []bool{false, true} {
		store, owner, _ := newHardWindowFallbackTestStore()
		owner.SessionCapacityEnabled = false
		store.maxConcurrency = 1
		held := store.TakePreferredAccountWithDispatch(owner.ID(), 0, nil, nil, DispatchPolicyStandard)
		require.Same(test, owner, held)
		trace := &SelectionTrace{}
		trace.PinAccount(owner.ID())
		checked := make(chan struct{}, 1)
		filter := func(account *Account) bool {
			select {
			case checked <- struct{}{}:
			default:
			}
			return true
		}
		requestContext, cancel := context.WithTimeout(context.Background(), 2*time.Second)
		defer cancel()
		result := make(chan *Account, 1)
		go func() {
			selected, _, _ := store.waitForSessionAvailableWithFilter(requestContext, "expired-root", time.Minute, 0, nil, filter, continuation, DispatchPolicyStandard, trace)
			result <- selected
		}()
		select {
		case <-checked:
		case <-requestContext.Done():
			test.Fatal("the original account was not checked")
		}
		store.Release(held)
		select {
		case selected := <-result:
			require.Same(test, owner, selected)
			store.Release(selected)
		case <-requestContext.Done():
			test.Fatal("the original account did not resume after release")
		}
	}
}

func TestPinnedSessionDoesNotGrantQuotaContinuationToFreshRequest(test *testing.T) {
	store := NewStore(nil, nil, nil)
	test.Cleanup(store.Stop)
	owner := &Account{DBID: 1, AccessToken: "owner-test", Status: StatusReady, PlanType: "plus", UsagePercent5h: 100, UsagePercent5hValid: true, Reset5hAt: time.Now().Add(time.Hour)}
	store.AddAccount(owner)
	trace := &SelectionTrace{}
	trace.PinAccount(owner.ID())
	selected, _, _ := store.NextForSessionWithDispatchGuard("root", 0, nil, nil, DispatchPolicyStandard, trace)
	require.Nil(test, selected)
}

func TestPinnedSessionNeverReassignsUnavailableOwner(test *testing.T) {
	for _, reason := range []string{"quota", "disabled", "paused", "cooldown", "error", "banned", "missing_credentials", "request_excluded"} {
		test.Run(reason, func(test *testing.T) {
			store, owner, other := newHardWindowFallbackTestStore()
			bindHardWindowFallbackTestRoot(test, store, owner, "permanent-root")
			excluded := map[int64]bool{}
			switch reason {
			case "quota":
				owner.PlanType, owner.UsagePercent7d, owner.UsagePercent7dValid, owner.Reset7dAt = "free", 100, true, time.Now().Add(time.Hour)
			case "disabled":
				atomic.StoreInt32(&owner.Disabled, 1)
			case "paused":
				atomic.StoreInt32(&owner.DispatchPaused, 1)
			case "cooldown":
				owner.Status, owner.CooldownUtil = StatusCooldown, time.Now().Add(time.Hour)
			case "error":
				owner.Status = StatusError
			case "banned":
				owner.HealthTier = HealthTierBanned
			case "missing_credentials":
				owner.AccessToken = ""
			case "request_excluded":
				excluded[owner.ID()] = true
			}
			trace := &SelectionTrace{}
			trace.PinAccount(owner.ID())
			selected, _, _ := store.NextForSessionWithDispatchGuard("permanent-root", 0, excluded, nil, DispatchPolicyStandard, trace)
			if selected != nil {
				require.Same(test, owner, selected)
				store.Release(selected)
			}
			require.Nil(test, store.TakePreferredAccountWithDispatch(other.ID(), 0, nil, nil, DispatchPolicyStandard, trace))
			boundID, found := store.SessionAffinityAccountID("permanent-root")
			require.True(test, found)
			require.Equal(test, owner.ID(), boundID)
		})
	}
}

func TestPinnedSessionNeverBorrowsAccountWhenSlotOrConcurrencyFull(test *testing.T) {
	store, owner, other := newHardWindowFallbackTestStore()
	owner.SessionCapacityMax = 2
	owner.SessionCapacityReserved = 1
	require.True(test, store.AdmitAccountSession(owner, "other-window", time.Now()))
	trace := &SelectionTrace{}
	trace.PinAccount(owner.ID())
	selected, _, _ := store.NextForSessionWithDispatchGuard("expired-root", 0, nil, nil, DispatchPolicyStandard, trace)
	require.Nil(test, selected)
	trace.SetExpandedWindow(true)
	selected, _, _ = store.NextForSessionWithDispatchGuard("expired-root", 0, nil, nil, DispatchPolicyStandard, trace)
	require.Same(test, owner, selected)
	store.Release(selected)
	require.Nil(test, store.TakePreferredAccountWithDispatch(other.ID(), 0, nil, nil, DispatchPolicyStandard, trace))
	store.maxConcurrency = 1
	held := store.TakePreferredAccountWithDispatch(owner.ID(), 0, nil, nil, DispatchPolicyStandard)
	require.Same(test, owner, held)
	defer store.Release(held)
	selected, _, _ = store.NextForSessionWithDispatchGuard("expired-root", 0, nil, nil, DispatchPolicyStandard, trace)
	require.Nil(test, selected)
}

func TestPinnedSessionRetainsValidProxyButNotRemovedProxy(test *testing.T) {
	store, owner, _ := newHardWindowFallbackTestStore()
	owner.SessionCapacityEnabled = false
	proxyURL := "http://127.0.0.1:18081"
	store.SetProxyURL(proxyURL)
	store.BindSessionAffinity("root", owner, proxyURL)
	trace := &SelectionTrace{}
	trace.PinAccount(owner.ID())
	selected, stickyProxy, _ := store.NextForSessionWithDispatchGuard("root", 0, nil, nil, DispatchPolicyStandard, trace)
	require.Same(test, owner, selected)
	require.Equal(test, proxyURL, stickyProxy)
	store.Release(selected)
	store.SetProxyURL("http://127.0.0.1:18082")
	selected, stickyProxy, _ = store.NextForSessionWithDispatchGuard("root", 0, nil, nil, DispatchPolicyStandard, trace)
	require.Same(test, owner, selected)
	require.NotEqual(test, proxyURL, stickyProxy)
	store.Release(selected)
}
