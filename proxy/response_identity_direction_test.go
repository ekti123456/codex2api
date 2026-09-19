package proxy

import (
	"encoding/json"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

// Regression shape observed in the pre-fix rollout: the official-side turn ID
// reached the client through output-item passthrough metadata.
func TestResponsePrivacyDoesNotEchoOfficialIdentityThroughItemPassthrough(t *testing.T) {
	h, account, _, _ := responsePrivacySetup(t)
	c, _, _ := responsePrivacyRequest(t, h, 101, "turn", "")
	for _, kind := range []string{"message", "reasoning", "custom_tool_call"} {
		t.Run(kind, func(t *testing.T) {
			for _, encoded := range []bool{false, true} {
				metadata := map[string]any{
					"turn_id": "official-mapped-turn", "root_turn_id": "official-mapped-turn",
					"nested": []any{map[string]any{"sessionId": "official-mapped-session", "account_id": "official-account", "request_id": "official-request"}},
				}
				var carrier any = metadata
				if encoded {
					data, err := json.Marshal(metadata)
					require.NoError(t, err)
					carrier = string(data)
				}
				item := map[string]any{"type": kind, "id": "item_keep", "call_id": "call_keep", "internal_chat_message_metadata_passthrough": carrier}
				for _, event := range []map[string]any{
					{"type": "response.output_item.done", "item": item},
					{"type": "response.completed", "response": map[string]any{"id": "resp_official", "output": []any{item}}},
				} {
					raw, err := json.Marshal(event)
					require.NoError(t, err)
					masked, err := maskResponsePayload(c.Request.Context(), account, raw, false)
					require.NoError(t, err)
					for _, value := range []string{"official-mapped-turn", "official-mapped-session", "official-account", "official-request", "resp_official"} {
						require.NotContains(t, string(masked), value)
					}
					require.Contains(t, string(masked), "item_keep")
					require.Contains(t, string(masked), "call_keep")
				}
			}
		})
	}
}

func TestResponsePrivacyPublicAliasesDifferFromOfficialValues(t *testing.T) {
	h, account, _, _ := responsePrivacySetup(t)
	first, _, _ := responsePrivacyRequest(t, h, 101, "turn", "")
	const officialResponse = "resp_official_direction"
	const officialState = "official-turn-state-direction"
	masked, err := maskResponsePayload(first.Request.Context(), account, []byte(`{"id":"`+officialResponse+`","headers":{"x-codex-turn-state":"`+officialState+`"},"metadata":{"nested":{"x-codex-turn-state":"`+officialState+`"}}}`), true)
	require.NoError(t, err)
	responseAlias := gjson.GetBytes(masked, "id").String()
	stateAlias := gjson.GetBytes(masked, "headers.x-codex-turn-state").String()
	require.True(t, h.db.IsManagedCodexResponseID(responseAlias))
	require.True(t, h.db.IsManagedCodexTurnStateAlias(stateAlias))
	require.NotEqual(t, officialResponse, responseAlias)
	require.NotEqual(t, officialState, stateAlias)
	require.Equal(t, stateAlias, gjson.GetBytes(masked, "metadata.nested.x-codex-turn-state").String())
	next, body, _ := responsePrivacyRequest(t, h, 101, "turn", responseAlias)
	require.NoError(t, validateResponseIdentityIngress(next, body))
	body, err = prepareResponseIdentityOutbound(next.Request.Context(), account, body)
	require.NoError(t, err)
	require.Equal(t, officialResponse, gjson.GetBytes(body, "previous_response_id").String())
	// The public token resolves to a different official-side value in storage.
	record, found, err := h.db.ReadCodexTurnState(t.Context(), stateAlias)
	require.NoError(t, err)
	require.True(t, found)
	require.Equal(t, officialState, record.Real)
}
