package proxy

import (
	"encoding/json"
	"fmt"
	"net/http"
	"net/http/httptest"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/gin-gonic/gin"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func legacyParentTestSetup(test *testing.T) (*Handler, *auth.Account, *auth.Account) {
	test.Helper()
	test.Setenv("CODEX_OUTBOUND_SESSION_MODE", "account")
	handler, owner, target, _ := failoverTestSetup(test, true)
	for _, thread := range []string{continuityTestThread, accountIdentitySampleRoot} {
		_, err := handler.db.CommitSessionContinuity(test.Context(), hashRiskIdentity(thread), database.SessionContinuityRecord{AccountID: owner.ID(), ThreadID: thread, NumberKnown: true, Number: 54})
		require.NoError(test, err)
	}
	identityOwner := codexIdentityDigest("codex-owner-v1", "credential:test-user-key")
	parentPolicyKey := codexIdentityDigest("codex-account-root-v1", identityOwner, owner.EffectiveAccountID(), continuityTestThread)
	_, err := handler.db.ResolveCodexIdentityMapping(test.Context(), parentPolicyKey, nil, false)
	require.NoError(test, err)
	return handler, owner, target
}

func legacyParentTestRequest(test *testing.T, handler *Handler, thread, parent, kind string, number uint64, stringMetadata bool) (*gin.Context, []byte) {
	test.Helper()
	metadata := map[string]any{"session_id": thread, "thread_id": thread, "window_id": fmt.Sprintf("%s:%d", thread, number), "window_number": number, "request_kind": kind, "thread_source": "user"}
	client := map[string]any{"session_id": thread, "thread_id": thread, "x-client-request-id": thread, "x-codex-window-id": metadata["window_id"]}
	if parent != "" {
		metadata["parent_thread_id"], metadata["forked_from_thread_id"] = parent, parent
		client["parent_thread_id"], client["x-codex-forked-from-thread-id"] = parent, parent
	}
	encodedMeta, err := json.Marshal(metadata)
	require.NoError(test, err)
	client["x-codex-turn-metadata"] = metadata
	if stringMetadata {
		client["x-codex-turn-metadata"] = string(encodedMeta)
	}
	body, err := json.Marshal(map[string]any{"model": "gpt-5.6-sol", "input": "Complete plaintext context", "client_metadata": client})
	require.NoError(test, err)
	request, _ := gin.CreateTestContext(httptest.NewRecorder())
	request.Request = httptest.NewRequest(http.MethodPost, "/v1/responses", nil)
	request.Request.Header.Set("Authorization", "Bearer test-user-key")
	request.Request.Header.Set("Session-Id", thread)
	request.Request.Header.Set("Thread-Id", thread)
	request.Request.Header.Set("X-Client-Request-Id", thread)
	request.Request.Header.Set("X-Codex-Window-Id", fmt.Sprint(metadata["window_id"]))
	request.Request.Header.Set(codexTurnMetadataHeader, string(encodedMeta))
	if parent != "" {
		request.Request.Header.Set(codexParentThreadIDHeader, parent)
		request.Request.Header.Set("X-Codex-Forked-From-Thread-Id", parent)
	}
	handler.bindCodexIdentityClaims(request)
	record, found, err := handler.db.ReadSessionContinuity(test.Context(), hashRiskIdentity(thread))
	require.NoError(test, err)
	require.True(test, found)
	handler.attachSessionOutboundEpoch(request, hashRiskIdentity(thread), record)
	return request, body
}

func requireLegacyParentOutput(test *testing.T, fingerprint *CodexFingerprint, body []byte, parent string) string {
	test.Helper()
	outbound := fingerprint.ApplyBody(body)
	headers := http.Header{}
	fingerprint.ApplySessionHeaders(headers)
	thread := gjson.GetBytes(outbound, "client_metadata.thread_id").String()
	require.Equal(test, thread, headers.Get("Session-Id"))
	require.Equal(test, thread, headers.Get("Thread-Id"))
	require.Equal(test, thread, headers.Get("X-Client-Request-Id"))
	require.Equal(test, thread, gjson.GetBytes(outbound, "client_metadata.session_id").String())
	require.Equal(test, parent, headers.Get(codexParentThreadIDHeader))
	require.Equal(test, parent, headers.Get("X-Codex-Forked-From-Thread-Id"))
	require.Equal(test, parent, gjson.Get(headers.Get(codexTurnMetadataHeader), "parent_thread_id").String())
	require.Equal(test, parent, gjson.GetBytes(outbound, "client_metadata.parent_thread_id").String())
	require.Equal(test, parent, gjson.GetBytes(outbound, "client_metadata.x-codex-forked-from-thread-id").String())
	meta := diagnosticMetadataObject(gjson.GetBytes(outbound, "client_metadata.x-codex-turn-metadata"))
	require.Equal(test, parent, meta.Get("parent_thread_id").String())
	require.Equal(test, parent, meta.Get("forked_from_thread_id").String())
	require.Equal(test, headers.Get("X-Codex-Window-Id"), meta.Get("window_id").String())
	return thread
}

