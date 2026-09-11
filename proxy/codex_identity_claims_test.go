package proxy

import (
	"context"
	"net/http"
	"net/http/httptest"
	"strconv"
	"strings"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
)

func codexOwnerTestContext(test *testing.T, handler *Handler, user string, verified bool) *gin.Context {
	test.Helper()
	ctx, _ := gin.CreateTestContext(httptest.NewRecorder())
	ctx.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	ctx.Request.Header.Set("Authorization", "Bearer shared-key")
	ctx.Request.Header.Set("X-NewAPI-User-Id", user)
	policy := verifiedNewAPIPolicyContext{APIKeyID: 1, Platform: "platform", MetaVerified: true, Identity: newAPIIdentity{UserID: user}}
	bindTransportOwner(ctx, policy, verified)
	handler.bindCodexIdentityClaims(ctx)
	return ctx
}

func TestCodexIdentityClaimsUserAccountAndLineageIsolation(test *testing.T) {
	test.Setenv("CODEX_OUTBOUND_SESSION_MODE", "preserve")
	handler := &Handler{}
	first := codexOwnerTestContext(test, handler, "first", true)
	second := codexOwnerTestContext(test, handler, "second", true)
	account := &auth.Account{DBID: 101, AccountID: "upstream-owner"}
	headers, body := fingerprintMetadataFixture(test, "session", "thread", "parent", "fork", 1)
	fingerprint := NewCodexTransportFingerprint(account, headers, body, "cache")
	require.NoError(test, fingerprint.ClaimSessionIdentity(first.Request.Context(), account, "shared-key"))
	require.NoError(test, fingerprint.ClaimSessionIdentity(first.Request.Context(), account, "new-token-same-user"))
	err := fingerprint.ClaimSessionIdentity(second.Request.Context(), account, "shared-key")
	require.Equal(test, "codex_session_identity_conflict", string(codexIdentityRequestError(err).Code))
	require.False(test, isRetryableRequestErrorForContext(second.Request.Context(), err, database.ContinuousRetryPolicy{Enabled: true}))
	require.Equal(test, http.StatusBadRequest, StatusCodeFromError(err))
	duplicate := &auth.Account{DBID: 102, AccountID: "upstream-owner"}
	require.Error(test, fingerprint.ClaimSessionIdentity(second.Request.Context(), duplicate, "shared-key"))
	otherAccount := &auth.Account{DBID: 103, AccountID: "different-upstream-owner"}
	require.NoError(test, fingerprint.ClaimSessionIdentity(second.Request.Context(), otherAccount, "shared-key"))
	childHeaders, childBody := fingerprintMetadataFixture(test, "child-session", "child-thread", "thread", "", 0)
	child := NewCodexTransportFingerprint(account, childHeaders, childBody, "other-cache")
	require.Error(test, child.ClaimSessionIdentity(second.Request.Context(), account, "shared-key"))
	require.NoError(test, child.ClaimSessionIdentity(first.Request.Context(), account, "shared-key"))
	flatChild := NewCodexTransportFingerprint(account, nil, []byte(`{"client_metadata":{"session_id":"new-session","thread_id":"new-thread","parent_thread_id":"thread"}}`), "cache")
	require.Error(test, flatChild.ClaimSessionIdentity(second.Request.Context(), account, "shared-key"))
	require.NoError(test, flatChild.ClaimSessionIdentity(first.Request.Context(), account, "shared-key"))
}

func TestCodexIdentityClaimsRejectBeforeSending(test *testing.T) {
	test.Setenv("CODEX_OUTBOUND_SESSION_MODE", "preserve")
	handler := &Handler{}
	account := &auth.Account{DBID: 104, AccessToken: "private"}
	headers, body := fingerprintMetadataFixture(test, "root", "root", "", "", 0)
	first := codexOwnerTestContext(test, handler, "first", true)
	second := codexOwnerTestContext(test, handler, "second", true)
	require.NoError(test, NewCodexTransportFingerprint(account, headers, body, "cache").ClaimSessionIdentity(first.Request.Context(), account, "shared"))
	for _, compact := range []bool{false, true} {
		var err error
		var response *http.Response
		if compact {
			response, err = ExecuteCompactRequest(second.Request.Context(), account, body, "cache", "http://127.0.0.1:1", "shared", nil, headers)
		} else {
			response, err = ExecuteRequest(second.Request.Context(), account, body, "cache", "http://127.0.0.1:1", "shared", nil, headers, false)
		}
		require.Nil(test, response)
		require.NotNil(test, codexIdentityRequestError(err))
	}
}

