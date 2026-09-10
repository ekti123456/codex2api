package proxy

import (
	"context"
	"encoding/json"
	"net/http"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/cache"
	"github.com/stretchr/testify/require"
)

func TestWindowUpgradeKeepsAccountAndExpiryAndRejectsOldTariff(test *testing.T) {
	handler := newWindowAuthorizationHandler(test)
	account := &auth.Account{DBID: 1695, AccessToken: "test", Status: auth.StatusReady, SessionCapacityEnabled: true, SessionCapacityMax: 2, SessionCapacityReserved: 1, SessionCapacityIdleTTLSeconds: 60}
	handler.store.AddAccount(account)
	grant := quoteWindowAuthorization(test, handler, "upgrade-main", "first", "user")
	request := windowAuthorizationRequest(test, handler, grant, "user")
	require.Nil(test, handler.requestWindowGrantError(request))
	key := sessionAffinityKey("newapi-root-session:"+grant.Fingerprint, grant.APIKeyID)
	require.NoError(test, handler.bindWindowGrantOwner(request, account.ID(), key))
	require.NoError(test, handler.confirmRequestWindowGrant(request))
	grant = *windowGrantForRequest(request)
	old := windowAuthorizationRequest(test, handler, grant, "user")
	require.Nil(test, handler.requestWindowGrantError(old))
	require.True(test, handler.store.AdmitAccountSession(account, "other-window", time.Now()))
	input := windowControlRequest{Operation: "upgrade", AllowExpansion: true, ExtraLimit: 2, Multiplier: 1.5, Root: grant.Grant.Root, GrantID: grant.Grant.ID}
	body, err := json.Marshal(input)
	require.NoError(test, err)
	foreign, foreignResponse := windowExpansionTestContext(test, "/v1/session-windows", body, newAPIPolicyMeta{})
	setSignedNewAPIRequestHeaders(test, foreign.Request, body, "foreign-window-upgrade", newAPIIdentity{UserID: "43", ClientIP: "203.0.113.8"}, "test-platform", "integration-secret", promptSessionTestFingerprint(test.Name()))
	addSignedNewAPIPolicyMeta(test, foreign, newAPIPolicyMeta{PlatformID: "test-platform", Profile: "balanced", Mode: "enforce", Provider: "openai", Protocol: "responses", SessionFingerprint: promptSessionTestFingerprint(test.Name())}, true)
	handler.ControlNewAPIUserWindows(foreign)
	require.Equal(test, http.StatusBadRequest, foreignResponse.Code, "another verified user cannot upgrade this grant")
	control, recorder := windowExpansionTestContext(test, "/v1/session-windows", body, newAPIPolicyMeta{})
	handler.ControlNewAPIUserWindows(control)
	require.Equal(test, http.StatusOK, recorder.Code, recorder.Body.String())
	state, err := handler.db.ReadUserWindowAdmissions(context.Background(), cache.PromptSessionLimitSubject(grant.Platform, grant.UserID))
	require.NoError(test, err)
	upgraded := state.Windows[grant.Grant.Root]
	require.True(test, upgraded.Expanded)
	require.Equal(test, 1.5, upgraded.Multiplier)
	require.Equal(test, grant.Grant.ExpiresAt, upgraded.ExpiresAt)
	require.Equal(test, grant.Grant.CreatedAt, upgraded.CreatedAt)
	require.Equal(test, account.ID(), upgraded.OwnerAccountID)
	require.NotEqual(test, grant.Grant.ID, upgraded.ID)
	require.NotNil(test, upgraded.UpgradedAt)
	total, reserved := handler.store.AccountSessionSlotCounts(account.ID(), time.Now())
	require.EqualValues(test, 2, total)
	require.EqualValues(test, 1, reserved)
	require.Equal(test, 1.0, windowGrantForRequest(old).Grant.Multiplier, "already authorized request keeps its snapshot")
	stale := windowAuthorizationRequest(test, handler, grant, "user")
	apiErr := handler.requestWindowGrantError(stale)
	require.NotNil(test, apiErr)
	require.Equal(test, "window_billing_refresh_required", string(apiErr.Code))
	handler.windowTariffs = nil
	apiErr = handler.requestWindowGrantError(windowAuthorizationRequest(test, handler, grant, "user"))
	require.NotNil(test, apiErr, "restart must still reject the old confirmed tariff")
	grant.Grant = *upgraded
	require.Nil(test, handler.requestWindowGrantError(windowAuthorizationRequest(test, handler, grant, "user")))
	control, recorder = windowExpansionTestContext(test, "/v1/session-windows", body, newAPIPolicyMeta{})
	handler.ControlNewAPIUserWindows(control)
	require.Equal(test, http.StatusBadRequest, recorder.Code, "replaying an old confirmation must not allocate again")
}

func TestWindowUpgradeRequiresConsentAndQuotaWithoutOccupyingOnFailure(test *testing.T) {
	handler := newWindowAuthorizationHandler(test)
	account := &auth.Account{DBID: 1695, AccessToken: "test", Status: auth.StatusReady, SessionCapacityEnabled: true, SessionCapacityMax: 3, SessionCapacityReserved: 2, SessionCapacityIdleTTLSeconds: 60}
	handler.store.AddAccount(account)
	grant := quoteWindowAuthorization(test, handler, "upgrade-main", "first", "user")
	request := windowAuthorizationRequest(test, handler, grant, "user")
	require.Nil(test, handler.requestWindowGrantError(request))
	key := sessionAffinityKey("newapi-root-session:"+grant.Fingerprint, grant.APIKeyID)
	require.NoError(test, handler.bindWindowGrantOwner(request, account.ID(), key))
	require.NoError(test, handler.confirmRequestWindowGrant(request))
	grant = *windowGrantForRequest(request)
	require.True(test, handler.store.AdmitAccountSession(account, "other-window", time.Now()))
	paid := quoteWindowAuthorization(test, handler, "paid", "second", "user")
	require.True(test, paid.Grant.Expanded)
	for _, consent := range []bool{false, true} {
		body, err := json.Marshal(windowControlRequest{Operation: "upgrade", AllowExpansion: consent, ExtraLimit: 1, Multiplier: 1.5, Root: grant.Grant.Root, GrantID: grant.Grant.ID})
		require.NoError(test, err)
		control, recorder := windowExpansionTestContext(test, "/v1/session-windows", body, newAPIPolicyMeta{})
		handler.ControlNewAPIUserWindows(control)
		require.Equal(test, http.StatusBadRequest, recorder.Code, recorder.Body.String())
		total, reserved := handler.store.AccountSessionSlotCounts(account.ID(), time.Now())
		require.EqualValues(test, 1, total)
		require.Zero(test, reserved)
	}
	state, err := handler.db.ReadUserWindowAdmissions(context.Background(), cache.PromptSessionLimitSubject(grant.Platform, grant.UserID))
	require.NoError(test, err)
	require.Equal(test, grant.Grant.ID, state.Windows[grant.Grant.Root].ID)
	require.Equal(test, 1.0, state.Windows[grant.Grant.Root].Multiplier)
}
