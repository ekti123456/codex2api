package admin

import (
	"context"
	"net/http"
	"testing"

	"github.com/codex2api/proxy"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestSettingsCodexTelemetryDefaultAndPartialUpdates(test *testing.T) {
	previous := proxy.CurrentRuntimeSettings()
	test.Cleanup(func() { proxy.ApplyRuntimeSettings(previous) })
	proxy.ApplyRuntimeSettings(proxy.DefaultRuntimeSettings())
	handler, db, _ := newResponseCacheSettingsAdminHandler(test)
	get := invokeResponseCacheSettingsAdmin(test, handler, http.MethodGet, nil)
	require.Equal(test, http.StatusOK, get.Code)
	require.Equal(test, gjson.False, gjson.GetBytes(get.Body.Bytes(), "codex_telemetry_enabled").Type)
	for _, enabled := range []bool{true, false} {
		update := invokeResponseCacheSettingsAdmin(test, handler, http.MethodPut, map[string]any{"codex_telemetry_enabled": enabled})
		require.Equal(test, http.StatusOK, update.Code, update.Body.String())
		require.Equal(test, enabled, decodeResponseCacheSettingsResponse(test, update).CodexTelemetryEnabled)
		require.Equal(test, enabled, proxy.CurrentRuntimeSettings().CodexTelemetryEnabled)
		unrelated := invokeResponseCacheSettingsAdmin(test, handler, http.MethodPut, map[string]any{"site_name": "unchanged telemetry"})
		require.Equal(test, http.StatusOK, unrelated.Code, unrelated.Body.String())
		persisted, err := db.GetSystemSettings(context.Background())
		require.NoError(test, err)
		require.Equal(test, enabled, persisted.CodexTelemetryEnabled)
		get = invokeResponseCacheSettingsAdmin(test, handler, http.MethodGet, nil)
		require.Equal(test, enabled, decodeResponseCacheSettingsResponse(test, get).CodexTelemetryEnabled)
	}
}
