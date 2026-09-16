package database

import (
	"context"
	"fmt"
	"strings"
	"unicode"
	"unicode/utf8"
)

// ProxyLocation is a stored approximate location, never resolved on inference.
type ProxyLocation struct {
	Country  string `json:"country,omitempty"`
	Region   string `json:"region,omitempty"`
	City     string `json:"city,omitempty"`
	Timezone string `json:"timezone,omitempty"`
}

type ProxyLocationOverrides struct {
	CountryCode *string
	Region      *string
	City        *string
}

func NormalizeProxyCountryCode(value string) string {
	value = strings.ToUpper(strings.TrimSpace(value))
	if len(value) != 2 || value[0] < 'A' || value[0] > 'Z' || value[1] < 'A' || value[1] > 'Z' {
		return ""
	}
	return value
}

func NormalizeProxyLocationText(value string) string {
	value = strings.TrimSpace(value)
	if utf8.RuneCountInString(value) > 128 || strings.IndexFunc(value, unicode.IsControl) >= 0 {
		return ""
	}
	return value
}

func ValidateProxyLocationOverrides(overrides ProxyLocationOverrides) error {
	if value := overrides.CountryCode; value != nil && strings.TrimSpace(*value) != "" && NormalizeProxyCountryCode(*value) == "" {
		return fmt.Errorf("国家代码必须是两位英文字母，例如 US、CN、JP")
	}
	for _, value := range []*string{overrides.Region, overrides.City} {
		if value != nil && strings.TrimSpace(*value) != "" && NormalizeProxyLocationText(*value) == "" {
			return fmt.Errorf("省州和城市须为不超过 128 字的文本，不能包含控制字符")
		}
	}
	return nil
}

// UpdateProxyLocationSettings edits all fields atomically with the proxy URL.
func (db *DB) UpdateProxyLocationSettings(ctx context.Context, id int64, url, label *string, enabled *bool, timezone *string, geo ProxyLocationOverrides) error {
	return db.updateProxyWithLocation(ctx, id, url, label, enabled, timezone, geo)
}
