package proxy

import (
	"net/http"
	"net/http/httptest"
	"strings"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAPIRelayOnlyWindowControlNeedsNoNewAPIIdentity(t *testing.T) {
	for _, operation := range []string{"quote", "quote_tiered", "list"} {
		t.Run(operation, func(t *testing.T) {
			handler := newWindowAuthorizationHandler(t)
			handler.store.AddAccount(apiRelayPolicyTestAccount())
			// This row belongs to another user; an unsigned caller must never see or change it.
			subject := cache.PromptSessionLimitSubject("test-platform", "42")
			require.NoError(t, handler.db.UpdateUserWindowAdmissions(t.Context(), subject, func(state *database.UserWindowAdmissionState) error {
				state.Windows["private-root"] = &database.UserWindowGrant{ID: "private-grant", Root: "private-root", Confirmed: true, Multiplier: 1.5, Expanded: true, ExpiresAt: time.Now().Add(time.Hour)}
				return nil
			}))
			recorder := httptest.NewRecorder()
			request, _ := gin.CreateTestContext(recorder)
			request.Request = httptest.NewRequest(http.MethodPost, "/v1/session-windows", strings.NewReader(`{"operation":"`+operation+`","allow_expansion":true,"extra_limit":5,"multiplier":1.5,"multiplier_step":0.1}`))
			request.Request.Header.Set("X-NewAPI-User-ID", "42")
			request.Set(contextAPIKeyID, int64(9))
			request.Set(contextAPIKeyRow, &database.APIKeyRow{ID: 9, Name: "API"})
			handler.ControlNewAPIUserWindows(request)
			require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			require.Equal(t, "ordinary_only", gjson.Get(recorder.Body.String(), "reason").String())
			require.True(t, gjson.Get(recorder.Body.String(), "api_relay_session_exempt").Bool())
			require.Empty(t, gjson.Get(recorder.Body.String(), "ticket").String())
			require.Empty(t, gjson.Get(recorder.Body.String(), "windows").Array())
			require.NotContains(t, recorder.Body.String(), "private-")
			state, err := handler.db.ReadUserWindowAdmissions(t.Context(), subject)
			require.NoError(t, err)
			require.Len(t, state.Windows, 1)
			require.Equal(t, "private-grant", state.Windows["private-root"].ID)
		})
	}
}

func TestWindowControlAPIRelayExemptionUsesKeyPermissions(t *testing.T) {
	for _, scenario := range []string{"mixed", "disabled_codex", "api_group", "codex_group", "denied_api", "no_accounts", "no_authenticated_key", "api_upgrade", "api_release"} {
		t.Run(scenario, func(t *testing.T) {
			handler := newWindowAuthorizationHandler(t)
			relay := apiRelayPolicyTestAccount()
			codex := &auth.Account{DBID: 1695, AccessToken: "codex", Status: auth.StatusReady, GroupIDs: []int64{1}}
			if scenario == "denied_api" {
				relay.SetAllowedAPIKeyIDs([]int64{10})
			}
			if scenario == "disabled_codex" {
				codex.Disabled = 1
			}
			if scenario != "no_accounts" {
				handler.store.AddAccount(relay)
			}
			if scenario == "mixed" || scenario == "disabled_codex" || scenario == "api_group" || scenario == "codex_group" {
				handler.store.AddAccount(codex)
			}
			operation := "quote_tiered"
			if scenario == "api_upgrade" {
				operation = "upgrade_tiered"
			}
			if scenario == "api_release" {
				operation = "release"
			}
			recorder := httptest.NewRecorder()
			request, _ := gin.CreateTestContext(recorder)
			request.Request = httptest.NewRequest(http.MethodPost, "/v1/session-windows", strings.NewReader(`{"operation":"`+operation+`","multiplier":1.5,"multiplier_step":0.1}`))
			request.Request.Header.Set("X-API-Session-Exempt", "true")
			if scenario != "no_authenticated_key" {
				request.Set(contextAPIKeyID, int64(9))
				row := &database.APIKeyRow{ID: 9, Name: "API"}
				if scenario == "api_group" {
					row.AllowedGroupIDs = []int64{2}
				}
				if scenario == "codex_group" {
					row.AllowedGroupIDs = []int64{1}
				}
				request.Set(contextAPIKeyRow, row)
				handler.store.SetAPIKeyAllowedGroups(9, row.AllowedGroupIDs)
			}
			handler.ControlNewAPIUserWindows(request)
			if scenario == "api_group" {
				require.Equal(t, http.StatusOK, recorder.Code, recorder.Body.String())
			} else {
				require.Equal(t, http.StatusUnauthorized, recorder.Code, recorder.Body.String())
				require.Equal(t, "window_identity_required", gjson.Get(recorder.Body.String(), "code").String())
			}
		})
	}
}

func TestAPIRelayWindowControlRetainsBearerAuthentication(t *testing.T) {
	handler := newWindowAuthorizationHandler(t)
	handler.store.AddAccount(apiRelayPolicyTestAccount())
	config := handler.store.GetPromptFilterConfig()
	config.Advanced.NewAPI.Enabled = false
	handler.store.SetPromptFilterConfig(config)
	const key = "sk-api-relay-window-test"
	_, err := handler.db.InsertAPIKey(t.Context(), "API", key)
	require.NoError(t, err)
	// Initialize the same key cache used by a real /v1 route.
	handler = NewHandler(handler.store, handler.db, nil, nil)
	router := gin.New()
	router.POST("/v1/session-windows", handler.APIKeyAuthMiddleware(), handler.ControlNewAPIUserWindows)
	for _, token := range []string{"invalid-key", key} {
		request := httptest.NewRequest(http.MethodPost, "/v1/session-windows", strings.NewReader(`{"operation":"quote_tiered","allow_expansion":true,"multiplier":1.5,"multiplier_step":0.1}`))
		request.Header.Set("Authorization", "Bearer "+token)
		request.Header.Set("Content-Type", "application/json")
		response := httptest.NewRecorder()
		router.ServeHTTP(response, request)
		if token != key {
			require.Equal(t, http.StatusUnauthorized, response.Code)
		} else {
			require.Equal(t, http.StatusOK, response.Code, response.Body.String())
			require.Equal(t, "ordinary_only", gjson.Get(response.Body.String(), "reason").String())
		}
	}
	handler.store.AddAccount(&auth.Account{DBID: 1695, AccessToken: "codex", Status: auth.StatusReady})
	request := httptest.NewRequest(http.MethodPost, "/v1/session-windows", strings.NewReader(`{"operation":"quote","multiplier":1}`))
	request.Header.Set("Authorization", "Bearer "+key)
	response := httptest.NewRecorder()
	router.ServeHTTP(response, request)
	require.Equal(t, http.StatusUnauthorized, response.Code, "changing the authorized pool must not retain the API-only exemption")
	require.Equal(t, "window_identity_required", gjson.Get(response.Body.String(), "code").String())
}
