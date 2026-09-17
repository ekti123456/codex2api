package promptfilter

import (
	"fmt"
	"math"
)

type SessionCreationCooldownTier struct {
	MinAverageSeconds int `json:"min_average_seconds"`
	IntervalSeconds   int `json:"interval_seconds"`
	WindowLimitDelta  int `json:"window_limit_delta"`
}

type SessionCreationCooldownConfig struct {
	Mode                   string                        `json:"mode"`
	FrequencyWindowSeconds int                           `json:"frequency_window_seconds"`
	FreeCreations          int                           `json:"free_creations"`
	HistoryDays            int                           `json:"history_days"`
	MinSamples             int                           `json:"min_samples"`
	MaxSamples             int                           `json:"max_samples"`
	MaxIntervalSeconds     int                           `json:"max_interval_seconds"`
	Tiers                  []SessionCreationCooldownTier `json:"tiers"`
}

func DefaultSessionCreationCooldownConfig() SessionCreationCooldownConfig {
	return SessionCreationCooldownConfig{
		Mode: "off", FrequencyWindowSeconds: 1800, FreeCreations: 2,
		HistoryDays: 7, MinSamples: 10, MaxSamples: 20, MaxIntervalSeconds: 900,
		Tiers: []SessionCreationCooldownTier{{MinAverageSeconds: 900}, {MinAverageSeconds: 600, IntervalSeconds: 300}, {MinAverageSeconds: 300, IntervalSeconds: 600}, {IntervalSeconds: 900}},
	}
}

func (cfg SessionCreationCooldownConfig) Validate() error {
	if cfg.Mode != "off" && cfg.Mode != "observe" && cfg.Mode != "enforce" {
		return fmt.Errorf("session_creation_cooldown.mode must be off, observe or enforce")
	}
	if cfg.FrequencyWindowSeconds < 60 || cfg.FrequencyWindowSeconds > 86400 || cfg.FreeCreations < 1 || cfg.FreeCreations > 1000 {
		return fmt.Errorf("session_creation_cooldown: frequency window must be 60..86400 seconds and free creations 1..1000")
	}
	if cfg.HistoryDays < 1 || cfg.HistoryDays > 90 || cfg.MinSamples < 1 || cfg.MaxSamples < cfg.MinSamples || cfg.MaxSamples > 200 {
		return fmt.Errorf("session_creation_cooldown: history must be 1..90 days and 1 <= min_samples <= max_samples <= 200")
	}
	if cfg.MaxIntervalSeconds < 0 || cfg.MaxIntervalSeconds > 86400 || len(cfg.Tiers) < 1 || len(cfg.Tiers) > 12 {
		return fmt.Errorf("session_creation_cooldown: maximum interval must be 0..86400 seconds with 1..12 tiers")
	}
	seen := make(map[int]bool)
	for _, tier := range cfg.Tiers {
		if tier.MinAverageSeconds < 0 || tier.MinAverageSeconds > 2592000 || tier.IntervalSeconds < 0 || tier.IntervalSeconds > 86400 || seen[tier.MinAverageSeconds] {
			return fmt.Errorf("session_creation_cooldown: tier lower bounds must be unique (0..2592000 seconds), intervals 0..86400 seconds")
		}
		seen[tier.MinAverageSeconds] = true
		if tier.WindowLimitDelta < -100000 || tier.WindowLimitDelta > 100000 {
			return fmt.Errorf("session_creation_cooldown: window limit adjustment must be -100000..100000")
		}
	}
	if !seen[0] {
		return fmt.Errorf("session_creation_cooldown: a tier starting at zero is required")
	}
	return nil
}

func (cfg SessionCreationCooldownConfig) Interval(averageSeconds float64, samples int) int {
	tier, found := cfg.MatchTier(averageSeconds, samples)
	if !found {
		return 0
	}
	return min(tier.IntervalSeconds, cfg.MaxIntervalSeconds)
}

func (cfg SessionCreationCooldownConfig) MatchTier(averageSeconds float64, samples int) (SessionCreationCooldownTier, bool) {
	if cfg.Mode == "off" || samples < cfg.MinSamples || math.IsNaN(averageSeconds) || math.IsInf(averageSeconds, 0) || cfg.Validate() != nil {
		return SessionCreationCooldownTier{}, false
	}
	var selected SessionCreationCooldownTier
	found := false
	for _, tier := range cfg.Tiers {
		if averageSeconds >= float64(tier.MinAverageSeconds) && (!found || tier.MinAverageSeconds > selected.MinAverageSeconds) {
			selected, found = tier, true
		}
	}
	return selected, found
}

func (cfg SessionCreationCooldownConfig) HasWindowLimitAdjustment() bool {
	for _, tier := range cfg.Tiers {
		if tier.WindowLimitDelta != 0 {
			return true
		}
	}
	return false
}

func (cfg SessionCreationCooldownConfig) AdjustedWindowLimit(base int, averageSeconds float64, samples int) int {
	if base <= 0 || cfg.Mode != "enforce" {
		return base
	}
	tier, found := cfg.MatchTier(averageSeconds, samples)
	if !found || tier.WindowLimitDelta == 0 {
		return base
	}
	// Zero means unlimited in admission code, never turn a reduction into an exemption.
	return min(100000, max(1, base+tier.WindowLimitDelta))
}
