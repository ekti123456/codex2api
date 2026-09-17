package proxy

import (
	"context"
	"encoding/json"
	"fmt"
	"sync"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/codex2api/security/promptfilter"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestSessionWindowAdjustmentAdmissionAndControlAgree(t *testing.T) {
	for _, tc := range []struct {
		name, mode              string
		delta, want, minSamples int
		override                string
		enabled                 bool
	}{
		{name: "reduce", mode: "enforce", delta: -1, want: 2, enabled: true},
		{name: "increase", mode: "enforce", delta: 1, want: 4, enabled: true},
		{name: "unchanged", mode: "enforce", want: 3, enabled: true},
		{name: "minimum", mode: "enforce", delta: -20, want: 1, enabled: true},
		{name: "observe", mode: "observe", delta: -1, want: 3, enabled: true},
		{name: "off", mode: "off", delta: -1, want: 3, enabled: true},
		{name: "samples", mode: "enforce", delta: -1, want: 3, minSamples: 20, enabled: true},
		{name: "custom", mode: "enforce", delta: -1, want: 5, override: "custom", enabled: true},
		{name: "unlimited", mode: "enforce", delta: -1, override: "off", enabled: true},
		{name: "quantity_disabled", mode: "enforce", delta: -1},
	} {
		t.Run(tc.name, func(t *testing.T) {
			h, cfg := newSessionCooldownFixture(t)
			cfg.Advanced.Risk.SessionCreationLimitEnabled = tc.enabled
			cfg.Advanced.Risk.SessionCreationLimit = 3
			cfg.Advanced.Risk.SessionCreationCooldown.Mode = tc.mode
			cfg.Advanced.Risk.SessionCreationCooldown.Tiers = []promptfilter.SessionCreationCooldownTier{{WindowLimitDelta: tc.delta}}
			if tc.minSamples > 0 {
				cfg.Advanced.Risk.SessionCreationCooldown.MinSamples = tc.minSamples
			}
			h.store.SetPromptFilterConfig(cfg)
			if tc.override != "" {
				h.store.ApplyPromptSessionLimitOverride(database.PromptSessionLimitOverride{Platform: "newapi", NewAPIUserID: "42", Mode: tc.override, Limit: 6, WindowSeconds: 3600})
			}
			control := promptSessionLimitVerifiedUserContext("control")
			_, identity := h.cachedNewAPIPolicyAuditState(control)
			limit, _ := h.userWindowControlLimits(control, identity)
			require.Equal(t, tc.want, limit)
			if tc.want == 0 {
				request := promptSessionLimitVerifiedUserContext("exempt")
				status, blocked := h.checkPromptSessionCreationLimit(request, cfg, nil)
				completeSessionCooldownFixture(h, request, true)
				require.False(t, blocked)
				require.False(t, status.Enabled)
				return
			}
			for i := 0; i < tc.want; i++ {
				request := promptSessionLimitVerifiedUserContext(fmt.Sprint("root-", i))
				status, blocked := h.checkPromptSessionCreationLimit(request, cfg, nil)
				require.False(t, blocked)
				require.Equal(t, tc.want, status.Limit)
				completeSessionCooldownFixture(h, request, true)
			}
			status, blocked := h.checkPromptSessionCreationLimit(promptSessionLimitVerifiedUserContext("overflow"), cfg, nil)
			require.True(t, blocked)
			require.False(t, status.Cooldown)
			require.Equal(t, tc.want, status.Used)
			// Reductions never evict the existing window.
			request := promptSessionLimitVerifiedUserContext("root-0")
			_, blocked = h.checkPromptSessionCreationLimit(request, cfg, nil)
			require.False(t, blocked)
			completeSessionCooldownFixture(h, request, true)
		})
	}
}

