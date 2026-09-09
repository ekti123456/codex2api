package auth

import (
	"context"
	"encoding/json"
	"strings"
	"time"
)

type rootAccountWaitState struct {
	changed chan struct{}
	waiters int
}

func (store *Store) notifyRootAccountWaiters(key string) {
	store.rootAccountWaitMu.Lock()
	defer store.rootAccountWaitMu.Unlock()
	if state := store.rootAccountWaiters[key]; state != nil {
		close(state.changed)
		state.changed = make(chan struct{})
	}
}

func (store *Store) WaitForRootAccount(requestContext context.Context, key string) (int64, error) {
	key = strings.TrimSpace(key)
	store.rootAccountWaitMu.Lock()
	if store.rootAccountWaiters == nil {
		store.rootAccountWaiters = make(map[string]*rootAccountWaitState)
	}
	state := store.rootAccountWaiters[key]
	if state == nil {
		state = &rootAccountWaitState{changed: make(chan struct{})}
		store.rootAccountWaiters[key] = state
	}
	state.waiters++
	store.rootAccountWaitMu.Unlock()
	defer func() {
		store.rootAccountWaitMu.Lock()
		defer store.rootAccountWaitMu.Unlock()
		state.waiters--
		if state.waiters == 0 {
			delete(store.rootAccountWaiters, key)
		}
	}()

	recheck := time.NewTicker(time.Second)
	defer recheck.Stop()
	checkAccountWindows := true
	for {
		if err := requestContext.Err(); err != nil {
			return 0, err
		}
		store.rootAccountWaitMu.Lock()
		changed := state.changed
		store.rootAccountWaitMu.Unlock()
		if checkAccountWindows {
			if accountID, found := store.localRootAccountWindowID(key); found {
				return accountID, requestContext.Err()
			}
			checkAccountWindows = false
		}
		store.sessionMu.RLock()
		binding, found := store.sessionBindings[key]
		store.sessionMu.RUnlock()
		if found && binding.expiresAt.After(time.Now()) {
			return binding.accountID, requestContext.Err()
		}
		if store.tokenCache != nil && !isProcessLocalSessionAffinityKey(key) {
			lookupContext, cancelLookup := context.WithTimeout(requestContext, accountSessionCacheTimeout)
			raw, ownerFound, ownerErr := store.tokenCache.GetRuntime(lookupContext, accountSessionOwnerRuntimeNamespace, key)
			var owner persistedAccountSessionOwner
			if ownerErr == nil && ownerFound && json.Unmarshal(raw, &owner) == nil && owner.AccountID > 0 {
				cancelLookup()
				return owner.AccountID, requestContext.Err()
			}
			cached, cachedFound, cachedErr := store.tokenCache.GetSessionAffinity(lookupContext, key)
			cancelLookup()
			if cachedErr == nil && cachedFound && cached.AccountID > 0 {
				return cached.AccountID, requestContext.Err()
			}
		}
		select {
		case <-requestContext.Done():
			return 0, requestContext.Err()
		case <-changed:
			checkAccountWindows = true
		case <-recheck.C:
		}
	}
}

func (store *Store) localRootAccountWindowID(key string) (int64, bool) {
	store.accountSessionMu.Lock()
	var ownerID int64
	var lastSeen time.Time
	for accountID, sessions := range store.accountSessions {
		if state := sessions[key]; state != nil {
			ownerID, lastSeen = accountID, state.lastSeen
			break
		}
	}
	store.accountSessionMu.Unlock()
	if ownerID == 0 {
		return 0, false
	}
	account := store.FindByID(ownerID)
	if account == nil {
		return 0, false
	}
	enabled, _, idleTTL := account.SessionCapacityConfig()
	return ownerID, enabled && lastSeen.Add(idleTTL).After(time.Now())
}
