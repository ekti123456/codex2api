package proxy

import (
	"context"
	"io"
	"net/http"
	"net/http/httptest"
	"sync/atomic"
	"testing"

	"github.com/codex2api/auth"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"github.com/tidwall/sjson"
)

func TestSessionFailoverRegressionQueuedBeforeFirstMigration(test *testing.T) {
	handler, owner, target, key := failoverTestSetup(test, true)
	request, body := outboundEpochTestRequest(test, handler, 0)
	require.Nil(test, handler.configureSessionModelAffinity(request, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, body))
	require.Nil(test, handler.commitSessionContinuity(request, owner))
	atomic.StoreInt32(&owner.Disabled, 1)
	migratingRequest, migratingBody := outboundEpochTestRequest(test, handler, 0)
	require.Nil(test, handler.configureSessionModelAffinity(migratingRequest, requestSessionIdentity{stableIdentity: true}, key, "gpt-5.6-sol", "gpt-5.6-sol", false, migratingBody))
	selected, _, handled := handler.takeSessionAccountFailover(migratingRequest.Request.Context(), key, 0, nil, nil, auth.DispatchPolicyStandard)
	require.True(test, handled)
	require.Same(test, target, selected)
	handler.store.Release(selected)
	record, found, err := handler.db.ReadSessionContinuity(context.Background(), hashRiskIdentity(key))
	require.NoError(test, err)
	require.True(test, found)
	previousResin := GetResinConfig()
	test.Cleanup(func() { SetResinConfig(previousResin) })
	var received atomic.Int32
	server := httptest.NewServer(http.HandlerFunc(func(writer http.ResponseWriter, request *http.Request) {
		received.Add(1)
		_, _ = io.Copy(io.Discard, request.Body)
		writer.Header().Set("Content-Type", "application/json")
		_, _ = writer.Write([]byte(`{"id":"response-old-account"}`))
	}))
	test.Cleanup(server.Close)
	SetResinConfig(&ResinConfig{BaseURL: server.URL, PlatformName: "audit-test"})
	response, sendError := ExecuteRequest(request.Request.Context(), owner, body, "cache", "", "test-user-key", nil, request.Request.Header, false)
	if response != nil {
		_ = response.Body.Close()
	}
	test.Logf("current_owner=%d generation=%d attempted_owner=%d received_by_old_account=%d send_error=%v", record.AccountID, record.FailoverCount, owner.ID(), received.Load(), sendError)
	require.Zero(test, received.Load(), "queued generation-zero request must not send to old account after migration")
}

func TestSessionFailoverRegressionCurrentAccountCompactionAfterMigration(test *testing.T) {
	handler, owner, target, key := failoverTestSetup(test, true)
	record, _, err := handler.db.SwitchSessionContinuityAccount(context.Background(), database.SessionAccountFailover{
		RootKey: hashRiskIdentity(key), ExpectedAccountID: owner.ID(), AccountID: target.ID(),
		ResetOutboundWindow: true, WindowThreadID: continuityTestThread,
	})
	require.NoError(test, err)
	producer, _ := outboundEpochTestRequest(test, handler, 0)
	handler.attachSessionOutboundEpoch(producer, hashRiskIdentity(key), record)
	handler.recordResponseContextProvenance(producer, target, []byte(`{"output":[{"type":"compaction","id":"new-target-compaction","encrypted_content":"new-target-produced-compaction"}]}`))
	request, body := outboundEpochTestRequest(test, handler, 1)
	body, err = sjson.SetRawBytes(body, "input", []byte(`[{"type":"compaction","id":"new-target-compaction","encrypted_content":"new-target-produced-compaction"},{"role":"user","content":"Continue"}]`))
	require.NoError(test, err)
	failure := handler.validateMigratedSessionContext(request, body, record, key)
	test.Logf("current_owner=%d compaction_producer=%d validation_error=%v", record.AccountID, target.ID(), failure)
	require.Nil(test, failure, "current target account's newly generated compaction should remain usable")
}

func TestSessionFailoverRegressionForkFromMigratedParent(test *testing.T) {
	handler, owner, target, key := failoverTestSetup(test, true)
	record, _, err := handler.db.SwitchSessionContinuityAccount(context.Background(), database.SessionAccountFailover{
		RootKey: hashRiskIdentity(key), ExpectedAccountID: owner.ID(), AccountID: target.ID(),
		ResetOutboundWindow: true, WindowThreadID: continuityTestThread,
	})
	require.NoError(test, err)
	parent, parentBody := outboundEpochTestRequest(test, handler, 0)
	handler.attachSessionOutboundEpoch(parent, hashRiskIdentity(key), record)
	parentFingerprint := NewCodexTransportFingerprint(target, parent.Request.Header, parentBody, "cache")
	require.NoError(test, parentFingerprint.ClaimSessionIdentity(parent.Request.Context(), target, "test-user-key"))
	parentOutbound := gjson.GetBytes(parentFingerprint.ApplyBody(parentBody), "client_metadata.thread_id").String()
	fork, _ := outboundEpochTestRequest(test, handler, 0)
	forkBody := []byte(`{"model":"gpt-5.6-sol","input":"Plaintext fork","client_metadata":{"session_id":"01a09302-49f4-7b53-b545-91ef29610317","thread_id":"01a09302-49f4-7b53-b545-91ef29610317","x-codex-forked-from-thread-id":"01a03bb0-9da5-7772-a16a-f38258dd30c4","x-codex-window-id":"01a09302-49f4-7b53-b545-91ef29610317:0"}}`)
	forkBody, err = sjson.SetBytes(forkBody, "client_metadata.thread_source", "user")
	require.NoError(test, err)
	forkBody, err = sjson.SetBytes(forkBody, "client_metadata.request_kind", "turn")
	require.NoError(test, err)
	forkIdentity := resolveRequestRootSessionIdentity(nil, forkBody)
	require.True(test, forkIdentity.ownsUserRootBinding())
	require.Equal(test, accountIdentitySampleRoot, forkIdentity.sessionID)
	require.Nil(test, handler.restoreMigratedSessionOwner(fork, "fresh-fork-key", forkBody))
	require.Nil(test, outboundEpochFromContext(fork.Request.Context()))
	forkFingerprint := NewCodexTransportFingerprint(target, http.Header{}, forkBody, "cache")
	require.NoError(test, forkFingerprint.ClaimSessionIdentity(fork.Request.Context(), target, "test-user-key"))
	forkParentOutbound := gjson.GetBytes(forkFingerprint.ApplyBody(forkBody), "client_metadata.x-codex-forked-from-thread-id").String()
	test.Logf("actual_parent_outbound=%s fork_reference_outbound=%s original_parent=%s", parentOutbound, forkParentOutbound, continuityTestThread)
	require.Equal(test, parentOutbound, forkParentOutbound, "fork reference must resolve the parent's actual migrated identity")
}
