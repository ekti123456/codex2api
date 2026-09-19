package proxy

import (
	"context"
	"io"
	"net/http"
	"strings"
	"testing"

	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
)

func TestAccountTestRawResponseDoesNotBypassUserPrivacy(t *testing.T) {
	h, account, other, _ := responsePrivacySetup(t)
	user, _, _ := responsePrivacyRequest(t, h, 101, "turn", "")
	for _, tc := range []struct {
		name string
		ctx  context.Context
	}{
		{"unbound", context.Background()},
		{"identity_store_only", WithCodexAccountTestIdentityStore(context.Background(), h.db, account)},
		{"different_account", WithCodexAccountTestRawResponse(context.Background(), other)},
		{"user_with_test_marker", WithCodexAccountTestRawResponse(user.Request.Context(), account)},
	} {
		t.Run(tc.name, func(t *testing.T) {
			resp := &http.Response{StatusCode: 200, Header: http.Header{"Content-Type": {"application/json"}, "X-Codex-Turn-State": {"official-turn"}},
				Body: io.NopCloser(strings.NewReader(`{"id":"resp_official","session_id":"official-session","output":[],"usage":{"input_tokens":23}}`))}
			var requestErr error
			finishTurnStateResponse(tc.ctx, account, &resp, &requestErr)
			require.NoError(t, requestErr)
			defer resp.Body.Close()
			body, err := io.ReadAll(resp.Body)
			require.NoError(t, err)
			require.NotContains(t, string(body), "official-session")
			require.NotContains(t, string(body), "resp_official")
			require.NotEqual(t, "official-turn", resp.Header.Get("X-Codex-Turn-State"))
			require.Equal(t, int64(23), gjson.GetBytes(body, "usage.input_tokens").Int())
			if tc.name == "user_with_test_marker" {
				require.True(t, h.db.IsManagedCodexResponseID(gjson.GetBytes(body, "id").String()))
				require.True(t, h.db.IsManagedCodexTurnStateAlias(resp.Header.Get("X-Codex-Turn-State")))
			}
		})
	}
}

func TestAccountTestRawResponseDoesNotChangeTurnStateOutbound(t *testing.T) {
	h, account, _, _ := responsePrivacySetup(t)
	ctx := WithCodexAccountTestRawResponse(WithCodexAccountTestIdentityStore(context.Background(), h.db, account), account)
	body, headers := PrepareCodexTurnStateOutbound(ctx, account,
		[]byte(`{"input":"unchanged","client_metadata":{"nested":{"X-Codex-Turn-State":"stale-state"}}}`),
		http.Header{"X-Codex-Turn-State": {"stale-state"}})
	require.NotContains(t, string(body), "stale-state")
	require.Empty(t, headers.Get("X-Codex-Turn-State"))
	require.Equal(t, "unchanged", gjson.GetBytes(body, "input").String())
}
