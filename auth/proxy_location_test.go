package auth

import (
	"context"
	"testing"

	"github.com/codex2api/database"
)

func TestProxyLocationReloadAndManualPriority(t *testing.T) {
	rows := []*database.ProxyRow{{URL: "http://a:8080", TestCountryCode: "US", TestRegion: "Ohio", TestCity: "Piketon", TestTimezone: "America/New_York", CityOverride: "Columbus"}, {URL: "http://b:8080", CountryCodeOverride: "JP", CityOverride: "Tokyo", TimezoneOverride: "Asia/Tokyo"}}
	store := &Store{proxyPoolLoader: func(context.Context) ([]*database.ProxyRow, error) { return rows, nil }}
	if err := store.ReloadProxyPool(); err != nil {
		t.Fatal(err)
	}
	want := database.ProxyLocation{Country: "US", Region: "Ohio", City: "Columbus", Timezone: "America/New_York"}
	if got := store.ProxyLocation(rows[0].URL); got != want {
		t.Fatalf("priority: %+v", got)
	}
	if got := store.ProxyLocation(rows[1].URL); got.Country != "JP" || got.City != "Tokyo" {
		t.Fatalf("manual only: %+v", got)
	}
	rows[0].CityOverride = ""
	rows[0].TestCity = "Dayton"
	if got := store.ProxyLocation(rows[0].URL); got != want {
		t.Fatal("cache changed without reload")
	}
	if err := store.ReloadProxyPool(); err != nil {
		t.Fatal(err)
	}
	if got := store.ProxyLocation(rows[0].URL); got.City != "Dayton" {
		t.Fatalf("clear manual: %+v", got)
	}
	store.RemoveProxyURLs([]string{rows[0].URL})
	if store.ProxyLocation(rows[0].URL) != (database.ProxyLocation{}) || store.ProxyLocation("unknown") != (database.ProxyLocation{}) {
		t.Fatal("stale/unknown location")
	}
}
