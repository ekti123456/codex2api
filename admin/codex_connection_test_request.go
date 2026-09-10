package admin

import (
	"encoding/json"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/proxy"
	"github.com/tidwall/sjson"
)

func buildCodexIndependentTestPayload(account *auth.Account, model, content string) []byte {
	payload := buildTestPayloadWithContent(model, content)
	sessionID := proxy.NewUpstreamSessionUUID()
	turnID := proxy.NewUpstreamSessionUUID()
	windowID := sessionID + ":0"
	turnMetadata := map[string]any{
		"request_kind":            "turn",
		"session_id":              sessionID,
		"thread_id":               sessionID,
		"turn_id":                 turnID,
		"window_id":               windowID,
		"turn_started_at_unix_ms": time.Now().UnixMilli(),
	}
	clientMetadata := map[string]any{
		"session_id":          sessionID,
		"thread_id":           sessionID,
		"turn_id":             turnID,
		"x-codex-window-id":   windowID,
		"x-client-request-id": sessionID,
	}
	if account != nil {
		if installationID := account.EffectiveCodexInstallationID(); installationID != "" {
			turnMetadata["installation_id"] = installationID
			clientMetadata["x-codex-installation-id"] = installationID
		}
	}
	encodedMetadata, _ := json.Marshal(turnMetadata)
	clientMetadata["x-codex-turn-metadata"] = string(encodedMetadata)
	payload, _ = sjson.SetBytes(payload, "client_metadata", clientMetadata)
	payload, _ = sjson.SetBytes(payload, "prompt_cache_key", sessionID)
	return payload
}
