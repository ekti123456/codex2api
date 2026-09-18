package admin

import (
	"encoding/json"
	"github.com/codex2api/database"
	"github.com/stretchr/testify/require"
	"github.com/tidwall/gjson"
	"testing"
)

func TestPublicUsageLimitsHideInternalRoutingIDs(t *testing.T) {
	row := &database.APIKeyRow{Key: "sk-audit-key", Limits: database.APIKeyLimits{RPM: 17, ModelAllow: []string{"gpt-5.6-sol"}, NoAffinityGroupIDs: []int64{987123}, ScopeLimits: []database.APIKeyScopeLimit{{ScopeType: "account", ScopeID: 654321, Requests1d: 10}}}}
	out, err := json.Marshal(newPublicAPIKeyUsageKey(row))
	require.NoError(t, err)
	require.False(t, gjson.GetBytes(out, "limits.scope_limits").Exists())
	require.False(t, gjson.GetBytes(out, "limits.no_affinity_group_ids").Exists())
	require.Equal(t, int64(17), gjson.GetBytes(out, "limits.rpm").Int())
	require.Equal(t, "gpt-5.6-sol", gjson.GetBytes(out, "limits.model_allow.0").String())
	// Projection must not mutate scheduling settings.
	require.Equal(t, int64(654321), row.Limits.ScopeLimits[0].ScopeID)
}
