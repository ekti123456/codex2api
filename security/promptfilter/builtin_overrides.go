package promptfilter

import (
	"encoding/json"
	"fmt"
	"regexp"
	"strings"
)

// BuiltinPatternOverride edits the visible rule fields while retaining the
// release's composite conditions, exclusions and signal-only classification.
type BuiltinPatternOverride struct {
	Name     string `json:"name"`
	Pattern  string `json:"pattern"`
	Weight   int    `json:"weight"`
	Category string `json:"category"`
	Strict   bool   `json:"strict"`
}

func BuiltinPatternFields(pattern PatternConfig) BuiltinPatternOverride {
	return BuiltinPatternOverride{Name: pattern.Name, Pattern: pattern.Pattern, Weight: pattern.Weight, Category: pattern.Category, Strict: pattern.Strict}
}

func ValidateBuiltinPatternOverride(rule BuiltinPatternOverride) error {
	var original *PatternConfig
	for i := range defaultPatternConfigs {
		if defaultPatternConfigs[i].Name == rule.Name {
			original = &defaultPatternConfigs[i]
			break
		}
	}
	if original == nil {
		return fmt.Errorf("内置规则不存在: %s", rule.Name)
	}
	if rule.Weight < 1 || rule.Weight > 1000 {
		return fmt.Errorf("规则权重必须为 1-1000")
	}
	if len(rule.Pattern) > 64*1024 {
		return fmt.Errorf("正则表达式不能超过 64 KiB")
	}
	if len(rule.Category) > 128 {
		return fmt.Errorf("分类不能超过 128 字节")
	}
	if strings.TrimSpace(rule.Pattern) == "" {
		if len(original.AllPatterns) == 0 && len(original.AnyPatterns) == 0 {
			return fmt.Errorf("正则表达式不能为空")
		}
	} else if _, err := regexp.Compile(rule.Pattern); err != nil {
		return fmt.Errorf("正则表达式无效: %w", err)
	}
	return nil
}

func ParseBuiltinPatternOverrides(raw string) ([]BuiltinPatternOverride, error) {
	if strings.TrimSpace(raw) == "" {
		return nil, nil
	}
	var rules []BuiltinPatternOverride
	if err := json.Unmarshal([]byte(raw), &rules); err != nil {
		return nil, err
	}
	return rules, nil
}

func EffectiveBuiltinPatternConfigs(overrides []BuiltinPatternOverride) []PatternConfig {
	patterns := BuiltinPatternConfigs()
	byName := make(map[string]BuiltinPatternOverride, len(overrides))
	for _, rule := range overrides {
		// Invalid or retired persisted edits must not disable the engine. Their
		// release defaults stay active; explicit saves are validated by the API.
		if ValidateBuiltinPatternOverride(rule) == nil {
			byName[rule.Name] = rule
		}
	}
	for i := range patterns {
		if rule, ok := byName[patterns[i].Name]; ok {
			patterns[i].Pattern, patterns[i].Weight = rule.Pattern, rule.Weight
			patterns[i].Category, patterns[i].Strict = rule.Category, rule.Strict
		}
	}
	return patterns
}
