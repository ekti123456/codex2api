package wsrelay

import (
	"crypto/sha256"
	"encoding/hex"
	"encoding/json"
	"net/http"
	"sort"
	"strings"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
)

func websocketConnectionProfile(headers http.Header) string {
	filtered := headers.Clone()
	stripCodexHandshakeSnapshotFromProfile(filtered)
	filtered.Del("Sec-WebSocket-Key")
	filtered.Del("X-Request-Id")
	values := make(map[string][]string, len(filtered))
	names := make([]string, 0, len(filtered))
	for name := range filtered {
		names = append(names, name)
	}
	sort.Strings(names)
	for _, name := range names {
		values[strings.ToLower(name)] = append(values[strings.ToLower(name)], filtered[name]...)
	}
	encoded, _ := json.Marshal(values)
	digest := sha256.Sum256(encoded)
	return hex.EncodeToString(digest[:16])
}

func (connection *WsConnection) matchesProfile(headers http.Header) bool {
	matches := connection.handshakeProfile == "" || connection.handshakeProfile == websocketConnectionProfile(headers)
	if !matches && (connection.session == nil || connection.session.PendingCount() == 0) {
		connection.noteExit("handshake_profile_changed", nil)
	}
	return matches
}

func recordConnection(observer *proxy.TransportObserver, connection *WsConnection, headers http.Header) {
	if connection == nil {
		return
	}
	reused := connection.acquisitions.Add(1) > 1
	peer := ""
	if connection.conn != nil && connection.conn.RemoteAddr() != nil {
		peer = connection.conn.RemoteAddr().String()
	}
	observer.Connection(connection.diagnosticID, connection.PoolKey, connection.handshakeProfile, connection.matchesProfile(headers), reused, connection.createdAt, peer, connection.proxyURL)
	observer.OutboundWebsocketHandshake(connection.upstreamIdentity)
	if connection.httpResp != nil {
		observer.ResponseHeaders(connection.httpResp.StatusCode, connection.httpResp.Header, true)
	}
}

func continuationUnavailable() error {
	return &proxy.Error{Code: "response_context_unavailable", Type: proxy.ErrorTypeInvalidRequest,
		HTTPStatus: http.StatusBadRequest, Retryable: false,
		Message: "原响应的连接上下文不可用，请恢复完整上下文或新开对话。"}
}

func (manager *Manager) continuationRequirement(responseID string, accountID int64, apiKey, scope string) (string, string) {
	manager.respConnMu.Lock()
	defer manager.respConnMu.Unlock()
	binding, found := manager.respConnBindings[responseID]
	if !found {
		if loss, exists := manager.continuationLosses[responseID]; exists {
			if time.Now().After(loss.expiresAt) {
				delete(manager.continuationLosses, responseID)
			} else {
				if loss.accountID != accountID || loss.apiKey != apiKey || loss.requestScope != scope {
					return "connection_local", "owner_mismatch"
				}
				if loss.persisted {
					return "persisted", "known"
				}
				return "connection_local", "lost:" + loss.reason
			}
		}
		return "external_unknown", "not_recorded"
	}
	if binding.accountID != accountID || binding.apiKey != apiKey || binding.requestScope != scope {
		return "connection_local", "owner_mismatch"
	}
	if time.Now().After(binding.expiresAt) {
		manager.rememberContinuationLossLocked(responseID, binding, "binding_expired")
		delete(manager.respConnBindings, responseID)
		if binding.persisted {
			return "persisted", "known"
		}
		return "connection_local", "binding_expired"
	}
	if binding.persisted {
		return "persisted", "known"
	}
	return "connection_local", "known"
}

func (manager *Manager) markResponsePersisted(responseID string, connection *WsConnection) {
	manager.respConnMu.Lock()
	defer manager.respConnMu.Unlock()
	if binding, found := manager.respConnBindings[responseID]; found && binding.conn == connection {
		binding.persisted = true
		manager.respConnBindings[responseID] = binding
	}
	if loss, found := manager.continuationLosses[responseID]; found && loss.connection.ConnectionID == connection.diagnosticID {
		loss.persisted = true
		manager.continuationLosses[responseID] = loss
	}
}

func releaseUnsentConnection(manager *Manager, connection *WsConnection, pending *PendingRequest) {
	if !connection.cancelUnsentReadLease(pending.RequestID) {
		manager.DiscardConnection(connection)
	}
	connection.session.RemovePendingRequest(pending.RequestID)
}

func acquireContinuation(manager *Manager, observer *proxy.TransportObserver, responseID string, account *auth.Account, apiKey, scope, wsURL string, headers http.Header, proxyOverride string, downstreamIDs ...string) (*WsConnection, *PendingRequest, string, bool, error) {
	mode, result := manager.continuationRequirement(responseID, account.ID(), apiKey, scope)
	observer.Continuation(mode, result)
	manager.observeContinuationLoss(observer, responseID, account.ID(), apiKey, scope)
	if result == "owner_mismatch" || result == "binding_expired" || strings.HasPrefix(result, "lost:") {
		return nil, nil, "", true, continuationUnavailable()
	}
	connection, pending, slot := manager.AcquirePreferredConnection(responseID, account.ID(), apiKey, scope)
	if connection != nil && len(downstreamIDs) > 0 && connection.downstreamConnectionID != downstreamIDs[0] {
		releaseUnsentConnection(manager, connection, pending)
		connection, pending, slot = nil, nil, ""
		result = "downstream_connection_changed"
	}
	if connection != nil && (connection.URL != wsURL || connection.proxyURL != effectiveProxyURL(account, proxyOverride) || !connection.matchesProfile(headers)) {
		recordConnection(observer, connection, headers)
		releaseUnsentConnection(manager, connection, pending)
		connection, pending, slot = nil, nil, ""
		result = "connection_configuration_changed"
	}
	if connection == nil && mode == "connection_local" {
		if result == "known" {
			result = "original_connection_unavailable_or_busy"
		}
		observer.Continuation(mode, result)
		manager.observeContinuationLoss(observer, responseID, account.ID(), apiKey, scope)
		return nil, nil, "", true, continuationUnavailable()
	}
	if connection != nil {
		observer.Continuation(mode, "original_connection")
	} else {
		observer.Continuation(mode, "fresh_connection_allowed")
	}
	return connection, pending, slot, mode == "connection_local", nil
}
