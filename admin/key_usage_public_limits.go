package admin

import "github.com/codex2api/database"

// Explicit public projection: adding an internal scheduling field must never
// automatically expose it through the API Key self-service endpoint.
type publicAPIKeyLimits struct {
	ModelRequestLimits     []database.APIKeyModelRequestLimit `json:"model_request_limits,omitempty"`
	ModelAllow             []string                           `json:"model_allow,omitempty"`
	ModelDeny              []string                           `json:"model_deny,omitempty"`
	PlanAllow              []string                           `json:"plan_allow,omitempty"`
	RPM                    int                                `json:"rpm,omitempty"`
	RPD                    int                                `json:"rpd,omitempty"`
	MaxConcurrency         int                                `json:"max_concurrency,omitempty"`
	CostLimit5h            float64                            `json:"cost_limit_5h,omitempty"`
	CostLimit7d            float64                            `json:"cost_limit_7d,omitempty"`
	CostLimit30d           float64                            `json:"cost_limit_30d,omitempty"`
	CostLimitDaily         float64                            `json:"cost_limit_daily,omitempty"`
	TokenLimit5h           int64                              `json:"token_limit_5h,omitempty"`
	TokenLimit7d           int64                              `json:"token_limit_7d,omitempty"`
	TokenLimit30d          int64                              `json:"token_limit_30d,omitempty"`
	TokenLimitDaily        int64                              `json:"token_limit_daily,omitempty"`
	DisableImageGeneration bool                               `json:"disable_image_generation,omitempty"`
	ImageGenerationPolicy  string                             `json:"image_generation_policy,omitempty"`
	AutoCompactOnOverflow  bool                               `json:"auto_compact_overflow,omitempty"`
	AllowLive              bool                               `json:"allow_live,omitempty"`
	UpstreamChannel        string                             `json:"upstream_channel,omitempty"`
}

func newPublicAPIKeyLimits(limits database.APIKeyLimits) publicAPIKeyLimits {
	return publicAPIKeyLimits{
		ModelRequestLimits: limits.ModelRequestLimits, ModelAllow: limits.ModelAllow, ModelDeny: limits.ModelDeny, PlanAllow: limits.PlanAllow,
		RPM: limits.RPM, RPD: limits.RPD, MaxConcurrency: limits.MaxConcurrency,
		CostLimit5h: limits.CostLimit5h, CostLimit7d: limits.CostLimit7d, CostLimit30d: limits.CostLimit30d, CostLimitDaily: limits.CostLimitDaily,
		TokenLimit5h: limits.TokenLimit5h, TokenLimit7d: limits.TokenLimit7d, TokenLimit30d: limits.TokenLimit30d, TokenLimitDaily: limits.TokenLimitDaily,
		DisableImageGeneration: limits.DisableImageGeneration, ImageGenerationPolicy: limits.ImageGenerationPolicy,
		AutoCompactOnOverflow: limits.AutoCompactOnOverflow, AllowLive: limits.AllowLive, UpstreamChannel: limits.UpstreamChannel,
	}
}