func TestLegacyParentCompatibilityKeepsOnlyOriginalParent(test *testing.T) {
	handler, owner, _ := legacyParentTestSetup(test)
	var mappedThread string
	for _, kind := range []string{"turn", "compaction", "thread_description", "guardian_review"} {
		for _, stringMetadata := range []bool{false, true} {
			request, body := legacyParentTestRequest(test, handler, accountIdentitySampleRoot, continuityTestThread, kind, 54, stringMetadata)
			original := string(body)
			fingerprint := NewCodexTransportFingerprint(owner, request.Request.Header, body, "cache")
			require.NoError(test, fingerprint.ClaimSessionIdentity(request.Request.Context(), owner, "test-user-key"))
			thread := requireLegacyParentOutput(test, fingerprint, body, continuityTestThread)
			require.NotEqual(test, accountIdentitySampleRoot, thread)
			if mappedThread != "" {
				require.Equal(test, mappedThread, thread)
			}
			mappedThread = thread
			require.Equal(test, "mapped_with_legacy_references", fingerprint.accountIdentityDiagnostic.Status)
			require.Equal(test, "preserved_legacy_parent", fingerprint.accountIdentityDiagnostic.References[0].Action)
			require.Equal(test, "original_account_unmigrated", fingerprint.accountIdentityDiagnostic.References[0].Reason)
			require.Equal(test, original, string(body))
			require.Equal(test, continuityTestThread, request.Request.Header.Get(codexParentThreadIDHeader))
		}
	}
}

func TestLegacyParentCompatibilityDoesNotSurviveChildFailover(test *testing.T) {
	handler, owner, target := legacyParentTestSetup(test)
	request, body := legacyParentTestRequest(test, handler, accountIdentitySampleRoot, continuityTestThread, "turn", 54, false)
	oldFingerprint := NewCodexTransportFingerprint(owner, request.Request.Header, body, "cache")
	require.NoError(test, oldFingerprint.ClaimSessionIdentity(request.Request.Context(), owner, "test-user-key"))
	firstThread := requireLegacyParentOutput(test, oldFingerprint, body, continuityTestThread)
	current := owner
	for index, account := range []*auth.Account{target, owner} {
		_, _, err := handler.db.SwitchSessionContinuityAccount(test.Context(), database.SessionAccountFailover{RootKey: hashRiskIdentity(accountIdentitySampleRoot), ExpectedAccountID: current.ID(), AccountID: account.ID(), ExpectedGeneration: uint64(index), ResetOutboundWindow: true, WindowThreadID: accountIdentitySampleRoot, WindowNumber: uint64(55 + index)})
		require.NoError(test, err)
		resumed, resumedBody := legacyParentTestRequest(test, handler, accountIdentitySampleRoot, continuityTestThread, "turn", uint64(55+index), false)
		require.Error(test, oldFingerprint.ClaimSessionIdentity(resumed.Request.Context(), account, "test-user-key"))
		fingerprint := NewCodexTransportFingerprint(account, resumed.Request.Header, resumedBody, "cache")
		require.Error(test, fingerprint.ClaimSessionIdentity(resumed.Request.Context(), account, "test-user-key"))
		require.Nil(test, fingerprint.accountIdentity)
		if index == 1 {
			require.Equal(test, "request_migrated", fingerprint.accountIdentityDiagnostic.References[0].Reason)
		}
		current = account
	}
	current = owner
	var mappedParent string
	for index, account := range []*auth.Account{target, owner} {
		_, _, err := handler.db.SwitchSessionContinuityAccount(test.Context(), database.SessionAccountFailover{RootKey: hashRiskIdentity(continuityTestThread), ExpectedAccountID: current.ID(), AccountID: account.ID(), ExpectedGeneration: uint64(index), ResetOutboundWindow: true, WindowThreadID: continuityTestThread, WindowNumber: uint64(55 + index)})
		require.NoError(test, err)
		parentRequest, parentBody := legacyParentTestRequest(test, handler, continuityTestThread, "", "turn", uint64(55+index), false)
		parent := NewCodexTransportFingerprint(account, parentRequest.Request.Header, parentBody, "cache")
		require.NoError(test, parent.ClaimSessionIdentity(parentRequest.Request.Context(), account, "test-user-key"))
		mappedParent = gjson.GetBytes(parent.ApplyBody(parentBody), "client_metadata.thread_id").String()
		require.NotEqual(test, continuityTestThread, mappedParent)
		current = account
	}
	resumed, resumedBody := legacyParentTestRequest(test, handler, accountIdentitySampleRoot, continuityTestThread, "turn", 56, false)
	fingerprint := NewCodexTransportFingerprint(owner, resumed.Request.Header, resumedBody, "cache")
	require.NoError(test, fingerprint.ClaimSessionIdentity(resumed.Request.Context(), owner, "test-user-key"))
	returnedThread := requireLegacyParentOutput(test, fingerprint, resumedBody, mappedParent)
	require.NotEqual(test, firstThread, returnedThread)
	require.NotEqual(test, accountIdentitySampleRoot, returnedThread)
	require.Equal(test, "mapped", fingerprint.accountIdentityDiagnostic.References[0].Action)
	meta := diagnosticMetadataObject(gjson.GetBytes(fingerprint.ApplyBody(resumedBody), "client_metadata.x-codex-turn-metadata"))
	require.Zero(test, meta.Get("window_number").Uint())
}

