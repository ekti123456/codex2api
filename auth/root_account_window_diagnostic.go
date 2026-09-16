package auth

import (
	"github.com/codex2api/database"
	"time"
)

// Snapshot only the target account/key without refreshing TTLs, hydrating the
// cache, taking a slot, or querying the database. Called at wait boundaries.
func (store *Store) RootAccountWindowDiagnostic(key string, accountID int64, now time.Time) *database.RootAccountWindowDiagnostic {
	diagnostic := &database.RootAccountWindowDiagnostic{ObservedAt: now.UTC(), Scope: "local", SlotState: "not_checked", LiveBindingState: "not_checked"}
	if store == nil {
		return diagnostic
	}
	store.sessionMu.RLock()
	binding, exists := store.sessionBindings[key]
	store.sessionMu.RUnlock()
	diagnostic.LiveBindingState = "missing"
	if exists {
		diagnostic.LiveBindingAccountID = binding.accountID
		diagnostic.LiveBindingState = "active"
		if !binding.expiresAt.After(now) {
			diagnostic.LiveBindingState = "expired"
		}
	}
	account := store.FindByID(accountID)
	if account == nil {
		return diagnostic
	}
	diagnostic.AccountPresent = true
	limits := account.SessionCapacityLimits()
	diagnostic.CapacityEnabled, diagnostic.TotalLimit, diagnostic.IdleTTLSeconds = limits.Enabled, limits.Total, int64(limits.IdleTTL/time.Second)
	store.accountSessionMu.Lock()
	defer store.accountSessionMu.Unlock()
	sessions := store.accountSessions[accountID]
	diagnostic.TotalUsed = len(sessions)
	diagnostic.SlotState = "missing"
	if state := sessions[key]; state != nil {
		diagnostic.SlotState = "active"
		diagnostic.SlotReserved, diagnostic.UpgradePending = state.reserved, state.pendingUpgrade
		diagnostic.LastSeen, diagnostic.ExpiresAt = state.lastSeen, state.lastSeen.Add(limits.IdleTTL)
		if !diagnostic.ExpiresAt.After(now) {
			diagnostic.SlotState = "expired"
		}
	}
	return diagnostic
}