func TestCodexTransportOwnersAndCachesUseVerifiedUsers(test *testing.T) {
	resetResponseCacheForTest()
	test.Cleanup(resetResponseCacheForTest)
	handler := &Handler{}
	first := codexOwnerTestContext(test, handler, "first", true)
	same := codexOwnerTestContext(test, handler, "first", true)
	second := codexOwnerTestContext(test, handler, "second", true)
	firstOwner := responseCacheOwnerForRequest(first, 1)
	require.Equal(test, firstOwner, responseCacheOwnerForRequest(same, 1))
	require.NotEqual(test, firstOwner, responseCacheOwnerForRequest(second, 1))
	require.NotEqual(test, firstOwner, responseCacheOwner(1))
	cacheCompletedResponse(firstOwner, []byte(`[{"role":"user","content":"private history"}]`), []byte(`{"type":"response.completed","response":{"id":"resp_owner_isolation","output":[{"type":"function_call","call_id":"call_owner","name":"test","arguments":"{}"}]}}`))
	require.NotEmpty(test, getResponseCache(responseCacheOwnerForRequest(same, 1), "resp_owner_isolation"))
	require.Empty(test, getResponseCache(responseCacheOwnerForRequest(second, 1), "resp_owner_isolation"))
	require.Empty(test, getResponseCache(responseCacheOwner(1), "resp_owner_isolation"))
	require.Equal(test, ScopeCodexPromptCacheKey(first.Request.Context(), "cache"), ScopeCodexPromptCacheKey(same.Request.Context(), "cache"))
	require.NotEqual(test, ScopeCodexPromptCacheKey(first.Request.Context(), "cache"), ScopeCodexPromptCacheKey(second.Request.Context(), "cache"))
	require.NotEqual(test, WebsocketTransportOwner(first.Request.Context(), "shared"), WebsocketTransportOwner(second.Request.Context(), "shared"))
	unverified := codexOwnerTestContext(test, handler, "first", false)
	require.Equal(test, responseCacheOwner(1), responseCacheOwnerForRequest(unverified, 1))
	require.Equal(test, "cache", ScopeCodexPromptCacheKey(unverified.Request.Context(), "cache"))
	anonymous, _ := gin.CreateTestContext(httptest.NewRecorder())
	anonymous.Request = httptest.NewRequest(http.MethodPost, "/responses", nil)
	owner := responseCacheOwnerForRequest(anonymous, 0)
	require.True(test, strings.HasPrefix(owner, "anonymous:"))
	require.Equal(test, owner, responseCacheOwnerForRequest(anonymous, 0))
	require.NotEqual(test, owner, responseCacheOwnerForRequest(unverified, 0))
	anonymous.Request = anonymous.Request.WithContext(WithDownstreamWebsocketConnection(context.Background()))
	connectionOwner := responseCacheOwnerForRequest(anonymous, 0)
	require.NotEqual(test, owner, connectionOwner)
	require.Equal(test, connectionOwner, responseCacheOwnerForRequest(anonymous, 0))
	anonymous.Request = anonymous.Request.WithContext(WithDownstreamWebsocketConnection(context.Background()))
	require.NotEqual(test, connectionOwner, responseCacheOwnerForRequest(anonymous, 0))
}

func TestLocalCodexClaimsCapacityDoesNotEvictOwners(test *testing.T) {
	claims := &localCodexIdentityClaims{owners: make(map[string]string)}
	for index := 0; index < 65536; index++ {
		claims.owners[strconv.Itoa(index)] = "owner"
	}
	require.Error(test, claims.ClaimCodexIdentities(context.Background(), []string{"new"}, "other"))
	require.NoError(test, claims.ClaimCodexIdentities(context.Background(), []string{"1"}, "owner"))
	require.ErrorIs(test, claims.ClaimCodexIdentities(context.Background(), []string{"1"}, "other"), database.ErrCodexIdentityConflict)
}
