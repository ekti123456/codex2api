package auth

import (
	"testing"
	"time"

	"github.com/stretchr/testify/require"
)

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