func TestSessionWindowAdjustmentSignedQuotesAndList(t *testing.T) {
	h := newWindowAuthorizationHandler(t)
	cfg := h.store.GetPromptFilterConfig()
	cfg.Advanced.Risk.SessionCreationLimit = 3
	cfg.Advanced.Risk.SessionCreationCooldown.Mode = "enforce"
	cfg.Advanced.Risk.SessionCreationCooldown.Tiers = []promptfilter.SessionCreationCooldownTier{{WindowLimitDelta: -1}}
	h.store.SetPromptFilterConfig(cfg)
	accountID, err := h.db.InsertOpenAIResponsesAccount(t.Context(), "samples", map[string]any{"base_url": "https://example.test", "api_key": "test"}, "")
	require.NoError(t, err)
	h.store.AddAccount(&auth.Account{DBID: accountID, SessionCapacityEnabled: true, SessionCapacityMax: 100, SessionCapacityIdleTTLSeconds: 60})
	for i := 0; i < 10; i++ {
		start := time.Now().UTC().Add(-time.Duration(120+i) * time.Minute)
		require.NoError(t, h.db.InsertUsageLog(t.Context(), &database.UsageLogInput{AccountID: accountID, NewAPIPlatform: "test-platform", NewAPIUserID: "42", SessionHash: fmt.Sprint("history-", i), SessionUsagePeriodID: fmt.Sprint("period-", i), SessionUsageStartedAt: start, ObservedAt: start.Add(5 * time.Minute), SessionUsageIdleSeconds: 60, StatusCode: 200}))
	}
	h.db.FlushUsageLogs()
	for i := 0; i < 3; i++ {
		grant := quoteWindowAuthorization(t, h, fmt.Sprint("quote-", i), fmt.Sprint("reservation-", i), "user")
		require.Equal(t, i == 2, grant.Grant.Expanded, "two ordinary windows, then existing expansion policy")
		request := windowAuthorizationRequest(t, h, grant, "user")
		require.NoError(t, h.validateRequestWindowGrant(request))
		status, blocked := h.checkPromptSessionCreationLimit(request, cfg, []byte(`{"model":"gpt-5.6-sol","input":"test"}`))
		require.False(t, blocked, "%+v", status)
		require.Equal(t, 2, status.Limit)
		completeSessionCooldownFixture(h, request, true)
	}
	body, _ := json.Marshal(windowControlRequest{Operation: "list", Multiplier: 1})
	request, response := windowExpansionTestContext(t, "/v1/session-windows", body, newAPIPolicyMeta{})
	h.ControlNewAPIUserWindows(request)
	require.Equal(t, 200, response.Code, response.Body.String())
	require.EqualValues(t, 2, gjson.GetBytes(response.Body.Bytes(), "limit").Int())
	require.EqualValues(t, 3, gjson.GetBytes(response.Body.Bytes(), "used").Int())
}

func TestSessionWindowAdjustmentConcurrentRequestsAndSampleReuse(t *testing.T) {
	h, cfg := newSessionCooldownFixture(t)
	cfg.Advanced.Risk.SessionCreationLimitEnabled = true
	cfg.Advanced.Risk.SessionCreationLimit = 3
	cfg.Advanced.Risk.SessionCreationCooldown.Tiers = []promptfilter.SessionCreationCooldownTier{{WindowLimitDelta: -1}}
	var workers sync.WaitGroup
	accepted := make(chan *gin.Context, 12)
	for i := 0; i < 12; i++ {
		workers.Go(func() {
			request := promptSessionLimitVerifiedUserContext(fmt.Sprint("concurrent-", i))
			if _, blocked := h.checkPromptSessionCreationLimit(request, cfg, nil); !blocked {
				accepted <- request
			}
		})
	}
	workers.Wait()
	close(accepted)
	require.Len(t, accepted, 2)
	for request := range accepted {
		value, found := request.Get(sessionCooldownAverageKey)
		require.True(t, found)
		snapshot := value.(sessionCooldownAverageSnapshot)
		require.Equal(t, 10, snapshot.Samples)
		require.Equal(t, 300.0, snapshot.Average)
		completeSessionCooldownFixture(h, request, true)
		value, _ = request.Get(sessionCooldownAverageKey)
		require.Nil(t, value, "next WS frame must not inherit the sample snapshot")
	}
	// Database failures and unverified identities leave the configured base intact.
	request := promptSessionLimitVerifiedUserContext("unavailable")
	_, identity := h.cachedNewAPIPolicyAuditState(request)
	identity.MetaVerified = false
	require.Equal(t, 3, h.adjustedUserWindowLimit(request, identity, cfg.Advanced.Risk.SessionCreationCooldown, 3, time.Now()))
	identity.MetaVerified = true
	ctx, cancel := context.WithCancel(request.Request.Context())
	cancel()
	request.Request = request.Request.WithContext(ctx)
	require.Equal(t, 3, h.adjustedUserWindowLimit(request, identity, cfg.Advanced.Risk.SessionCreationCooldown, 3, time.Now()))
}
