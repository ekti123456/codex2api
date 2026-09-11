package wsrelay

import (
	"time"

	"github.com/codex2api/proxy"
)

const continuationLossTTL = 30 * time.Minute

func (manager *Manager) cleanupContinuationRecords() {
	manager.respConnMu.Lock()
	defer manager.respConnMu.Unlock()
	now := time.Now()
	for id, binding := range manager.respConnBindings {
		if now.After(binding.expiresAt) {
			manager.rememberContinuationLossLocked(id, binding, "binding_expired")
			delete(manager.respConnBindings, id)
		}
	}
	for id, loss := range manager.continuationLosses {
		if now.After(loss.expiresAt) {
			delete(manager.continuationLosses, id)
		}
	}
}

type continuationLoss struct {
	accountID    int64
	apiKey       string
	requestScope string
	persisted    bool
	reason       string
	connection   proxy.WebsocketConnectionLifecycle
	expiresAt    time.Time
}

func (manager *Manager) rememberContinuationLossLocked(responseID string, binding responseConnBinding, reason string) {
	if manager.continuationLosses == nil {
		manager.continuationLosses = make(map[string]continuationLoss)
	}
	now := time.Now()
	if len(manager.continuationLosses) >= responseConnBindingMaxEntries {
		oldestID := ""
		var oldest time.Time
		for id, loss := range manager.continuationLosses {
			if now.After(loss.expiresAt) {
				delete(manager.continuationLosses, id)
				continue
			}
			if oldestID == "" || loss.expiresAt.Before(oldest) {
				oldestID, oldest = id, loss.expiresAt
			}
		}
		if _, exists := manager.continuationLosses[responseID]; !exists && len(manager.continuationLosses) >= responseConnBindingMaxEntries {
			delete(manager.continuationLosses, oldestID)
		}
	}
	loss := continuationLoss{accountID: binding.accountID, apiKey: binding.apiKey, requestScope: binding.requestScope,
		persisted: binding.persisted, reason: reason, expiresAt: now.Add(continuationLossTTL)}
	if binding.conn != nil {
		loss.connection = binding.conn.lifecycle("unavailable")
		if loss.connection.ExitReason != "" {
			loss.reason = loss.connection.ExitReason
		}
	}
	manager.continuationLosses[responseID] = loss
}

func (manager *Manager) recordConnectionLoss(connection *WsConnection) {
	manager.respConnMu.Lock()
	defer manager.respConnMu.Unlock()
	for id, binding := range manager.respConnBindings {
		if binding.conn == connection {
			manager.rememberContinuationLossLocked(id, binding, "connection_closed")
			delete(manager.respConnBindings, id)
		}
	}
}

func (manager *Manager) observeContinuationLoss(observer *proxy.TransportObserver, responseID string, accountID int64, apiKey, scope string) {
	manager.respConnMu.Lock()
	loss, found := manager.continuationLosses[responseID]
	manager.respConnMu.Unlock()
	if found && time.Now().Before(loss.expiresAt) && loss.accountID == accountID && loss.apiKey == apiKey && loss.requestScope == scope {
		observer.ConnectionLoss(loss.connection)
	}
}
