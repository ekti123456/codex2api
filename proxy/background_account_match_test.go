package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync"
	"sync/atomic"
	"testing"
	"time"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

type backgroundMatchRaceStore struct {
	CodexIdentityStore
	once        sync.Once
	switchOwner func()
}

func (store *backgroundMatchRaceStore) ClaimCodexIdentities(ctx context.Context, keys []string, owner string) error {
	store.once.Do(store.switchOwner)
	return store.CodexIdentityStore.ClaimCodexIdentities(ctx, keys, owner)
}

func TestBackgroundAccountMatchRechecksAfterIdentityPreparation(test *testing.T) {
	for _, endpoint := range []string{"http", "compact"} {
		test.Run(endpoint, func(test *testing.T) {
			handler, owner, target, key := failoverTestSetup(test, true)
			previousResin := GetResinConfig()
			test.Cleanup(func() { SetResinConfig(previousResin) })
			var sent atomic.Int32
			server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
				sent.Add(1)
				_, _ = writer.Write([]byte(`{}`))
			}))
			test.Cleanup(server.Close)
			SetResinConfig(&ResinConfig{BaseURL: server.URL, PlatformName: "background-race"})
			request, body := failoverTestRequest(test, handler)
			require.Nil(test, handler.prepareBackgroundAccountMatch(request, key, body))
			switched := false
			store := &backgroundMatchRaceStore{CodexIdentityStore: handler.db, switchOwner: func() {
				_, _, err := handler.db.SwitchSessionContinuityAccount(context.Background(), database.SessionAccountFailover{
					RootKey: hashRiskIdentity(key), ExpectedAccountID: owner.ID(), AccountID: target.ID(), Reason: "account_disabled", At: time.Now(),
				})
				require.NoError(test, err)
				switched = true
			}}
			ctx := WithCodexIdentityStore(request.Request.Context(), store)
			var response *http.Response
			var err error
			if endpoint == "compact" {
				response, err = ExecuteCompactRequest(ctx, owner, body, "cache", "", "test-user-key", nil, request.Request.Header)
			} else {
				response, err = ExecuteRequest(ctx, owner, body, "cache", "", "test-user-key", nil, request.Request.Header, false)
			}
			require.True(test, switched)
			require.Error(test, err)
			require.Nil(test, response)
			require.Zero(test, sent.Load())
			require.Equal(test, "owner_changed", usageRequestDiagnosticState(request).BackgroundAccountMatch.Result)
			require.False(test, isRetryableRequestErrorForContext(ctx, err, database.ContinuousRetryPolicy{}))
		})
	}
}

