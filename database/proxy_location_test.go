package database

import (
	"context"
	"errors"
	"path/filepath"
	"testing"
)

func TestProxyLocationPersistenceAndOverrides(t *testing.T) {
	ctx := context.Background()
	path := filepath.Join(t.TempDir(), "location.db")
	db, err := New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	t.Cleanup(func() { _ = db.Close() })
	url := "http://geo.example:8080"
	id, err := db.InsertProxy(ctx, url, "location")
	if err != nil {
		t.Fatal(err)
	}
	manualCountry, manualRegion, manualCity := " ca ", " Ontario ", " Toronto "
	if err := db.UpdateProxyLocationSettings(ctx, id, nil, nil, nil, nil, ProxyLocationOverrides{&manualCountry, &manualRegion, &manualCity}); err != nil {
		t.Fatal(err)
	}
	geo := ProxyLocation{Country: "us", Region: "Ohio", City: "Piketon", Timezone: "America/New_York"}
	if err := db.UpdateProxyTestLocationResult(ctx, id, url, ProxyTestStatusSuccess, "1.2.3.4", "US", 3, geo); err != nil {
		t.Fatal(err)
	}
	if err := db.Close(); err != nil {
		t.Fatal(err)
	}
	db, err = New("sqlite", path)
	if err != nil {
		t.Fatal(err)
	}
	check := func(want ProxyLocation) {
		t.Helper()
		row, err := db.GetProxy(ctx, id)
		if err != nil {
			t.Fatal(err)
		}
		got := ProxyLocation{Country: row.TestCountryCode, Region: row.TestRegion, City: row.TestCity, Timezone: row.TestTimezone}
		if got != want || row.CountryCodeOverride != "CA" || row.RegionOverride != "Ontario" || row.CityOverride != "Toronto" {
			t.Fatalf("stored detected/manual location: %+v", row)
		}
		for _, list := range []func(context.Context) ([]*ProxyRow, error){db.ListProxies, db.ListEnabledProxies, func(ctx context.Context) ([]*ProxyRow, error) { return db.ListProxiesByIDs(ctx, []int64{id}) }} {
			rows, err := list(ctx)
			if err != nil || len(rows) != 1 || *rows[0] != *row {
				t.Fatalf("list mismatch: %v %v", rows, err)
			}
		}
	}
	geo.Country = "US"
	check(geo)
	// Missing fields on the same IP and failed tests keep saved location.
	if err := db.UpdateProxyTestLocationResult(ctx, id, url, ProxyTestStatusSuccess, "1.2.3.4", "", 1, ProxyLocation{}); err != nil {
		t.Fatal(err)
	}
	check(geo)
	if err := db.UpdateProxyTestLocationResult(ctx, id, url, ProxyTestStatusError, "", "", 0, ProxyLocation{}); err != nil {
		t.Fatal(err)
	}
	row, err := db.GetProxy(ctx, id)
	if err != nil || row.TestCity != "Piketon" || row.CityOverride != "Toronto" {
		t.Fatalf("failed probe lost saved location: %+v %v", row, err)
	}
	if err := db.UpdateProxyTestLocationResult(ctx, id, url, ProxyTestStatusSuccess, "5.6.7.8", "", 1, ProxyLocation{Country: "GB"}); err != nil {
		t.Fatal(err)
	}
	check(ProxyLocation{Country: "GB"})
	invalid := "USA"
	label := "must not be saved"
	if err := db.UpdateProxyLocationSettings(ctx, id, nil, &label, nil, nil, ProxyLocationOverrides{CountryCode: &invalid}); err == nil {
		t.Fatal("accepted invalid country")
	}
	row, _ = db.GetProxy(ctx, id)
	if row.Label != "location" {
		t.Fatal("invalid update partially saved")
	}
	newURL := "http://new-geo.example:8080"
	if err := db.UpdateProxy(ctx, id, &newURL, nil, nil); err != nil {
		t.Fatal(err)
	}
	check(ProxyLocation{})
	if err := db.UpdateProxyTestLocationResult(ctx, id, url, ProxyTestStatusSuccess, "1.2.3.4", "", 1, geo); !errors.Is(err, ErrProxyTestTargetChanged) {
		t.Fatalf("stale probe: %v", err)
	}
	empty := ""
	if err := db.UpdateProxyLocationSettings(ctx, id, nil, nil, nil, nil, ProxyLocationOverrides{&empty, &empty, &empty}); err != nil {
		t.Fatal(err)
	}
	row, _ = db.GetProxy(ctx, id)
	if row.CountryCodeOverride != "" || row.RegionOverride != "" || row.CityOverride != "" {
		t.Fatal("manual values not cleared")
	}
}
