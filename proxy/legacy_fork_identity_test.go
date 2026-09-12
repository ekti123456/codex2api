package proxy

import (
	"fmt"
	"net/http"
	"testing"

	"github.com/codex2api/auth"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestLegacyForkPreservesLocalIdentityAndMapsParentSeparately(test *testing.T) {
	test.Setenv("CODEX_OUTBOUND_SESSION_MODE", "account")
	handler := newWindowAuthorizationHandler(test)
	ctx := WithCodexIdentityStore(test.Context(), handler.db)
	account := &auth.Account{DBID: 1695, AccountID: accountIdentitySampleAccount}
	owner := codexIdentityDigest("codex-owner-v1", "credential:test-key")
	childKey := codexIdentityDigest("codex-account-root-v1", owner, accountIdentitySampleAccount, accountIdentitySampleRoot)
	_, err := handler.db.ResolveCodexIdentityMapping(ctx, childKey, nil, false)
	require.NoError(test, err)
	parentBody := []byte(fmt.Sprintf(`{"input":"parent","client_metadata":{"session_id":"%s","thread_id":"%s"}}`, continuityTestThread, continuityTestThread))
	parent := NewCodexTransportFingerprint(account, nil, parentBody, "cache")
	require.NoError(test, parent.ClaimSessionIdentity(ctx, account, "test-key"))
	mappedParent := gjson.GetBytes(parent.ApplyBody(parentBody), "client_metadata.thread_id").String()
	require.NotEqual(test, continuityTestThread, mappedParent)
	for _, kind := range []string{"turn", "compaction", "thread_description"} {
		test.Run(kind, func(test *testing.T) {
			body := []byte(fmt.Sprintf(`{"input":"fork","client_metadata":{"session_id":"%s","thread_id":"%s","x-codex-forked-from-thread-id":"%s","x-codex-turn-metadata":{"session_id":"%s","thread_id":"%s","forked_from_thread_id":"%s","window_id":"%s:27","window_number":27,"request_kind":"%s"}}}`, accountIdentitySampleRoot, accountIdentitySampleRoot, continuityTestThread, accountIdentitySampleRoot, accountIdentitySampleRoot, continuityTestThread, accountIdentitySampleRoot, kind))
			headers := http.Header{}
			headers.Set("Session-Id", accountIdentitySampleRoot)
			headers.Set("X-Codex-Forked-From-Thread-Id", continuityTestThread)
			original := string(body)
			fingerprint := NewCodexTransportFingerprint(account, headers, body, "cache")
			require.NoError(test, fingerprint.ClaimSessionIdentity(ctx, account, "test-key"))
			outbound := fingerprint.ApplyBody(body)
			require.Equal(test, accountIdentitySampleRoot, gjson.GetBytes(outbound, "client_metadata.session_id").String())
			require.Equal(test, accountIdentitySampleRoot, gjson.GetBytes(outbound, "client_metadata.thread_id").String())
			require.Equal(test, mappedParent, gjson.GetBytes(outbound, "client_metadata.x-codex-forked-from-thread-id").String())
			require.Equal(test, mappedParent, gjson.GetBytes(outbound, "client_metadata.x-codex-turn-metadata.forked_from_thread_id").String())
			require.Equal(test, int64(27), gjson.GetBytes(outbound, "client_metadata.x-codex-turn-metadata.window_number").Int())
			require.Equal(test, original, string(body))
			require.Equal(test, continuityTestThread, headers.Get("X-Codex-Forked-From-Thread-Id"))
			outboundHeaders := http.Header{}
			fingerprint.ApplySessionHeaders(outboundHeaders)
			require.Equal(test, accountIdentitySampleRoot, outboundHeaders.Get("Session-Id"))
			require.Equal(test, mappedParent, outboundHeaders.Get("X-Codex-Forked-From-Thread-Id"))
			require.Equal(test, "cache", fingerprint.ScopeCacheKey(ctx, "cache"))
			require.Equal(test, "preserved_with_mapped_references", fingerprint.accountIdentityDiagnostic.Status)
			require.NoError(test, fingerprint.ClaimSessionIdentity(ctx, account, "test-key"))
			require.Equal(test, outbound, fingerprint.ApplyBody(outbound))
		})
	}
}

func TestLegacyForkWithoutAccountMappingKeepsOriginalParent(test *testing.T) {
	test.Setenv("CODEX_OUTBOUND_SESSION_MODE", "preserve")
	previous := CurrentRuntimeSettings()
	test.Cleanup(func() { ApplyRuntimeSettings(previous) })
	settings := previous
	settings.CodexSessionFailoverEnabled = false
	ApplyRuntimeSettings(settings)
	handler := newWindowAuthorizationHandler(test)
	ctx := WithCodexIdentityStore(test.Context(), handler.db)
	account := &auth.Account{DBID: 1695, AccountID: accountIdentitySampleAccount}
	body := []byte(fmt.Sprintf(`{"input":"fork","client_metadata":{"session_id":"%s","thread_id":"%s","parent_thread_id":"%s"}}`, accountIdentitySampleRoot, accountIdentitySampleRoot, continuityTestThread))
	fingerprint := NewCodexTransportFingerprint(account, nil, body, "cache")
	require.NoError(test, fingerprint.ClaimSessionIdentity(ctx, account, "test-key"))
	require.Equal(test, continuityTestThread, gjson.GetBytes(fingerprint.ApplyBody(body), "client_metadata.parent_thread_id").String())
	require.Nil(test, fingerprint.accountIdentity)
}
