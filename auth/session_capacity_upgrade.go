package auth

import (
	"errors"
	"time"

	"github.com/google/uuid"
)

var ErrReservedSessionFull = errors.New("当前窗口绑定账号的扩容会话容量已满，请新开对话或切换其他窗口使用")
var ErrSessionUpgradeUnnecessary = errors.New("当前窗口仍可使用普通会话容量，无需扩容加价")

func (store *Store) WithExpandedSessionReservation(accountID int64, key string, commit func() error) error {
	account := store.FindByID(accountID)
	if account == nil || !account.IsAvailable() {
		return errors.New("当前窗口绑定账号不可用，未执行扩容")
	}
	limits := account.SessionCapacityLimits()
	if !limits.Enabled || key == "" || limits.Reserved <= 0 {
		return ErrReservedSessionFull
	}
	now := time.Now()
	store.ensureAccountSessionsLoaded(account, now)
	store.accountSessionMu.Lock()
	reserved := store.purgeExpiredAccountSessionsLocked(accountID, limits.IdleTTL, now)
	sessions := store.accountSessions[accountID]
	if sessions[key] != nil || int64(len(sessions))-reserved < limits.Total-limits.Reserved {
		store.accountSessionMu.Unlock()
		return ErrSessionUpgradeUnnecessary
	}
	if int64(len(sessions)) >= limits.Total || reserved >= limits.Reserved {
		store.accountSessionMu.Unlock()
		return ErrReservedSessionFull
	}
	if sessions == nil {
		sessions = make(map[string]*accountSessionState)
		store.accountSessions[accountID] = sessions
	}
	reservation := &accountSessionState{sessionID: key, lastSeen: now, usagePeriodID: uuid.NewString(), usageStartedAt: now, reserved: true, pendingUpgrade: true}
	sessions[key] = reservation
	store.accountSessionMu.Unlock()
	err := commit()
	store.accountSessionMu.Lock()
	if store.accountSessions[accountID][key] == reservation {
		if err != nil {
			delete(store.accountSessions[accountID], key)
		} else {
			reservation.pendingUpgrade = false
			reservation.lastSeen = time.Now()
		}
	}
	store.accountSessionMu.Unlock()
	return err
}
