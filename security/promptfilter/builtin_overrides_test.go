package promptfilter

import (
	"encoding/json"
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

func TestBuiltinOverrideCompleteConditionsAndScoring(t *testing.T) {
	cfg := DefaultConfig()
	cfg.Enabled, cfg.Mode = true, ModeBlock
	cfg.Advanced.ContextDiscount.Enabled = false
	var original PatternConfig
	for _, rule := range BuiltinPatternConfigs() {
		if rule.Name == "generic_exploit" {
			original = rule
		} else {
			cfg.DisabledPatterns = append(cfg.DisabledPatterns, rule.Name)
		}
	}
	edit := BuiltinPatternFields(original)
	edit.Pattern, edit.Weight, edit.Category = "primary_probe", 75, "probe"
	edit.AllPatterns = []string{"required_probe"}
	edit.AnyPatterns = []string{"alternative_alpha", "alternative_beta"}
	edit.ExcludePatterns = []string{"excluded_probe"}
	edit.AuthorizationExcludePatterns = []string{"authorized_probe"}
	minimum := 2
	edit.MinMatches = &minimum
	cfg.BuiltinOverrides = []BuiltinPatternOverride{edit}
	text := "primary_probe required_probe alternative_alpha alternative_beta"
	inspect := func(input string) Verdict {
		t.Helper()
		engine, err := engineForConfig(cfg)
		require.NoError(t, err)
		return engine.InspectText(input)
	}
	for _, input := range []string{
		"required_probe alternative_alpha alternative_beta", "primary_probe alternative_alpha alternative_beta",
		"primary_probe required_probe alternative_alpha", text + " excluded_probe",
	} {
		require.Empty(t, inspect(input).Matched, input)
	}
	verdict := inspect(text)
	require.Len(t, verdict.Matched, 1)
	require.True(t, verdict.Matched[0].SignalOnly)
	require.Equal(t, ActionAllow, verdict.Action)
	require.Less(t, verdict.Score, cfg.Threshold)
	auditEngine, err := engineForConfig(cfg)
	require.NoError(t, err)
	signalOnly := false
	edit.SignalOnly = &signalOnly
	cfg.BuiltinOverrides = []BuiltinPatternOverride{edit}
	executionEngine, err := engineForConfig(cfg)
	require.NoError(t, err)
	require.NotSame(t, auditEngine, executionEngine)
	require.Equal(t, ActionBlock, inspect(text).Action)
	require.Equal(t, 75, inspect(text).Score)
	cfg.Advanced.Enforcement.AuthorizedPentestAllowed = false
	require.Len(t, inspect(text+" authorized_probe").Matched, 1)
	cfg.Advanced.Enforcement.AuthorizedPentestAllowed = true
	require.Empty(t, inspect(text+" authorized_probe").Matched)
	// Empty arrays clear conditions, including conditional exclusions.
	edit.Pattern = ""
	edit.AnyPatterns, edit.ExcludePatterns, edit.AuthorizationExcludePatterns = []string{}, []string{}, []string{}
	zero := 0
	edit.MinMatches = &zero
	cfg.BuiltinOverrides = []BuiltinPatternOverride{edit}
	require.Equal(t, ActionBlock, inspect("required_probe excluded_probe authorized_probe").Action)
	// The existing strict-over-signal rule remains visible, not silently changed.
	signalOnly = true
	edit.Strict = true
	cfg.BuiltinOverrides = []BuiltinPatternOverride{edit}
	require.Equal(t, ActionBlock, inspect("required_probe").Action)
}

func TestBuiltinOverrideLegacyPersistenceAndSnapshotIsolation(t *testing.T) {
	for _, original := range BuiltinPatternConfigs() {
		legacy := BuiltinPatternOverride{Name: original.Name, Pattern: original.Pattern, Weight: original.Weight, Category: original.Category, Strict: original.Strict}
		data, err := json.Marshal([]BuiltinPatternOverride{legacy})
		require.NoError(t, err)
		parsed, err := ParseBuiltinPatternOverrides(string(data))
		require.NoError(t, err)
		require.True(t, BuiltinPatternOverridesEqual(BuiltinPatternFields(original), parsed[0]), original.Name)
		for _, effective := range EffectiveBuiltinPatternConfigs(parsed) {
			if effective.Name == original.Name {
				require.Equal(t, original, effective)
			}
		}
	}
	edit := BuiltinPatternFields(PatternConfig{Name: "generic_exploit", Pattern: "isolated_probe", Weight: 75, AllPatterns: []string{"required_probe"}, SignalOnly: true})
	cfg := NormalizeConfig(Config{BuiltinOverrides: []BuiltinPatternOverride{edit}})
	cfg.BuiltinOverrides[0].AllPatterns[0] = "changed_probe"
	*cfg.BuiltinOverrides[0].SignalOnly = false
	*cfg.BuiltinOverrides[0].MinMatches = 1
	require.Equal(t, "required_probe", edit.AllPatterns[0])
	require.True(t, *edit.SignalOnly)
	require.Zero(t, *edit.MinMatches)
	require.False(t, BuiltinPatternOverridesEqual(edit, cfg.BuiltinOverrides[0]))
}

func TestBuiltinOverrideRejectsInvalidConditions(t *testing.T) {
	base := BuiltinPatternFields(PatternConfig{Name: "generic_exploit", Pattern: "primary_probe", Weight: 50})
	for _, field := range []string{"all", "any", "exclude", "authorization"} {
		t.Run(field, func(t *testing.T) {
			for _, expression := range []string{"", "["} {
				edit := base
				switch field {
				case "all":
					edit.AllPatterns = []string{expression}
				case "any":
					edit.AnyPatterns = []string{expression}
				case "exclude":
					edit.ExcludePatterns = []string{expression}
				case "authorization":
					edit.AuthorizationExcludePatterns = []string{expression}
				}
				require.Error(t, ValidateBuiltinPatternOverride(edit))
			}
		})
	}
	for _, minimum := range []int{-1, 1} {
		edit := base
		edit.MinMatches = &minimum
		require.Error(t, ValidateBuiltinPatternOverride(edit))
	}
	base.Pattern = ""
	require.Error(t, ValidateBuiltinPatternOverride(base))
}

func TestCustomPatternAuthorizationConditionsValidatedWhileDisabled(t *testing.T) {
	rule := PatternConfig{Name: "conditional_custom", Pattern: "primary_probe_987654", Weight: 75, AuthorizationExcludePatterns: []string{"["}}
	require.Error(t, ValidateCustomPatterns([]PatternConfig{rule}))
	rule.AuthorizationExcludePatterns = []string{"authorized_probe"}
	require.NoError(t, ValidateCustomPatterns([]PatternConfig{rule}))
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
