package proxy

import (
	"bytes"
	"context"
	"fmt"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func TestSessionContinuityMissingSourceKeepsExpiredZeroWindowOwner(test *testing.T) {
	test.Setenv("CODEX_SESSION_AFFINITY_TTL", "20h")
	for _, mode := range []string{"off", "observe", "enforce"} {
		for _, state := range []string{"ready", "disabled", "paused", "quota"} {
			test.Run(mode+"/"+state, func(test *testing.T) {
				handler := newWindowAuthorizationHandler(test)
				config := handler.store.GetPromptFilterConfig()
				config.Advanced.Risk.SessionContinuityMode = mode
				handler.store.SetPromptFilterConfig(config)
				owner := &auth.Account{DBID: 1589, AccessToken: "owner-test", Status: auth.StatusReady, Models: []string{"gpt-5.6-sol"}}
				other := &auth.Account{DBID: 1695, AccessToken: "other-test", Status: auth.StatusReady, Models: []string{"gpt-5.6-sol"}}
				handler.store.AddAccounts([]*auth.Account{owner, other})
				switch state {
				case "disabled":
					atomic.StoreInt32(&owner.Disabled, 1)
				case "paused":
					atomic.StoreInt32(&owner.DispatchPaused, 1)
				case "quota":
					owner.PlanType, owner.UsagePercent7d, owner.UsagePercent7dValid = "free", 100, true
					owner.Reset7dAt = time.Now().Add(time.Hour)
				}
				request, body := continuityTestRequest(0, "turn")
				body = bytes.ReplaceAll(body, []byte(`,"thread_source":"user"`), nil)
				request.Set(contextAPIKeyID, int64(101))
				identity := handler.resolveRequestSessionIdentityForContext(request, body)
				key := capacityAwareSessionAffinityKey(identity, 101)
				require.Empty(test, usageRequestDiagnosticState(request).Resolved.ThreadSource)
				require.Equal(test, "resolved", usageRequestDiagnosticState(request).Resolved.RootState)
				stored := database.SessionContinuityRecord{AccountID: owner.ID(), ThreadID: continuityTestThread, NumberKnown: true, LastSeen: time.Now().Add(-21 * time.Hour)}
				_, err := handler.db.CommitSessionContinuity(context.Background(), hashRiskIdentity(key), stored)
				require.NoError(test, err)
				_, live := handler.store.LiveSessionAccountID(key, time.Now())
				require.False(test, live)
				require.Nil(test, handler.configureSessionModelAffinity(request, identity, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
				trace := selectionTraceForRequest(request)
				require.Equal(test, owner.ID(), trace.PinnedAccount())
				require.Equal(test, "persistent_binding", usageRequestDiagnosticState(request).Continuity.OwnerSource)
				selected, _, _ := handler.store.NextForSessionWithDispatchGuard(key, 101, nil, nil, auth.DispatchPolicyStandard, trace)
				if state == "ready" {
					require.Same(test, owner, selected)
					handler.store.Release(selected)
				} else {
					require.Nil(test, selected)
				}
				require.Nil(test, handler.store.TakePreferredAccountWithDispatch(other.ID(), 101, nil, nil, auth.DispatchPolicyStandard, trace))
				record, found, err := handler.db.ReadSessionContinuity(context.Background(), hashRiskIdentity(key))
				require.NoError(test, err)
				require.True(test, found)
				require.Equal(test, owner.ID(), record.AccountID)
			})
		}
	}
}

func TestSessionContinuityHeaderOnlyCreatesAndRestoresPermanentOwner(test *testing.T) {
	handler := newWindowAuthorizationHandler(test)
	owner := &auth.Account{DBID: 1589, AccessToken: "owner-test", Status: auth.StatusReady, Models: []string{"gpt-5.6-sol"}}
	handler.store.AddAccount(owner)
	body := []byte(`{"model":"gpt-5.6-sol","input":"hello"}`)
	for _, keyID := range []int64{101, 101, 102} {
		request, _ := gin.CreateTestContext(httptest.NewRecorder())
		request.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", bytes.NewReader(body))
		request.Request.Header.Set("Session-Id", continuityTestThread)
		request.Set(contextAPIKeyID, keyID)
		identity := handler.resolveRequestSessionIdentityForContext(request, body)
		beginDispatchSelection(request)
		key := capacityAwareSessionAffinityKey(identity, keyID)
		require.Nil(test, handler.configureSessionModelAffinity(request, identity, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
		diagnostic := usageRequestDiagnosticState(request).Continuity
		require.NotNil(test, diagnostic)
		require.Nil(test, diagnostic.Current)
		_, found, err := handler.db.ReadSessionContinuity(context.Background(), hashRiskIdentity(key))
		require.NoError(test, err)
		if found {
			require.Equal(test, owner.ID(), selectionTraceForRequest(request).PinnedAccount())
			require.Equal(test, "persistent_binding", diagnostic.OwnerSource)
		} else {
			require.Zero(test, selectionTraceForRequest(request).PinnedAccount())
		}
		if keyID == 101 {
			require.Nil(test, handler.commitSessionContinuity(request, owner))
			handler.continuityRecords = nil
			handler.store.UnbindSessionAffinity(key, owner.ID())
		}
	}
}

func TestSessionContinuityMissingSourceDoesNotPromoteBackgroundOrUnknownRoots(test *testing.T) {
	for _, scenario := range []struct {
		name      string
		source    string
		rootState string
		subagent  string
		identity  requestSessionIdentity
	}{
		{name: "background", source: "agent_created_thread", rootState: "resolved", identity: requestSessionIdentity{stableIdentity: true, requiresRootAccount: true}},
		{name: "related", rootState: "resolved", identity: requestSessionIdentity{stableIdentity: true, relatedToRoot: true}},
		{name: "subagent", rootState: "resolved", subagent: "guardian", identity: requestSessionIdentity{stableIdentity: true}},
		{name: "unresolved", rootState: "unavailable", identity: requestSessionIdentity{stableIdentity: true}},
		{name: "conflicting", rootState: "conflict", identity: requestSessionIdentity{stableIdentity: true}},
		{name: "content_derived", rootState: "unavailable"},
		{name: "bypass", rootState: "resolved", identity: requestSessionIdentity{stableIdentity: true, bypassWindowAccounting: true}},
	} {
		test.Run(scenario.name, func(test *testing.T) {
			handler := newWindowAuthorizationHandler(test)
			request, body := continuityTestRequest(0, "turn")
			usageRequestDiagnosticState(request).Resolved = &usageRequestResolution{ThreadSource: scenario.source, RootState: scenario.rootState, SubagentKind: scenario.subagent}
			require.Nil(test, handler.prepareSessionContinuity(request, scenario.identity, "root::api-key:101", body))
			require.Nil(test, continuityRequest(request))
			require.Zero(test, selectionTraceForRequest(request).PinnedAccount())
		})
	}
}

func TestSessionContinuityMissingSourceUsesCurrentWebSocketFrame(test *testing.T) {
	handler := newWindowAuthorizationHandler(test)
	request, body := continuityTestRequest(72, "turn")
	body = bytes.ReplaceAll(body, []byte(`,"thread_source":"user"`), nil)
	request.Request.Header.Set("Connection", "Upgrade")
	request.Request.Header.Set("Upgrade", "websocket")
	request.Request.Header.Set("Session-Id", continuityTestThread)
	request.Request.Header.Set("X-Codex-Window-Id", continuityTestThread+":0")
	request.Set(contextAPIKeyID, int64(101))
	identity := handler.resolveRequestSessionIdentityForContext(request, body)
	key := capacityAwareSessionAffinityKey(identity, 101)
	_, err := handler.db.CommitSessionContinuity(context.Background(), hashRiskIdentity(key), database.SessionContinuityRecord{AccountID: 1589, ThreadID: continuityTestThread, Number: 71, NumberKnown: true, LastSeen: time.Now().Add(-21 * time.Hour)})
	require.NoError(test, err)
	require.Nil(test, handler.prepareSessionContinuity(request, identity, key, body))
	diagnostic := usageRequestDiagnosticState(request).Continuity
	require.NotNil(test, diagnostic)
	require.Equal(test, uint64(72), *diagnostic.Current)
	require.Equal(test, "window_advanced", diagnostic.Result)
	require.Equal(test, int64(1589), selectionTraceForRequest(request).PinnedAccount())
}

func TestSessionContinuitySignedSourceDisappearsWithoutChangingOwner(test *testing.T) {
	handler := newRootlessPassiveModelTestHandler(test)
	owner := &auth.Account{DBID: 1589, AccessToken: "owner-test", Status: auth.StatusReady, Models: []string{"gpt-5.6-sol"}}
	handler.store.AddAccount(owner)
	fingerprint := newAPIRootSessionFingerprint("test-platform", "42", continuityTestThread)
	key := sessionAffinityKey("newapi-root-session:"+fingerprint, 101)
	for index, source := range []string{"user", "", "user"} {
		test.Run(fmt.Sprintf("request_%d", index), func(test *testing.T) {
			_, body := continuityTestRequest(0, "turn")
			if source == "" {
				body = bytes.ReplaceAll(body, []byte(`,"thread_source":"user"`), nil)
			}
			meta := newAPIPolicyMeta{RootSessionVersion: 1, RootSessionState: newAPIPolicyRootSessionResolved, RootSessionRelation: newAPIPolicyRootSessionRelationRoot, RootSessionFingerprint: fingerprint, ThreadSource: source, RequestKind: "turn"}
			request, _ := signedRootlessPassiveModelContext(test, http.MethodPost, "/v1/responses", body, meta)
			handler.primeNewAPIPolicyContext(request, body)
			status, policy := handler.cachedNewAPIPolicyAuditState(request)
			require.Equal(test, "verified", status)
			require.True(test, policy.MetaVerified)
			identity := handler.resolveRequestSessionIdentityForContext(request, body)
			beginDispatchSelection(request)
			require.Equal(test, key, capacityAwareSessionAffinityKey(identity, 101))
			require.Equal(test, source, usageRequestDiagnosticState(request).Resolved.ThreadSource)
			require.False(test, passiveInternalRequestAuthorized(request))
			require.Nil(test, handler.configureSessionModelAffinity(request, identity, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
			if index > 0 {
				require.Equal(test, "persistent_binding", usageRequestDiagnosticState(request).Continuity.OwnerSource)
				require.Equal(test, owner.ID(), selectionTraceForRequest(request).PinnedAccount())
			}
			require.Nil(test, handler.commitSessionContinuity(request, owner))
			handler.store.UnbindSessionAffinity(key, owner.ID())
		})
	}
}

func TestSessionContinuityHeaderOnlyUnavailableOwnerCannotReachAnotherAccount(test *testing.T) {
	for _, state := range []string{"disabled", "paused", "usage_limit_reached"} {
		test.Run(state, func(test *testing.T) {
			var callsOwner, callsOther atomic.Int32
			var exhausted atomic.Bool
			handler, _, owner, _, key, cleanup := newStickyFailureHarness(test, 1, 1,
				http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					callsOwner.Add(1)
					if exhausted.Load() {
						writer.Header().Set("Content-Type", "application/json")
						writer.WriteHeader(http.StatusTooManyRequests)
						_, _ = fmt.Fprint(writer, `{"error":{"code":"usage_limit_reached","message":"quota exhausted"}}`)
						return
					}
					stickyFailureSuccess(writer)
				}),
				http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
					callsOther.Add(1)
					stickyFailureSuccess(writer)
				}),
			)
			defer cleanup()
			require.Equal(test, http.StatusOK, runStickyFailureRequest(test, handler).Code)
			switch state {
			case "disabled":
				atomic.StoreInt32(&owner.Disabled, 1)
			case "paused":
				atomic.StoreInt32(&owner.DispatchPaused, 1)
			case "usage_limit_reached":
				exhausted.Store(true)
			}
			for range 2 {
				response := runStickyFailureRequest(test, handler)
				require.NotEqual(test, http.StatusOK, response.Code, response.Body.String())
				require.Zero(test, callsOther.Load())
				entry, found, err := handler.readSessionContinuity(context.Background(), hashRiskIdentity(key))
				require.NoError(test, err)
				require.True(test, found)
				require.Equal(test, owner.ID(), entry.Record.AccountID)
			}
			if state != "usage_limit_reached" {
				require.Equal(test, int32(1), callsOwner.Load())
			}
		})
	}
}
