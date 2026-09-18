package promptfilter

import (
	"testing"

	"github.com/stretchr/testify/require"
)

func TestBuiltinOverridesChangeCachedEngineAndRestore(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enabled = true
	before, err := engineForConfig(cfg)
	require.NoError(t, err)
	edit := BuiltinPatternOverride{Name: "prompt_fake_authorization", Pattern: "builtin_override_probe_987654", Weight: 83, Category: "prompt_injection", Strict: true}
	require.NoError(t, ValidateBuiltinPatternOverride(edit))
	cfg.BuiltinOverrides = []BuiltinPatternOverride{edit}
	after, err := engineForConfig(cfg)
	require.NoError(t, err)
	require.NotSame(t, before, after)
	verdict := after.InspectText("builtin_override_probe_987654")
	found := 0
	for _, match := range verdict.Matched {
		if match.Name == edit.Name {
			found++
			require.Equal(t, edit.Weight, match.Weight)
		}
	}
	require.Equal(t, 1, found, "the override must replace, not duplicate, the builtin")
	copy := NormalizeConfig(cfg)
	copy.BuiltinOverrides[0].Weight = 1
	require.Equal(t, 83, cfg.BuiltinOverrides[0].Weight)
	cfg.BuiltinOverrides = nil
	restored, err := engineForConfig(cfg)
	require.NoError(t, err)
	require.Same(t, before, restored)
	require.Empty(t, restored.InspectText("builtin_override_probe_987654").Matched)
}

func TestBuiltinOverridesKeepCompositeConditionsAndDefaults(t *testing.T) {
	for _, original := range BuiltinPatternConfigs() {
		t.Run(original.Name, func(t *testing.T) {
			edit := BuiltinPatternFields(original)
			require.NoError(t, ValidateBuiltinPatternOverride(edit))
			edit.Weight = 123
			for _, effective := range EffectiveBuiltinPatternConfigs([]BuiltinPatternOverride{edit}) {
				if effective.Name != original.Name {
					continue
				}
				require.Equal(t, 123, effective.Weight)
				effective.Weight = original.Weight
				require.Equal(t, original, effective)
			}
		})
	}
	bad := BuiltinPatternOverride{Name: "prompt_fake_authorization", Pattern: "[", Weight: 20}
	require.Error(t, ValidateBuiltinPatternOverride(bad))
	require.Equal(t, BuiltinPatternConfigs(), EffectiveBuiltinPatternConfigs([]BuiltinPatternOverride{bad}))
	bad.Name = "missing"
	require.Error(t, ValidateBuiltinPatternOverride(bad))
}
