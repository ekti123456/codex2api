package proxy

import (
	"context"
	"strconv"
	"strings"
	"sync"

	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type codexWebSearchLocationKey struct{}
type codexWebSearchLocationState struct {
	mu        sync.Mutex
	resolve   func(string) database.ProxyLocation
	locations map[string]database.ProxyLocation
}

// WithCodexWebSearchLocation binds a location resolver and snapshots the enabled setting for a request.
func WithCodexWebSearchLocation(ctx context.Context, enabled bool, resolve func(string) database.ProxyLocation) context.Context {
	if !enabled || resolve == nil {
		// A persistent downstream WebSocket may reuse the previous frame's
		// context. Shadow its enabled state when the setting is turned off.
		return context.WithValue(ctx, codexWebSearchLocationKey{}, (*codexWebSearchLocationState)(nil))
	}
	return context.WithValue(ctx, codexWebSearchLocationKey{}, &codexWebSearchLocationState{
		resolve: resolve, locations: make(map[string]database.ProxyLocation),
	})
}

// ApplyCodexOutboundLocation preserves the existing environment behavior and
// independently updates search tools, including when input must stay intact.
func ApplyCodexOutboundLocation(ctx context.Context, body []byte, proxyURL string) []byte {
	body = ApplyCodexEnvironment(ctx, body, proxyURL)
	if ctx == nil || IsResinEnabled() {
		return body
	}
	state, _ := ctx.Value(codexWebSearchLocationKey{}).(*codexWebSearchLocationState)
	proxyURL = strings.TrimSpace(proxyURL)
	if state == nil || proxyURL == "" {
		return body
	}
	state.mu.Lock()
	location, exists := state.locations[proxyURL]
	if !exists {
		location = state.resolve(proxyURL)
		state.locations[proxyURL] = location
	}
	state.mu.Unlock()
	if location == (database.ProxyLocation{}) {
		return body
	}
	tools := gjson.GetBytes(body, "tools")
	if !tools.IsArray() {
		return body
	}
	value := map[string]string{"type": "approximate"}
	for key, field := range map[string]string{"country": location.Country, "region": location.Region, "city": location.City, "timezone": location.Timezone} {
		if field != "" {
			value[key] = field
		}
	}
	original := body
	for i, tool := range tools.Array() {
		kind := tool.Get("type").String()
		if kind != "web_search" && !strings.HasPrefix(kind, "web_search_") {
			continue
		}
		var err error
		body, err = sjson.SetBytes(body, "tools."+strconv.Itoa(i)+".user_location", value)
		if err != nil {
			return original
		}
	}
	return body
}