func TestLegacyParentCompatibilityAlsoPreservesExistingChild(test *testing.T) {
	handler, owner, _ := legacyParentTestSetup(test)
	identityOwner := codexIdentityDigest("codex-owner-v1", "credential:test-user-key")
	childPolicyKey := codexIdentityDigest("codex-account-root-v1", identityOwner, owner.EffectiveAccountID(), accountIdentitySampleRoot)
	_, err := handler.db.ResolveCodexIdentityMapping(test.Context(), childPolicyKey, nil, false)
	require.NoError(test, err)
	request, body := legacyParentTestRequest(test, handler, accountIdentitySampleRoot, continuityTestThread, "turn", 54, false)
	fingerprint := NewCodexTransportFingerprint(owner, request.Request.Header, body, "cache")
	require.NoError(test, fingerprint.ClaimSessionIdentity(request.Request.Context(), owner, "test-user-key"))
	require.Equal(test, accountIdentitySampleRoot, requireLegacyParentOutput(test, fingerprint, body, continuityTestThread))
	require.Equal(test, "preserved_with_legacy_references", fingerprint.accountIdentityDiagnostic.Status)
	require.Equal(test, "cache", fingerprint.ScopeCacheKey(request.Request.Context(), "cache"))
}

func TestLegacyParentCompatibilityPinsOriginalAccountReference(test *testing.T) {
	handler, owner, target := legacyParentTestSetup(test)
	request, body := legacyParentTestRequest(test, handler, accountIdentitySampleRoot, continuityTestThread, "turn", 54, false)
	fingerprint := NewCodexTransportFingerprint(owner, request.Request.Header, body, "cache")
	require.NoError(test, fingerprint.ClaimSessionIdentity(request.Request.Context(), owner, "test-user-key"))
	_, _, err := handler.db.SwitchSessionContinuityAccount(test.Context(), database.SessionAccountFailover{RootKey: hashRiskIdentity(continuityTestThread), ExpectedAccountID: owner.ID(), AccountID: target.ID(), ResetOutboundWindow: true, WindowThreadID: continuityTestThread, WindowNumber: 55})
	require.NoError(test, err)
	resumed, resumedBody := legacyParentTestRequest(test, handler, accountIdentitySampleRoot, continuityTestThread, "turn", 54, false)
	next := NewCodexTransportFingerprint(owner, resumed.Request.Header, resumedBody, "cache")
	require.NoError(test, next.ClaimSessionIdentity(resumed.Request.Context(), owner, "test-user-key"))
	requireLegacyParentOutput(test, next, resumedBody, continuityTestThread)
}

func TestLegacyParentCompatibilityRequiresOriginalEpoch(test *testing.T) {
	parent := database.CodexIdentityEpoch{RootKey: hashRiskIdentity(continuityTestThread)}
	epoch := &sessionOutboundEpoch{key: hashRiskIdentity(accountIdentitySampleRoot), record: database.SessionContinuityRecord{AccountID: 1007}}
	require.Equal(test, "", legacyCodexParentReferenceBlock(epoch, 1007, parent, true))
	require.Equal(test, "request_epoch_unavailable", legacyCodexParentReferenceBlock(nil, 1007, parent, true))
	require.Equal(test, "request_epoch_unavailable", legacyCodexParentReferenceBlock(epoch, 1008, parent, true))
	require.Equal(test, "parent_owner_unavailable", legacyCodexParentReferenceBlock(epoch, 1007, parent, false))
	require.Equal(test, "parent_owner_unavailable", legacyCodexParentReferenceBlock(epoch, 1007, database.CodexIdentityEpoch{}, true))
	parent.Generation = 1
	require.Equal(test, "parent_migrated", legacyCodexParentReferenceBlock(epoch, 1007, parent, true))
	parent.Generation = 0
	epoch.record.FailoverCount = 2
	require.Equal(test, "request_migrated", legacyCodexParentReferenceBlock(epoch, 1007, parent, true))
	epoch.record.FailoverCount, epoch.record.OutboundWindowReset = 0, true
	require.Equal(test, "request_migrated", legacyCodexParentReferenceBlock(epoch, 1007, parent, true))
}
