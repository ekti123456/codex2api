package promptfilter

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestSessionContinuityModeDefaultsAndConfigRoundTrip(test *testing.T) {
	for _, raw := range []string{"", `{}`, `{"risk":{"enabled":true}}`} {
		config, err := ParseAdvancedConfig(raw)
		require.NoError(test, err)
		require.Equal(test, "observe", config.Risk.SessionContinuityMode)
	}
	for _, mode := range []string{"off", "observe", "enforce"} {
		document, err := ParseAdvancedConfigDocument(`{"risk":{"session_continuity_mode":"` + mode + `"}}`)
		require.NoError(test, err)
		saved, err := ParseAdvancedConfig(document.Raw)
		require.NoError(test, err)
		require.Equal(test, mode, saved.Risk.SessionContinuityMode)
	}
	_, err := ParseAdvancedConfigDocument(`{"risk":{"session_continuity_mode":"invalid"}}`)
	require.Error(test, err)
}
