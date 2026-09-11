package proxy

import (
	"encoding/json"
	"log"
	"sync"
	"time"

	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type WebsocketConnectionLifecycle struct {
	ConnectionID string    `json:"connection_id"`
	AccountID    int64     `json:"account_id"`
	PoolKeyHash  string    `json:"pool_key_hash"`
	State        string    `json:"state"`
	OpenedAt     time.Time `json:"opened_at"`
	ObservedAt   time.Time `json:"observed_at"`
	ExitReason   string    `json:"exit_reason,omitempty"`
	CloseCode    int       `json:"close_code,omitempty"`
	AgeMillis    int64     `json:"age_millis"`
	IdleMillis   int64     `json:"idle_millis"`
	Pending      int       `json:"pending"`
}

var websocketLifecycleHistory = struct {
	sync.Mutex
	items map[string]WebsocketConnectionLifecycle
}{items: make(map[string]WebsocketConnectionLifecycle)}

const websocketLifecycleRetention = 30 * time.Minute
const websocketLifecycleLimit = 4096

func RecordWebsocketLifecycle(event WebsocketConnectionLifecycle) {
	if event.ConnectionID == "" {
		return
	}
	websocketLifecycleHistory.Lock()
	now := time.Now()
	if len(websocketLifecycleHistory.items) >= websocketLifecycleLimit {
		oldestID := ""
		var oldest time.Time
		for id, item := range websocketLifecycleHistory.items {
			if now.Sub(item.ObservedAt) > websocketLifecycleRetention {
				delete(websocketLifecycleHistory.items, id)
				continue
			}
			if oldestID == "" || item.ObservedAt.Before(oldest) {
				oldestID, oldest = id, item.ObservedAt
			}
		}
		if _, exists := websocketLifecycleHistory.items[event.ConnectionID]; !exists && len(websocketLifecycleHistory.items) >= websocketLifecycleLimit {
			delete(websocketLifecycleHistory.items, oldestID)
		}
	}
	websocketLifecycleHistory.items[event.ConnectionID] = event
	websocketLifecycleHistory.Unlock()
	encoded, _ := json.Marshal(event)
	log.Printf("[WS lifecycle] %s", encoded)
}

func EnrichWebsocketLifecycle(payload []byte) []byte {
	return enrichWebsocketLifecycle(payload, "upstream.")
}

func EnrichUpstreamWebsocketLifecycle(payload []byte) []byte {
	return enrichWebsocketLifecycle(payload, "")
}

func enrichWebsocketLifecycle(payload []byte, prefix string) []byte {
	connectionID := gjson.GetBytes(payload, prefix+"connection_id").String()
	if connectionID == "" {
		return payload
	}
	accountID := gjson.GetBytes(payload, prefix+"account_id").Int()
	websocketLifecycleHistory.Lock()
	event, found := websocketLifecycleHistory.items[connectionID]
	if found && time.Since(event.ObservedAt) > websocketLifecycleRetention {
		delete(websocketLifecycleHistory.items, connectionID)
		found = false
	}
	websocketLifecycleHistory.Unlock()
	if !found || event.AccountID != accountID {
		return payload
	}
	updated, err := sjson.SetBytes(payload, prefix+"connection_lifecycle", event)
	if err != nil {
		return payload
	}
	return updated
}

func (observer *TransportObserver) ConnectionLoss(event WebsocketConnectionLifecycle) {
	observer.update(func(diagnostic *UpstreamTransportDiagnostic) {
		diagnostic.LostConnection = &event
	})
}
