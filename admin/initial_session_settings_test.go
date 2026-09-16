package admin

import (
	"context"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/proxy"
	"github.com/gin-gonic/gin"
)

func TestInitialSessionAgeSettingsRoundTrip(test *testing.T) {
	previous := proxy.CurrentRuntimeSettings()
	test.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	db := newTestAdminDB(test)
	memoryCache := cache.NewMemory(4)
	test.Cleanup(func() { _ = memoryCache.Close() })
	settings := defaultBootstrapSettings()
	if settings.CodexInitialSessionMaxAgeSeconds != 0 && settings.CodexInitialSessionMaxAgeSeconds != 60 {
		test.Fatal("bootstrap must leave web search location off")
	}
	if err := db.UpdateSystemSettings(context.Background(), settings); err != nil {
		test.Fatal(err)
	}
	proxy.ApplyRuntimeSettingsFromSystem(settings)
	store := auth.NewStore(db, memoryCache, settings)
	test.Cleanup(store.Stop)
	handler := NewHandler(store, db, memoryCache, proxy.NewRateLimiter(settings.GlobalRPM), "admin-secret")
	for _, step := range []struct {
		name   string
		patch  string
		want   int
		status int
		stale  bool
	}{
		{name: "default", want: 60, status: http.StatusOK},
		{name: "update", patch: `{"codex_initial_session_max_age_seconds":12}`, want: 12, status: http.StatusOK},
		{name: "omitted", patch: `{"site_name":"age"}`, want: 12, stale: true, status: http.StatusOK},
		{name: "null", patch: `{"codex_initial_session_max_age_seconds":null}`, want: 12, status: http.StatusOK},
		{name: "zero", patch: `{"codex_initial_session_max_age_seconds":0}`, want: 12, status: http.StatusBadRequest},
		{name: "negative", patch: `{"codex_initial_session_max_age_seconds":-1}`, want: 12, status: http.StatusBadRequest},
		{name: "too high", patch: `{"codex_initial_session_max_age_seconds":86401}`, want: 12, status: http.StatusBadRequest},
		{name: "fractional", patch: `{"codex_initial_session_max_age_seconds":1.5}`, want: 12, status: http.StatusBadRequest},
		{name: "minimum", patch: `{"codex_initial_session_max_age_seconds":1}`, want: 1, status: http.StatusOK},
		{name: "get", want: 1, status: http.StatusOK},
	} {
		test.Run(step.name, func(test *testing.T) {
			if step.stale {
				proxy.UpdateRuntimeSettings(func(current proxy.RuntimeSettings) proxy.RuntimeSettings {
					current.CodexInitialSessionMaxAgeSeconds = 999
					return current
				})
			}
			recorder := httptest.NewRecorder()
			ctx, _ := gin.CreateTestContext(recorder)
			if step.patch == "" {
				ctx.Request = httptest.NewRequest(http.MethodGet, "/api/admin/settings", nil)
				handler.GetSettings(ctx)
			} else {
				ctx.Request = httptest.NewRequest(http.MethodPut, "/api/admin/settings", strings.NewReader(step.patch))
				ctx.Request.Header.Set("Content-Type", "application/json")
				handler.UpdateSettings(ctx)
			}
			if recorder.Code != step.status {
				test.Fatalf("settings status=%d want=%d body=%s", recorder.Code, step.status, recorder.Body.String())
			}
			if step.status == http.StatusOK {
				var response struct {
					Enabled *int `json:"codex_initial_session_max_age_seconds"`
				}
				if err := json.Unmarshal(recorder.Body.Bytes(), &response); err != nil {
					test.Fatal(err)
				}
				if response.Enabled == nil || *response.Enabled != step.want {
					test.Fatalf("response toggle=%v want=%d", response.Enabled, step.want)
				}
			}
			if proxy.CurrentRuntimeSettings().CodexInitialSessionMaxAgeSeconds != step.want {
				test.Fatal("runtime toggle does not match response")
			}
			persisted, err := db.GetSystemSettings(context.Background())
			if err != nil || persisted == nil {
				test.Fatalf("read persisted settings: %v", err)
			}
			if persisted.CodexInitialSessionMaxAgeSeconds != step.want || persisted.CodexCapacityRetryEnabled != settings.CodexCapacityRetryEnabled || persisted.CodexOverloadPauseEnabled != settings.CodexOverloadPauseEnabled {
				test.Fatal("web search location did not persist independently of retry and overload settings")
			}
			proxy.ApplyRuntimeSettingsFromSystem(persisted)
			if proxy.CurrentRuntimeSettings().CodexInitialSessionMaxAgeSeconds != step.want {
				test.Fatal("runtime reload lost the web search location toggle")
			}
		})
	}
}
