package admin

import (
	"bytes"
	"encoding/json"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/proxy"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestBuiltinRuleEditingPersistsAndPreservesOtherSettings(t *testing.T) {
	db := newTestAdminDB(t)
	tc := cache.NewMemory(4)
	t.Cleanup(func() { _ = tc.Close() })
	settings := defaultBootstrapSettings()
	settings.PromptFilterDisabledPatterns = `["prompt_fake_authorization"]`
	require.NoError(t, db.UpdateSystemSettings(t.Context(), settings))
	store := auth.NewStore(db, tc, settings)
	t.Cleanup(store.Stop)
	h := NewHandler(store, db, tc, proxy.NewRateLimiter(settings.GlobalRPM), "admin-secret")
	var original promptfilter.BuiltinPatternOverride
	for _, pattern := range promptfilter.BuiltinPatternConfigs() {
		if pattern.Name == "prompt_fake_authorization" {
			original = promptfilter.BuiltinPatternFields(pattern)
		}
	}
	require.NotEmpty(t, original.Name)
	send := func(expected *promptfilter.BuiltinPatternOverride, edit *promptfilter.BuiltinPatternOverride) *httptest.ResponseRecorder {
		body, err := json.Marshal(map[string]any{"expected": expected, "rule": edit})
		require.NoError(t, err)
		w := httptest.NewRecorder()
		c, _ := gin.CreateTestContext(w)
		c.Request = httptest.NewRequest(http.MethodPut, "/prompt-filter/rules/builtin/"+original.Name, bytes.NewReader(body))
		c.Params = gin.Params{{Key: "name", Value: original.Name}}
		h.UpdatePromptFilterBuiltinRule(c)
		return w
	}
	edit := original
	edit.Pattern, edit.Weight = "builtin_override_probe_987654", 83
	signalOnly, minimum := true, 2
	edit.SignalOnly, edit.MinMatches = &signalOnly, &minimum
	edit.AllPatterns = []string{"required_probe"}
	edit.AnyPatterns = []string{"alternative_alpha", "alternative_beta"}
	edit.ExcludePatterns = []string{"excluded_probe"}
	edit.AuthorizationExcludePatterns = []string{"authorized_probe"}
	w := send(&original, &edit)
	require.Equal(t, 200, w.Code, w.Body.String())
	var rules promptFilterRulesResponse
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &rules))
	for _, rule := range rules.BuiltinPatterns {
		if rule.Name == original.Name {
			require.True(t, rule.Overridden)
			require.False(t, rule.Enabled)
			require.Equal(t, original, *rule.Default)
			require.Equal(t, edit.Pattern, rule.Pattern)
			require.True(t, rule.SignalOnly)
			require.Equal(t, edit.AllPatterns, rule.AllPatterns)
			require.Equal(t, edit.AnyPatterns, rule.AnyPatterns)
			require.Equal(t, edit.ExcludePatterns, rule.ExcludePatterns)
			require.Equal(t, edit.AuthorizationExcludePatterns, rule.AuthorizationExcludePatterns)
			require.Equal(t, 2, rule.MinMatches)
		}
	}
	require.Equal(t, []promptfilter.BuiltinPatternOverride{edit}, store.GetPromptFilterConfig().BuiltinOverrides)
	// A stale editor cannot overwrite this change, including by restoring defaults.
	require.Equal(t, 409, send(&original, nil).Code)
	staleConditions := edit
	staleConditions.ExcludePatterns = []string{"stale_exclusion"}
	require.Equal(t, 409, send(&staleConditions, nil).Code)
	invalid := edit
	invalid.Pattern = "["
	require.Equal(t, 400, send(&edit, &invalid).Code)
	invalid = edit
	invalid.AuthorizationExcludePatterns = []string{"["}
	require.Equal(t, 400, send(&edit, &invalid).Code)
	invalid = edit
	invalid.Name = "renamed"
	require.Equal(t, 400, send(&edit, &invalid).Code)
	require.Equal(t, 400, send(nil, &edit).Code)
	// Normal settings persistence has no write access to the overrides column.
	require.NoError(t, db.UpdateSystemSettings(t.Context(), settings))
	persisted, err := db.GetSystemSettings(t.Context())
	require.NoError(t, err)
	reloaded := auth.NewStore(nil, nil, persisted)
	t.Cleanup(reloaded.Stop)
	require.Equal(t, []promptfilter.BuiltinPatternOverride{edit}, reloaded.GetPromptFilterConfig().BuiltinOverrides)
	// Exercise the normal settings handler's runtime publication too.
	settingsRequest, _ := gin.CreateTestContext(httptest.NewRecorder())
	settingsRequest.Request = httptest.NewRequest(http.MethodPut, "/settings", bytes.NewBufferString(`{"prompt_filter_threshold":61}`))
	h.UpdateSettings(settingsRequest)
	require.Equal(t, 200, settingsRequest.Writer.Status())
	require.Equal(t, []promptfilter.BuiltinPatternOverride{edit}, store.GetPromptFilterConfig().BuiltinOverrides)
	w = send(&edit, nil)
	require.Equal(t, 200, w.Code, w.Body.String())
	require.Empty(t, store.GetPromptFilterConfig().BuiltinOverrides)
	require.NoError(t, json.Unmarshal(w.Body.Bytes(), &rules))
	for _, rule := range rules.BuiltinPatterns {
		if rule.Name == original.Name {
			require.False(t, rule.Overridden)
			require.False(t, rule.Enabled)
			require.Equal(t, original.Pattern, rule.Pattern)
			require.False(t, rule.SignalOnly)
			require.Empty(t, rule.AllPatterns)
			require.Empty(t, rule.AnyPatterns)
			require.Empty(t, rule.ExcludePatterns)
			require.Empty(t, rule.AuthorizationExcludePatterns)
			require.Zero(t, rule.MinMatches)
		}
	}
}
