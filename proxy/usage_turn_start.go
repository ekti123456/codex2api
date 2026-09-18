package proxy

import (
	"context"
	"strconv"
	"time"

	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
)

type usageTurnStart struct {
	TurnID  string `json:"turn_id"`
	First   *bool  `json:"is_first_request"`
	Preview string `json:"prompt_preview,omitempty"`
	logged  bool
}

func (h *Handler) captureUsageTurnStart(c *gin.Context, body []byte, state *usageRequestDiagnostics) {
	if state.turnStartCaptured || h.db == nil || state.Resolved == nil {
		return
	}
	state.turnStartCaptured = true
	r := state.Resolved
	if r.RootState != "resolved" || r.Passive || r.SubagentKind != "" ||
		(r.ThreadSource != "user" && r.ThreadSource != "") {
		return
	}
	// Use the captured original identities, before outbound identity rewriting.
	thread, turn, started := "", "", ""
	for _, source := range []string{"turn_metadata_header", "client_metadata.x-codex-turn-metadata", "client_metadata"} {
		m := state.Incoming[source]
		for key, target := range map[string]*string{"thread_id": &thread, "turn_id": &turn, "turn_started_at_unix_ms": &started} {
			if value := m[key]; value != "" {
				if *target != "" && *target != value {
					return // Conflicting identity is unknown, never a first turn.
				}
				*target = value
			}
		}
	}
	if !validSessionGraphUUID(thread) || !validSessionGraphUUID(turn) {
		return
	}
	state.TurnStart = &usageTurnStart{TurnID: turn}
	millis, err := strconv.ParseInt(started, 10, 64)
	if err != nil || millis <= 0 || millis > state.StartedAt.Add(time.Minute).UnixMilli() {
		return
	}
	preview, direct := promptfilter.UsageUserPreview(ingressRequestBody(c, body))
	if r.RequestKind == "compaction" || (state.ResponsesInput != nil && state.ResponsesInput.Mode != "ordinary") {
		direct = false
	}
	scope := codexIdentityDigest("usage-turn-start-v1", responseCacheOwnerForRequest(c, requestAPIKeyID(c)), thread, turn)
	ctx, cancel := context.WithTimeout(c.Request.Context(), time.Second)
	defer cancel()
	first, err := h.db.ObserveUsageTurnStart(ctx, scope, state.CorrelationID, time.UnixMilli(millis), direct)
	if err != nil {
		return // Observability failure must not interrupt model requests.
	}
	state.TurnStart.First = first
	if first != nil && *first {
		state.TurnStart.Preview = preview
	}
}

func populateUsageTurnStart(state *usageRequestDiagnostics, input *database.UsageLogInput) {
	turn := state.TurnStart
	if turn == nil {
		return
	}
	input.TurnID = turn.TurnID
	if turn.First == nil || input.RequestType != "user" {
		return
	}
	first := *turn.First && !turn.logged
	input.IsTurnFirstRequest = &first
	if first {
		input.TurnPromptPreview = turn.Preview
		turn.logged = true // A retry attempt must not duplicate the badge.
	}
}
