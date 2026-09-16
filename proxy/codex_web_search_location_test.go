package proxy

import (
	"bytes"
	"context"
	"testing"

	"github.com/codex2api/database"
	"github.com/tidwall/gjson"
)

func TestCodexWebSearchLocationSwitchAndInputPreservation(t *testing.T) {
	previousResin := GetResinConfig()
	SetResinConfig(nil)
	t.Cleanup(func() { SetResinConfig(previousResin) })
	body := []byte(`{"input":[{"type":"reasoning","id":"rs_old","encrypted_content":"unchanged"},{"type":"function_call","call_id":"c1","arguments":"{}"}],"tools":[{"type":"web_search","filters":{"allowed_domains":["example.com"]},"user_location":{"type":"approximate","country":"CN","city":"old"}},{"type":"web_search_preview"},{"type":"function","name":"web_search","parameters":{"type":"object"}}]}`)
	calls := map[string]int{}
	geo := database.ProxyLocation{Country: "US", Region: "Ohio", City: "Piketon", Timezone: "America/New_York"}
	resolve := func(url string) database.ProxyLocation {
		calls[url]++
		if url == "proxy-a" {
			return geo
		}
		if url == "proxy-b" {
			return database.ProxyLocation{Country: "JP"}
		}
		return database.ProxyLocation{}
	}
	if got := ApplyCodexOutboundLocation(WithCodexWebSearchLocation(context.Background(), false, resolve), body, "proxy-a"); !bytes.Equal(got, body) || len(calls) != 0 {
		t.Fatal("default off mutated payload or resolved")
	}
	ctx := context.WithValue(context.Background(), sessionAccountFailoverContextKey{}, &sessionAccountFailoverPlan{PreserveInput: true})
	ctx = WithCodexWebSearchLocation(ctx, true, resolve)
	first := ApplyCodexOutboundLocation(ctx, body, "proxy-a")
	disabled := WithCodexWebSearchLocation(ctx, false, resolve)
	if got := ApplyCodexOutboundLocation(disabled, body, "proxy-a"); !bytes.Equal(got, body) {
		t.Fatal("disabled frame inherited enabled state from previous frame")
	}
	if gjson.GetBytes(first, "tools.0.user_location.city").String() != "Piketon" || gjson.GetBytes(first, "tools.1.user_location.country").String() != "US" {
		t.Fatalf("not applied: %s", first)
	}
	if gjson.GetBytes(first, "input").Raw != gjson.GetBytes(body, "input").Raw || gjson.GetBytes(first, "tools.2").Raw != gjson.GetBytes(body, "tools.2").Raw || gjson.GetBytes(first, "tools.0.filters").Raw != gjson.GetBytes(body, "tools.0.filters").Raw {
		t.Fatal("changed input, function definition or search filters")
	}
	geo.City = "New York"
	if again := ApplyCodexOutboundLocation(ctx, body, "proxy-a"); !bytes.Equal(again, first) || calls["proxy-a"] != 1 {
		t.Fatal("same request did not use stable cached location")
	}
	second := ApplyCodexOutboundLocation(ctx, body, "proxy-b")
	if gjson.GetBytes(second, "tools.0.user_location.country").String() != "JP" || gjson.GetBytes(second, "tools.0.user_location.city").Exists() {
		t.Fatalf("switch kept old city: %s", second)
	}
	for _, url := range []string{"", "unknown"} {
		if got := ApplyCodexOutboundLocation(ctx, body, url); !bytes.Equal(got, body) {
			t.Fatal("missing proxy location modified body")
		}
	}
	withoutTools := []byte(`{"input":"hello"}`)
	if got := ApplyCodexOutboundLocation(ctx, withoutTools, "proxy-a"); !bytes.Equal(got, withoutTools) {
		t.Fatal("created tools")
	}
	fresh := WithCodexWebSearchLocation(context.Background(), true, resolve)
	if got := ApplyCodexOutboundLocation(fresh, body, "proxy-a"); gjson.GetBytes(got, "tools.0.user_location.city").String() != "New York" {
		t.Fatal("new request did not see updated cache")
	}
	SetResinConfig(&ResinConfig{BaseURL: "http://resin.invalid", PlatformName: "test"})
	if got := ApplyCodexOutboundLocation(fresh, body, "proxy-a"); !bytes.Equal(got, body) {
		t.Fatal("used configured proxy location for unknown Resin exit")
	}
}
