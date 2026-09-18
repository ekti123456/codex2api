package proxy

import (
	"context"
	"encoding/base64"
	"sync"
	"unicode/utf8"

	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
)

type usageTurnStateKey struct{}

// Each upstream attempt owns its observation. A late read from an earlier
// attempt cannot overwrite the next attempt, even when accounts are reused.
type usageTurnStateObservation struct {
	mu                   sync.Mutex
	length, decodedBytes *int
}

func beginUsageTurnStateAttempt(c *gin.Context) {
	if c != nil && c.Request != nil {
		c.Request = c.Request.WithContext(context.WithValue(c.Request.Context(), usageTurnStateKey{}, &usageTurnStateObservation{}))
	}
}

func observeUsageTurnState(ctx context.Context, real string) {
	observation, _ := ctx.Value(usageTurnStateKey{}).(*usageTurnStateObservation)
	if observation == nil {
		return
	}
	observation.mu.Lock()
	defer observation.mu.Unlock()
	// Keep the first real token for this attempt, matching the header delivered
	// to Codex. An empty HTTP header may be followed by a nonempty WS/SSE event.
	if observation.length != nil && *observation.length > 0 {
		return
	}
	length := utf8.RuneCountInString(real)
	observation.length = &length
	if length == 0 {
		return
	}
	for _, encoding := range []*base64.Encoding{base64.URLEncoding, base64.RawURLEncoding, base64.StdEncoding, base64.RawStdEncoding} {
		if decoded, err := encoding.Strict().DecodeString(real); err == nil {
			size := len(decoded)
			observation.decodedBytes = &size
			break
		}
	}
}

func populateUsageTurnState(c *gin.Context, input *database.UsageLogInput) {
	if c == nil || c.Request == nil || input == nil {
		return
	}
	observation, _ := c.Request.Context().Value(usageTurnStateKey{}).(*usageTurnStateObservation)
	if observation == nil {
		return
	}
	observation.mu.Lock()
	defer observation.mu.Unlock()
	// Copy scalars; the asynchronous log writer must not retain mutable state.
	if observation.length != nil {
		length := *observation.length
		input.TurnStateLength = &length
	}
	if observation.decodedBytes != nil {
		size := *observation.decodedBytes
		input.TurnStateDecodedBytes = &size
	}
}