func TestBackgroundAccountMatchRejectsStaleOwnerBeforeSend(test *testing.T) {
	handler, owner, target, key := failoverTestSetup(test, true)
	previousResin := GetResinConfig()
	test.Cleanup(func() { SetResinConfig(previousResin) })
	var requests atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		requests.Add(1)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"id":"response-test"}`))
	}))
	test.Cleanup(server.Close)
	SetResinConfig(&ResinConfig{BaseURL: server.URL, PlatformName: "background-match"})
	request, body := failoverTestRequest(test, handler)
	identity := requestSessionIdentity{stableIdentity: true, relatedToRoot: true, requiresRootAccount: true, affinityID: "failover-root"}
	relatedKey := auth.ProtectedRelatedSessionAffinityKey(key)
	require.Nil(test, handler.configureSessionModelAffinity(request, identity, relatedKey, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
	match := backgroundAccountMatchFromContext(request.Request.Context())
	require.NotNil(test, match)
	require.Equal(test, owner.ID(), match.accountID)
	require.NotEmpty(test, match.diagnostic.ScopeHash)
	require.Equal(test, continuityTestThread[:8], match.diagnostic.SessionIDPrefix)

	_, _, err := handler.db.SwitchSessionContinuityAccount(context.Background(), database.SessionAccountFailover{
		RootKey: hashRiskIdentity(key), ExpectedAccountID: owner.ID(), AccountID: target.ID(), Reason: "account_disabled", At: time.Now(),
	})
	require.NoError(test, err)
	handler.store.UnbindSessionAffinity(key, owner.ID())
	handler.store.BindSessionAffinity(key, target, "")
	for _, compact := range []bool{false, true} {
		var response *http.Response
		if compact {
			response, err = ExecuteCompactRequest(request.Request.Context(), owner, body, "cache", "", "test-key", nil, request.Request.Header)
		} else {
			response, err = ExecuteRequest(request.Request.Context(), owner, body, "cache", "", "test-key", nil, request.Request.Header, false)
		}
		require.Error(test, err)
		require.Nil(test, response)
		require.Equal(test, "owner_changed", match.diagnostic.Result)
		require.NotNil(test, codexIdentityRequestError(err))
	}
	require.Zero(test, requests.Load())

	fresh, raw := failoverTestRequest(test, handler)
	require.Nil(test, handler.configureSessionModelAffinity(fresh, identity, relatedKey, "gpt-5.6-sol", "gpt-5.6-sol", false, raw))
	filter := handler.applyPassiveInternalModelRouting(fresh, "gpt-5.6-sol", identity, relatedKey, false, nil)
	require.False(test, filter(owner))
	require.True(test, filter(target))
	response, err := ExecuteRequest(fresh.Request.Context(), target, raw, "cache", "", "test-key", nil, fresh.Request.Header, false)
	require.NoError(test, err)
	_, err = io.Copy(io.Discard, response.Body)
	require.NoError(test, err)
	require.NoError(test, response.Body.Close())
	require.EqualValues(test, 1, requests.Load())
	require.Equal(test, target.ID(), usageRequestDiagnosticState(fresh).BackgroundAccountMatch.AccountID)
	require.EqualValues(test, 1, usageRequestDiagnosticState(fresh).BackgroundAccountMatch.Generation)
}

func TestBackgroundAccountMatchGenerationAndLiveWindow(test *testing.T) {
	for _, scenario := range []string{"wrong_account", "aba", "expired_root", "storage_missing", "storage_error"} {
		test.Run(scenario, func(test *testing.T) {
			handler, owner, target, key := failoverTestSetup(test, true)
			request, body := failoverTestRequest(test, handler)
			require.Nil(test, handler.prepareBackgroundAccountMatch(request, key, body))
			selected := owner
			switch scenario {
			case "wrong_account":
				selected = target
			case "aba":
				for generation, account := range []*auth.Account{target, owner} {
					expected := owner.ID()
					if generation > 0 {
						expected = target.ID()
					}
					_, _, err := handler.db.SwitchSessionContinuityAccount(context.Background(), database.SessionAccountFailover{
						RootKey: hashRiskIdentity(key), ExpectedAccountID: expected, AccountID: account.ID(),
						ExpectedGeneration: uint64(generation), At: time.Now(), Reason: "account_disabled",
					})
					require.NoError(test, err)
				}
			case "expired_root":
				handler.store.UnbindSessionAffinity(key, owner.ID())
			case "storage_missing":
				handler.db = newWindowAuthorizationHandler(test).db
			case "storage_error":
				canceled, cancel := context.WithCancel(request.Request.Context())
				cancel()
				request.Request = request.Request.WithContext(canceled)
			}
			require.Error(test, ValidateBackgroundAccountMatch(request.Request.Context(), selected))
		})
	}
}

func TestBackgroundAccountMatchFiltersRelatedWithoutModelBypass(test *testing.T) {
	handler, owner, target, key := failoverTestSetup(test, false)
	request, body := failoverTestRequest(test, handler)
	identity := requestSessionIdentity{stableIdentity: true, relatedToRoot: true, affinityID: "failover-root"}
	relatedKey := auth.RelatedSessionAffinityKey(key)
	require.Nil(test, handler.configureSessionModelAffinity(request, identity, relatedKey, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
	filter := handler.applyPassiveInternalModelRouting(request, "gpt-5.6-sol", identity, relatedKey, false, func(*auth.Account) bool { return true })
	require.True(test, filter(owner))
	require.False(test, filter(target))
	require.Nil(test, handler.configureSessionModelAffinity(request, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
	require.Nil(test, backgroundAccountMatchFromContext(request.Request.Context()))
	require.Nil(test, usageRequestDiagnosticState(request).BackgroundAccountMatch)
}

func TestBackgroundAccountMatchSignedRootlessFollowsCurrentOwner(test *testing.T) {
	handler, owner, target, _ := failoverTestSetup(test, true)
	_, body := accountIdentityFixture(test, false, true)
	body, err := sjson.SetBytes(body, "input", "independent background context")
	require.NoError(test, err)
	body, err = sjson.SetBytes(body, "client_metadata.x-codex-turn-metadata.thread_source", "agent_created_thread")
	require.NoError(test, err)
	fingerprint := promptSessionTestFingerprint("prefix-resolved-main")
	meta := newAPIPolicyMeta{
		RootSessionVersion: 1, RootSessionState: newAPIPolicyRootSessionResolved,
		RootSessionRelation: newAPIPolicyRootSessionRelationRelated, RootSessionFingerprint: fingerprint,
		ThreadSource: "agent_created_thread", RequestKind: "turn", RootAssociation: "scope_prefix_unique", PassiveFeature: newAPIPassiveFeatureRelatedInternal,
	}
	key := sessionAffinityKey("newapi-root-session:"+fingerprint, 101)
	_, err = handler.db.CommitSessionContinuity(context.Background(), hashRiskIdentity(key), database.SessionContinuityRecord{
		AccountID: owner.ID(), ThreadID: accountIdentitySampleRoot, NumberKnown: true, LastSeen: time.Now(),
	})
	require.NoError(test, err)
	handler.store.BindSessionAffinity(key, owner, "")
	var originalScope string
	for generation, account := range []*auth.Account{owner, target} {
		if generation > 0 {
			_, _, err = handler.db.SwitchSessionContinuityAccount(context.Background(), database.SessionAccountFailover{
				RootKey: hashRiskIdentity(key), ExpectedAccountID: owner.ID(), AccountID: target.ID(), At: time.Now(), Reason: "account_disabled",
			})
			require.NoError(test, err)
			handler.store.UnbindSessionAffinity(key, owner.ID())
			handler.store.BindSessionAffinity(key, target, "")
		}
		test.Run([]string{"before", "after"}[generation], func(test *testing.T) {
			request, _ := signedRootlessPassiveModelContext(test, http.MethodPost, "/v1/responses", body, meta)
			handler.primeNewAPIPolicyContext(request, body)
			handler.bindCodexIdentityClaims(request)
			identity := handler.resolveRequestSessionIdentityForContext(request, body)
			status, _ := handler.cachedNewAPIPolicyAuditState(request)
			require.Equal(test, "verified", status)
			require.True(test, identity.relatedToRoot)
			require.True(test, identity.requiresRootAccount)
			require.False(test, gjson.GetBytes(body, "client_metadata.x-codex-turn-metadata.parent_thread_id").Exists())
			beginDispatchSelection(request)
			relatedKey := capacityAwareSessionAffinityKey(identity, 101)
			require.Nil(test, handler.configureSessionModelAffinity(request, identity, relatedKey, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
			match := usageRequestDiagnosticState(request).BackgroundAccountMatch
			require.NotNil(test, match)
			require.Equal(test, account.ID(), match.AccountID)
			require.EqualValues(test, generation, match.Generation)
			require.Equal(test, accountIdentitySampleRoot[:8], match.SessionIDPrefix)
			require.NotEmpty(test, match.ScopeHash)
			if generation == 0 {
				originalScope = match.ScopeHash
			} else {
				require.Equal(test, originalScope, match.ScopeHash)
			}
			filter := handler.applyPassiveInternalModelRouting(request, "gpt-5.6-sol", identity, relatedKey, false, nil)
			require.True(test, filter(account))
			if generation > 0 {
				require.False(test, filter(owner))
			}
		})
	}
}
