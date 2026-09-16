package admin

import (
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/gin-gonic/gin"
	"github.com/tidwall/gjson"
)

func TestProxyLocationProbeEnglishAndCountryCode(t *testing.T) {
	server := httptest.NewServer(http.HandlerFunc(func(w http.ResponseWriter, r *http.Request) {
		if r.URL.Query().Get("lang") != "en" || !strings.Contains(r.URL.Query().Get("fields"), "countryCode") {
			t.Errorf("geo query: %s", r.URL)
		}
		fmt.Fprint(w, `{"status":"success","query":"1.2.3.4","country":"United States","countryCode":"US","regionName":"Ohio","city":"Piketon","timezone":"America/New_York"}`)
	}))
	defer server.Close()
	result := probeProxy(context.Background(), server.URL, "zh-CN")
	if !result.Success || result.CountryCode != "US" || result.Region != "Ohio" || result.City != "Piketon" {
		t.Fatalf("probe: %+v", result)
	}
	_, _, _, _, _, code := parseIPWhoisGeoFields(gjson.Parse(`{"country_code":"gb"}`))
	if code != "GB" {
		t.Fatalf("fallback code: %s", code)
	}
}

func TestProxyLocationSingleBatchAndEdit(t *testing.T) {
	db := newAdminProxyTestDB(t)
	store := newAdminProxyTestStore(t, db)
	url := "http://geo.example:8080"
	id, err := db.InsertProxy(context.Background(), url, "")
	if err != nil {
		t.Fatal(err)
	}
	handler := &Handler{db: db, store: store, proxyProbe: func(context.Context, string, string) proxyProbeResult {
		return proxyProbeResult{Success: true, Conclusive: true, IP: "1.2.3.4", CountryCode: "US", Region: "Ohio", City: "Piketon", Timezone: "America/New_York"}
	}}
	call := func(path, body string, action func(*gin.Context)) *httptest.ResponseRecorder {
		t.Helper()
		rec := httptest.NewRecorder()
		ctx, _ := gin.CreateTestContext(rec)
		ctx.Request = httptest.NewRequest(http.MethodPost, path, strings.NewReader(body))
		ctx.Request.Header.Set("Content-Type", "application/json")
		ctx.Params = gin.Params{{Key: "id", Value: fmt.Sprint(id)}}
		action(ctx)
		return rec
	}
	for _, batch := range []bool{false, true} {
		var rec *httptest.ResponseRecorder
		if batch {
			rec = call("/api/admin/proxies/test-all", fmt.Sprintf(`{"ids":[%d]}`, id), handler.TestAllProxies)
		} else {
			rec = call("/api/admin/proxies/test", fmt.Sprintf(`{"url":%q,"id":%d}`, url, id), handler.TestProxy)
		}
		if rec.Code != 200 {
			t.Fatalf("test: %s", rec.Body.String())
		}
		row, err := db.GetProxy(context.Background(), id)
		if err != nil || row.TestCountryCode != "US" || row.TestRegion != "Ohio" || row.TestCity != "Piketon" {
			t.Fatalf("save: %+v %v", row, err)
		}
		if got := store.ProxyLocation(url); got.City != "Piketon" {
			t.Fatalf("test reload: %+v", got)
		}
	}
	for _, body := range []string{`{"country_code_override":"USA"}`, `{"city_override":"bad\nvalue"}`} {
		if rec := call("/api/admin/proxies/1", body, handler.UpdateProxy); rec.Code != 400 {
			t.Fatalf("invalid edit: %d %s", rec.Code, rec.Body.String())
		}
	}
	if rec := call("/api/admin/proxies/1", `{"country_code_override":"ca","region_override":"Ontario","city_override":"Toronto","timezone_override":"America/Toronto"}`, handler.UpdateProxy); rec.Code != 200 {
		t.Fatalf("edit: %s", rec.Body.String())
	}
	call("/api/admin/proxies/test", fmt.Sprintf(`{"url":%q,"id":%d}`, url, id), handler.TestProxy)
	if got := store.ProxyLocation(url); got.Country != "CA" || got.City != "Toronto" || got.Timezone != "America/Toronto" {
		t.Fatalf("retest overwrote manual: %+v", got)
	}
	if rec := call("/api/admin/proxies/1", `{"country_code_override":"","region_override":"","city_override":"","timezone_override":""}`, handler.UpdateProxy); rec.Code != 200 {
		t.Fatalf("clear: %s", rec.Body.String())
	}
	if got := store.ProxyLocation(url); got.Country != "US" || got.City != "Piketon" {
		t.Fatalf("fallback: %+v", got)
	}
}
