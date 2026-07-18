package promptfilter

import (
	"bytes"
	"compress/gzip"
	"compress/zlib"
	"encoding/base64"
	"encoding/hex"
	"encoding/json"
	"html"
	"io"
	"net/url"
	"sort"
	"strconv"
	"strings"
	"unicode"
	"unicode/utf8"

	"golang.org/x/text/unicode/norm"
)

type AdvancedConfig struct {
	Normalization   NormalizationConfig   `json:"normalization"`
	ContextDiscount ContextDiscountConfig `json:"context_discount"`
	Enforcement     EnforcementConfig     `json:"enforcement"`
	Risk            RiskConfig            `json:"risk"`
	Sidecar         SidecarConfig         `json:"sidecar"`
	Session         SessionConfig         `json:"session"`
	Attachment      AttachmentConfig      `json:"attachment"`
	Output          OutputConfig          `json:"output"`
	Intelligence    IntelligenceConfig    `json:"intelligence"`
	NewAPI          NewAPIConfig          `json:"newapi"`
	Guard           GuardConfig           `json:"guard"`
}

const (
	GuardModeInherit = "inherit"
	GuardModeOff     = "off"
	GuardModeShadow  = "shadow"
	GuardModeWarn    = "warn"
	GuardModeEnforce = "enforce"

	GuardProfileBalanced = "balanced"
	GuardProfileStrict   = "strict"
	GuardProfileResearch = "research"
)

// GuardConfig controls the extensible request guard pipeline. The existing
// prompt-filter switch remains the master switch; Mode "inherit" maps the
// existing monitor/warn/block setting into shadow/warn/enforce.
// New source layers default to profile-controlled behavior; the balanced
// profile scans only the current user prompt, preserving legacy behavior.
type GuardConfig struct {
	Mode                  string            `json:"mode"`
	DefaultProfile        string            `json:"default_profile"`
	AllowTrustedOverrides bool              `json:"allow_trusted_overrides"`
	ProviderProfiles      map[string]string `json:"provider_profiles,omitempty"`
	Layers                GuardLayerConfig  `json:"layers"`
}

type GuardLayerConfig struct {
	CurrentUser       GuardLayerModeConfig `json:"current_user"`
	History           GuardLayerModeConfig `json:"history"`
	System            GuardLayerModeConfig `json:"system"`
	Developer         GuardLayerModeConfig `json:"developer"`
	Instructions      GuardLayerModeConfig `json:"instructions"`
	ToolOutput        GuardLayerModeConfig `json:"tool_output"`
	ToolArguments     GuardLayerModeConfig `json:"tool_arguments"`
	AttachmentRefs    GuardLayerModeConfig `json:"attachment_refs"`
	SessionContext    GuardLayerModeConfig `json:"session_context"`
	AttachmentContent GuardLayerModeConfig `json:"attachment_content"`
}

type GuardLayerModeConfig struct {
	Mode string `json:"mode"`
}

// NewAPIConfig controls signed identity propagation and repeat-offender directives.
type NewAPIConfig struct {
	Enabled              bool   `json:"enabled"`
	MaxClockSkewSeconds  int    `json:"max_clock_skew_seconds"`
	OffenseWindowSeconds int    `json:"offense_window_seconds"`
	BanAfter             int    `json:"ban_after"`
	Secret               string `json:"-"`
}

type EnforcementConfig struct {
	TerminalCategories []string `json:"terminal_categories"`
}

type NormalizationConfig struct {
	Enabled           bool `json:"enabled"`
	DecodeURL         bool `json:"decode_url"`
	DecodeHTML        bool `json:"decode_html"`
	DecodeBase64      bool `json:"decode_base64"`
	DecodeHex         bool `json:"decode_hex"`
	DecodeROT13       bool `json:"decode_rot13"`
	DecodeEscapes     bool `json:"decode_escapes"`
	DecodeCompression bool `json:"decode_compression"`
	MaxDecodeRuns     int  `json:"max_decode_runs"`
	MaxDecodedBytes   int  `json:"max_decoded_bytes"`
	MaxEncodedBlocks  int  `json:"max_encoded_blocks"`
}

// ContextDiscountConfig keeps legitimate defensive analysis usable without
// allowing a stack of generic "research only" statements to erase an
// otherwise explicit operational request.
type ContextDiscountConfig struct {
	Enabled                bool `json:"enabled"`
	IntentAware            bool `json:"intent_aware"`
	MaxDiscount            int  `json:"max_discount"`
	OperationalMaxDiscount int  `json:"operational_max_discount"`
}

type RiskConfig struct {
	Enabled              bool `json:"enabled"`
	WindowSeconds        int  `json:"window_seconds"`
	BlockThreshold       int  `json:"block_threshold"`
	ReviewThreshold      int  `json:"review_threshold"`
	UserWeightPercent    int  `json:"user_weight_percent"`
	IPWeightPercent      int  `json:"ip_weight_percent"`
	SessionWeightPercent int  `json:"session_weight_percent"`
}

type SidecarConfig struct {
	Enabled                bool   `json:"enabled"`
	BaseURL                string `json:"base_url"`
	TimeoutSeconds         int    `json:"timeout_seconds"`
	FailClosed             bool   `json:"fail_closed"`
	MinScore               int    `json:"min_score"`
	ScanCleanEnabled       bool   `json:"scan_clean_enabled"`
	SamplePercent          int    `json:"sample_percent"`
	Mode                   string `json:"mode"`
	MaxTextLength          int    `json:"max_text_length"`
	CacheTTLSeconds        int    `json:"cache_ttl_seconds"`
	MaxConcurrent          int    `json:"max_concurrent"`
	CircuitBreakerFailures int    `json:"circuit_breaker_failures"`
	CircuitBreakerSeconds  int    `json:"circuit_breaker_seconds"`
}

type SessionConfig struct {
	Enabled               bool `json:"enabled"`
	WindowSeconds         int  `json:"window_seconds"`
	MaxFragments          int  `json:"max_fragments"`
	MaxTextLength         int  `json:"max_text_length"`
	CombineShortFragments bool `json:"combine_short_fragments"`
	ShortFragmentMaxChars int  `json:"short_fragment_max_chars"`
	RequireSignedIdentity bool `json:"require_signed_identity"`
}

type AttachmentConfig struct {
	Enabled                bool   `json:"enabled"`
	BaseURL                string `json:"base_url"`
	TimeoutSeconds         int    `json:"timeout_seconds"`
	MaxFiles               int    `json:"max_files"`
	MaxBytes               int    `json:"max_bytes"`
	MaxExtractedChars      int    `json:"max_extracted_chars"`
	CacheTTLSeconds        int    `json:"cache_ttl_seconds"`
	MaxConcurrent          int    `json:"max_concurrent"`
	CircuitBreakerFailures int    `json:"circuit_breaker_failures"`
	CircuitBreakerSeconds  int    `json:"circuit_breaker_seconds"`
	AllowRemoteURLs        bool   `json:"allow_remote_urls"`
}

type OutputConfig struct {
	Enabled      bool `json:"enabled"`
	BufferBytes  int  `json:"buffer_bytes"`
	OverlapBytes int  `json:"overlap_bytes"`
	StrictOnly   bool `json:"strict_only"`
}

// IntelligenceConfig controls the optional public-source rule intelligence job.
// It is disabled by default and never auto-adds rules unless AutoAdd is explicitly enabled.
type IntelligenceConfig struct {
	Enabled          bool     `json:"enabled"`
	IntervalHours    int      `json:"interval_hours"`
	Queries          []string `json:"queries"`
	MaxSearchResults int      `json:"max_search_results"`
	ModelEnabled     bool     `json:"model_enabled"`
	Model            string   `json:"model"`
	MaxModelCalls    int      `json:"max_model_calls"`
	AutoAdd          bool     `json:"auto_add"`
}

func DefaultIntelligenceQueries() []string {
	return []string{
		"LLM jailbreak prompt injection",
		"ChatGPT jailbreak prompt",
		"Codex prompt injection jailbreak",
		"大模型 破限 提示词",
		"GPT 破甲 提示词",
		"AI 越狱 提示词",
		"中文 prompt injection 绕过",
	}
}

func DefaultAdvancedConfig() AdvancedConfig {
	return AdvancedConfig{
		Normalization:   NormalizationConfig{MaxDecodeRuns: 1, MaxDecodedBytes: 32768, MaxEncodedBlocks: 16},
		ContextDiscount: ContextDiscountConfig{Enabled: true, IntentAware: true, MaxDiscount: 90, OperationalMaxDiscount: 0},
		Risk:            RiskConfig{WindowSeconds: 600, BlockThreshold: 100, ReviewThreshold: 60, UserWeightPercent: 50, IPWeightPercent: 30, SessionWeightPercent: 20},
		Sidecar:         SidecarConfig{TimeoutSeconds: 1, FailClosed: false, MinScore: 30, SamplePercent: 5, Mode: GuardModeShadow, MaxTextLength: 8192, CacheTTLSeconds: 60, MaxConcurrent: 16, CircuitBreakerFailures: 3, CircuitBreakerSeconds: 30},
		Session:         SessionConfig{WindowSeconds: 300, MaxFragments: 3, MaxTextLength: 4096, ShortFragmentMaxChars: 24, RequireSignedIdentity: true},
		Attachment:      AttachmentConfig{TimeoutSeconds: 2, MaxFiles: 4, MaxBytes: 65536, MaxExtractedChars: 8192, CacheTTLSeconds: 300, MaxConcurrent: 8, CircuitBreakerFailures: 3, CircuitBreakerSeconds: 30},
		Output:          OutputConfig{BufferBytes: 4096, OverlapBytes: 512, StrictOnly: true},
		Intelligence:    IntelligenceConfig{IntervalHours: 24, Queries: DefaultIntelligenceQueries(), MaxSearchResults: 20, Model: "gpt-5.4", MaxModelCalls: 1},
		NewAPI:          NewAPIConfig{MaxClockSkewSeconds: 120, OffenseWindowSeconds: 86400, BanAfter: 2},
		Guard:           DefaultGuardConfig(),
	}
}

// RecommendedAdvancedConfig is intentionally separate from
// DefaultAdvancedConfig. The latter remains the compatibility fallback for
// older databases whose JSON is empty or missing fields; this preset is used
// only for fresh installs and explicit "recommended defaults" in the UI.
func RecommendedAdvancedConfig() AdvancedConfig {
	cfg := DefaultAdvancedConfig()
	cfg.Normalization = NormalizationConfig{
		Enabled:           true,
		DecodeURL:         true,
		DecodeHTML:        true,
		DecodeBase64:      true,
		DecodeHex:         true,
		DecodeROT13:       true,
		DecodeEscapes:     true,
		DecodeCompression: true,
		MaxDecodeRuns:     2,
		MaxDecodedBytes:   32768,
		MaxEncodedBlocks:  16,
	}
	cfg.Risk.UserWeightPercent = 60
	cfg.Risk.IPWeightPercent = 20
	cfg.Risk.SessionWeightPercent = 20
	cfg.Sidecar.FailClosed = false
	cfg.Session.Enabled = true
	cfg.Guard.DefaultProfile = GuardProfileBalanced
	cfg.Guard.Layers = GuardLayerConfig{
		CurrentUser:       GuardLayerModeConfig{Mode: GuardModeEnforce},
		History:           GuardLayerModeConfig{Mode: GuardModeOff},
		System:            GuardLayerModeConfig{Mode: GuardModeOff},
		Developer:         GuardLayerModeConfig{Mode: GuardModeOff},
		Instructions:      GuardLayerModeConfig{Mode: GuardModeOff},
		ToolOutput:        GuardLayerModeConfig{Mode: GuardModeShadow},
		ToolArguments:     GuardLayerModeConfig{Mode: GuardModeOff},
		AttachmentRefs:    GuardLayerModeConfig{Mode: GuardModeShadow},
		SessionContext:    GuardLayerModeConfig{Mode: GuardModeShadow},
		AttachmentContent: GuardLayerModeConfig{Mode: GuardModeShadow},
	}
	return NormalizeAdvancedConfig(cfg)
}

func DefaultGuardConfig() GuardConfig {
	inherit := GuardLayerModeConfig{Mode: GuardModeInherit}
	return GuardConfig{
		Mode:             GuardModeInherit,
		DefaultProfile:   GuardProfileBalanced,
		ProviderProfiles: map[string]string{},
		Layers: GuardLayerConfig{
			CurrentUser:       inherit,
			History:           inherit,
			System:            inherit,
			Developer:         inherit,
			Instructions:      inherit,
			ToolOutput:        inherit,
			ToolArguments:     inherit,
			AttachmentRefs:    inherit,
			SessionContext:    inherit,
			AttachmentContent: inherit,
		},
	}
}

func ParseAdvancedConfig(raw string) (AdvancedConfig, error) {
	cfg := DefaultAdvancedConfig()
	if strings.TrimSpace(raw) == "" {
		return cfg, nil
	}
	if err := json.Unmarshal([]byte(raw), &cfg); err != nil {
		return AdvancedConfig{}, err
	}
	return NormalizeAdvancedConfig(cfg), nil
}

func MarshalAdvancedConfig(cfg AdvancedConfig) string {
	b, err := json.Marshal(NormalizeAdvancedConfig(cfg))
	if err != nil {
		return "{}"
	}
	return string(b)
}

func NormalizeAdvancedConfig(cfg AdvancedConfig) AdvancedConfig {
	d := DefaultAdvancedConfig()
	cfg.Guard = NormalizeGuardConfig(cfg.Guard)
	seenCategories := map[string]bool{}
	categories := make([]string, 0, len(cfg.Enforcement.TerminalCategories))
	for _, category := range cfg.Enforcement.TerminalCategories {
		category = strings.ToLower(strings.TrimSpace(category))
		if category != "" && !seenCategories[category] {
			seenCategories[category] = true
			categories = append(categories, category)
		}
	}
	cfg.Enforcement.TerminalCategories = categories
	if cfg.Normalization.MaxDecodeRuns <= 0 {
		cfg.Normalization.MaxDecodeRuns = d.Normalization.MaxDecodeRuns
	}
	if cfg.Normalization.MaxDecodeRuns > 2 {
		cfg.Normalization.MaxDecodeRuns = 2
	}
	if cfg.Normalization.MaxDecodedBytes <= 0 {
		cfg.Normalization.MaxDecodedBytes = d.Normalization.MaxDecodedBytes
	}
	if cfg.Normalization.MaxDecodedBytes > 65536 {
		cfg.Normalization.MaxDecodedBytes = 65536
	}
	if cfg.Normalization.MaxEncodedBlocks <= 0 {
		cfg.Normalization.MaxEncodedBlocks = d.Normalization.MaxEncodedBlocks
	}
	if cfg.Normalization.MaxEncodedBlocks > 32 {
		cfg.Normalization.MaxEncodedBlocks = 32
	}
	if cfg.ContextDiscount.MaxDiscount < 0 {
		cfg.ContextDiscount.MaxDiscount = 0
	}
	if cfg.ContextDiscount.MaxDiscount > 90 {
		cfg.ContextDiscount.MaxDiscount = 90
	}
	if cfg.ContextDiscount.OperationalMaxDiscount < 0 {
		cfg.ContextDiscount.OperationalMaxDiscount = 0
	}
	if cfg.ContextDiscount.OperationalMaxDiscount > cfg.ContextDiscount.MaxDiscount {
		cfg.ContextDiscount.OperationalMaxDiscount = cfg.ContextDiscount.MaxDiscount
	}
	if cfg.Risk.WindowSeconds <= 0 {
		cfg.Risk.WindowSeconds = d.Risk.WindowSeconds
	}
	if cfg.Risk.WindowSeconds > 86400 {
		cfg.Risk.WindowSeconds = 86400
	}
	if cfg.Risk.BlockThreshold <= 0 {
		cfg.Risk.BlockThreshold = d.Risk.BlockThreshold
	}
	if cfg.Risk.ReviewThreshold <= 0 {
		cfg.Risk.ReviewThreshold = d.Risk.ReviewThreshold
	}
	if cfg.Sidecar.TimeoutSeconds <= 0 {
		cfg.Sidecar.TimeoutSeconds = d.Sidecar.TimeoutSeconds
	}
	if cfg.Sidecar.TimeoutSeconds > 30 {
		cfg.Sidecar.TimeoutSeconds = 30
	}
	if cfg.Sidecar.MinScore < 0 {
		cfg.Sidecar.MinScore = 0
	}
	if cfg.Sidecar.MinScore > 100 {
		cfg.Sidecar.MinScore = 100
	}
	if cfg.Sidecar.SamplePercent < 0 {
		cfg.Sidecar.SamplePercent = 0
	}
	if cfg.Sidecar.SamplePercent > 100 {
		cfg.Sidecar.SamplePercent = 100
	}
	switch strings.ToLower(strings.TrimSpace(cfg.Sidecar.Mode)) {
	case GuardModeShadow, GuardModeWarn, GuardModeEnforce:
		cfg.Sidecar.Mode = strings.ToLower(strings.TrimSpace(cfg.Sidecar.Mode))
	default:
		cfg.Sidecar.Mode = d.Sidecar.Mode
	}
	if cfg.Sidecar.MaxTextLength <= 0 {
		cfg.Sidecar.MaxTextLength = d.Sidecar.MaxTextLength
	}
	if cfg.Sidecar.MaxTextLength > 65536 {
		cfg.Sidecar.MaxTextLength = 65536
	}
	if cfg.Sidecar.CacheTTLSeconds < 0 {
		cfg.Sidecar.CacheTTLSeconds = 0
	}
	if cfg.Sidecar.CacheTTLSeconds > 86400 {
		cfg.Sidecar.CacheTTLSeconds = 86400
	}
	if cfg.Sidecar.MaxConcurrent <= 0 {
		cfg.Sidecar.MaxConcurrent = d.Sidecar.MaxConcurrent
	}
	if cfg.Sidecar.MaxConcurrent > 128 {
		cfg.Sidecar.MaxConcurrent = 128
	}
	if cfg.Sidecar.CircuitBreakerFailures <= 0 {
		cfg.Sidecar.CircuitBreakerFailures = d.Sidecar.CircuitBreakerFailures
	}
	if cfg.Sidecar.CircuitBreakerFailures > 20 {
		cfg.Sidecar.CircuitBreakerFailures = 20
	}
	if cfg.Sidecar.CircuitBreakerSeconds <= 0 {
		cfg.Sidecar.CircuitBreakerSeconds = d.Sidecar.CircuitBreakerSeconds
	}
	if cfg.Sidecar.CircuitBreakerSeconds > 3600 {
		cfg.Sidecar.CircuitBreakerSeconds = 3600
	}
	if cfg.Session.WindowSeconds <= 0 {
		cfg.Session.WindowSeconds = d.Session.WindowSeconds
	}
	if cfg.Session.WindowSeconds > 3600 {
		cfg.Session.WindowSeconds = 3600
	}
	if cfg.Session.MaxFragments <= 0 {
		cfg.Session.MaxFragments = d.Session.MaxFragments
	}
	if cfg.Session.MaxFragments > 10 {
		cfg.Session.MaxFragments = 10
	}
	if cfg.Session.MaxTextLength <= 0 {
		cfg.Session.MaxTextLength = d.Session.MaxTextLength
	}
	if cfg.Session.MaxTextLength > 16384 {
		cfg.Session.MaxTextLength = 16384
	}
	if cfg.Session.ShortFragmentMaxChars <= 0 {
		cfg.Session.ShortFragmentMaxChars = d.Session.ShortFragmentMaxChars
	}
	if cfg.Session.ShortFragmentMaxChars > 256 {
		cfg.Session.ShortFragmentMaxChars = 256
	}
	if cfg.Attachment.TimeoutSeconds <= 0 {
		cfg.Attachment.TimeoutSeconds = d.Attachment.TimeoutSeconds
	}
	if cfg.Attachment.TimeoutSeconds > 30 {
		cfg.Attachment.TimeoutSeconds = 30
	}
	if cfg.Attachment.MaxFiles <= 0 {
		cfg.Attachment.MaxFiles = d.Attachment.MaxFiles
	}
	if cfg.Attachment.MaxFiles > 16 {
		cfg.Attachment.MaxFiles = 16
	}
	if cfg.Attachment.MaxBytes < 1024 {
		cfg.Attachment.MaxBytes = d.Attachment.MaxBytes
	}
	if cfg.Attachment.MaxBytes > 1048576 {
		cfg.Attachment.MaxBytes = 1048576
	}
	if cfg.Attachment.MaxExtractedChars <= 0 {
		cfg.Attachment.MaxExtractedChars = d.Attachment.MaxExtractedChars
	}
	if cfg.Attachment.MaxExtractedChars > 65536 {
		cfg.Attachment.MaxExtractedChars = 65536
	}
	if cfg.Attachment.CacheTTLSeconds < 0 {
		cfg.Attachment.CacheTTLSeconds = 0
	}
	if cfg.Attachment.CacheTTLSeconds > 86400 {
		cfg.Attachment.CacheTTLSeconds = 86400
	}
	if cfg.Attachment.MaxConcurrent <= 0 {
		cfg.Attachment.MaxConcurrent = d.Attachment.MaxConcurrent
	}
	if cfg.Attachment.MaxConcurrent > 64 {
		cfg.Attachment.MaxConcurrent = 64
	}
	if cfg.Attachment.CircuitBreakerFailures <= 0 {
		cfg.Attachment.CircuitBreakerFailures = d.Attachment.CircuitBreakerFailures
	}
	if cfg.Attachment.CircuitBreakerFailures > 20 {
		cfg.Attachment.CircuitBreakerFailures = 20
	}
	if cfg.Attachment.CircuitBreakerSeconds <= 0 {
		cfg.Attachment.CircuitBreakerSeconds = d.Attachment.CircuitBreakerSeconds
	}
	if cfg.Attachment.CircuitBreakerSeconds > 3600 {
		cfg.Attachment.CircuitBreakerSeconds = 3600
	}
	if cfg.Output.BufferBytes < 512 {
		cfg.Output.BufferBytes = d.Output.BufferBytes
	}
	if cfg.Output.BufferBytes > 65536 {
		cfg.Output.BufferBytes = 65536
	}
	if cfg.Output.OverlapBytes < 64 {
		cfg.Output.OverlapBytes = d.Output.OverlapBytes
	}
	if cfg.Output.OverlapBytes >= cfg.Output.BufferBytes {
		cfg.Output.OverlapBytes = cfg.Output.BufferBytes / 4
	}
	if cfg.Intelligence.IntervalHours < 1 {
		cfg.Intelligence.IntervalHours = d.Intelligence.IntervalHours
	}
	if cfg.Intelligence.IntervalHours > 720 {
		cfg.Intelligence.IntervalHours = 720
	}
	if cfg.Intelligence.MaxSearchResults < 1 {
		cfg.Intelligence.MaxSearchResults = d.Intelligence.MaxSearchResults
	}
	if cfg.Intelligence.MaxSearchResults > 100 {
		cfg.Intelligence.MaxSearchResults = 100
	}
	if strings.TrimSpace(cfg.Intelligence.Model) == "" {
		cfg.Intelligence.Model = d.Intelligence.Model
	}
	if cfg.Intelligence.MaxModelCalls < 0 {
		cfg.Intelligence.MaxModelCalls = 0
	}
	if cfg.Intelligence.MaxModelCalls > 3 {
		cfg.Intelligence.MaxModelCalls = 3
	}
	if cfg.NewAPI.MaxClockSkewSeconds < 30 {
		cfg.NewAPI.MaxClockSkewSeconds = d.NewAPI.MaxClockSkewSeconds
	}
	if cfg.NewAPI.MaxClockSkewSeconds > 600 {
		cfg.NewAPI.MaxClockSkewSeconds = 600
	}
	if cfg.NewAPI.OffenseWindowSeconds < 60 {
		cfg.NewAPI.OffenseWindowSeconds = d.NewAPI.OffenseWindowSeconds
	}
	if cfg.NewAPI.OffenseWindowSeconds > 2592000 {
		cfg.NewAPI.OffenseWindowSeconds = 2592000
	}
	if cfg.NewAPI.BanAfter < 2 {
		cfg.NewAPI.BanAfter = d.NewAPI.BanAfter
	}
	if cfg.NewAPI.BanAfter > 10 {
		cfg.NewAPI.BanAfter = 10
	}
	queries := make([]string, 0, len(cfg.Intelligence.Queries))
	for _, query := range cfg.Intelligence.Queries {
		query = strings.TrimSpace(query)
		if query != "" && len(queries) < 10 {
			queries = append(queries, query)
		}
	}
	cfg.Intelligence.Queries = queries
	return cfg
}

func NormalizeGuardConfig(cfg GuardConfig) GuardConfig {
	d := DefaultGuardConfig()
	cfg.Mode = normalizeGuardMode(cfg.Mode, d.Mode)
	cfg.DefaultProfile = normalizeGuardProfileName(cfg.DefaultProfile, d.DefaultProfile)

	profiles := make(map[string]string, len(d.ProviderProfiles)+len(cfg.ProviderProfiles))
	for provider, profile := range d.ProviderProfiles {
		profiles[provider] = profile
	}
	for provider, profile := range cfg.ProviderProfiles {
		provider = strings.ToLower(strings.TrimSpace(provider))
		if provider == "" {
			continue
		}
		profiles[provider] = normalizeGuardProfileName(profile, cfg.DefaultProfile)
	}
	cfg.ProviderProfiles = profiles

	cfg.Layers.CurrentUser.Mode = normalizeGuardMode(cfg.Layers.CurrentUser.Mode, d.Layers.CurrentUser.Mode)
	cfg.Layers.History.Mode = normalizeGuardMode(cfg.Layers.History.Mode, d.Layers.History.Mode)
	cfg.Layers.System.Mode = normalizeGuardMode(cfg.Layers.System.Mode, d.Layers.System.Mode)
	cfg.Layers.Developer.Mode = normalizeGuardMode(cfg.Layers.Developer.Mode, d.Layers.Developer.Mode)
	cfg.Layers.Instructions.Mode = normalizeGuardMode(cfg.Layers.Instructions.Mode, d.Layers.Instructions.Mode)
	cfg.Layers.ToolOutput.Mode = normalizeGuardMode(cfg.Layers.ToolOutput.Mode, d.Layers.ToolOutput.Mode)
	cfg.Layers.ToolArguments.Mode = normalizeGuardMode(cfg.Layers.ToolArguments.Mode, d.Layers.ToolArguments.Mode)
	cfg.Layers.AttachmentRefs.Mode = normalizeGuardMode(cfg.Layers.AttachmentRefs.Mode, d.Layers.AttachmentRefs.Mode)
	cfg.Layers.SessionContext.Mode = normalizeGuardMode(cfg.Layers.SessionContext.Mode, d.Layers.SessionContext.Mode)
	cfg.Layers.AttachmentContent.Mode = normalizeGuardMode(cfg.Layers.AttachmentContent.Mode, d.Layers.AttachmentContent.Mode)
	return cfg
}

func normalizeGuardMode(mode string, fallback string) string {
	switch strings.ToLower(strings.TrimSpace(mode)) {
	case GuardModeInherit:
		return GuardModeInherit
	case GuardModeOff:
		return GuardModeOff
	case GuardModeShadow:
		return GuardModeShadow
	case GuardModeWarn:
		return GuardModeWarn
	case GuardModeEnforce:
		return GuardModeEnforce
	default:
		if fallback == "" {
			return GuardModeInherit
		}
		return fallback
	}
}

func normalizeGuardProfileName(name string, fallback string) string {
	switch strings.ToLower(strings.TrimSpace(name)) {
	case GuardProfileBalanced:
		return GuardProfileBalanced
	case GuardProfileStrict:
		return GuardProfileStrict
	case GuardProfileResearch:
		return GuardProfileResearch
	default:
		if fallback == "" {
			return GuardProfileBalanced
		}
		return fallback
	}
}

func scanViews(text string, cfg NormalizationConfig) []string {
	base := normalizeForScan(text)
	if !cfg.Enabled {
		return []string{base}
	}

	const (
		maxNormalizationViews   = 64
		maxNormalizationSources = 32
		maxEncodedFragmentBytes = 16 * 1024
	)

	views := []string{base}
	viewSet := map[string]struct{}{base: {}}
	addOne := func(value string) {
		value = normalizeForScan(value)
		if value == "" || len(value) > DefaultMaxTextLength*4 || len(views) >= maxNormalizationViews {
			return
		}
		if _, exists := viewSet[value]; exists {
			return
		}
		viewSet[value] = struct{}{}
		views = append(views, value)
	}
	add := func(value string) {
		canonical := norm.NFKC.String(stripInvisible(value))
		addOne(canonical)
		addOne(compactForScan(canonical))
	}

	maxDecodedBytes := cfg.MaxDecodedBytes
	if maxDecodedBytes <= 0 || maxDecodedBytes > 65536 {
		maxDecodedBytes = 32768
	}
	maxEncodedBlocks := cfg.MaxEncodedBlocks
	if maxEncodedBlocks <= 0 || maxEncodedBlocks > 32 {
		maxEncodedBlocks = 16
	}
	budget := decodeBudget{remainingBytes: maxDecodedBytes, maxBlocks: maxEncodedBlocks}
	normalized := norm.NFKC.String(stripInvisible(text))
	add(normalized)
	sourceSet := map[string]struct{}{normalized: {}}
	frontier := []string{normalized}

	for run := 0; run < cfg.MaxDecodeRuns && len(frontier) > 0 && budget.remainingBytes > 0; run++ {
		next := make([]string, 0, len(frontier))
		enqueue := func(value string, encodedBlock bool) bool {
			if value == "" || len(value) > DefaultMaxTextLength*4 || len(sourceSet) >= maxNormalizationSources {
				return false
			}
			if _, exists := sourceSet[value]; exists {
				return false
			}
			if !budget.accept(len(value), encodedBlock) {
				return false
			}
			sourceSet[value] = struct{}{}
			add(value)
			next = append(next, value)
			return true
		}

		for _, source := range frontier {
			if cfg.DecodeURL {
				if decoded, err := url.QueryUnescape(source); err == nil && decoded != source {
					enqueue(decoded, false)
				}
			}
			if cfg.DecodeHTML {
				if decoded := html.UnescapeString(source); decoded != source {
					enqueue(decoded, false)
				}
			}
			if cfg.DecodeEscapes {
				if decoded, changed := decodeEscapedText(source); changed {
					enqueue(decoded, false)
				}
			}
			if cfg.DecodeBase64 || cfg.DecodeHex {
				decodedBlocks := decodeEmbeddedBlocks(source, cfg, budget.remainingBytes, budget.maxBlocks-budget.blocks, maxEncodedFragmentBytes)
				joined := make([]string, 0, len(decodedBlocks))
				accepted := make([]decodedBlock, 0, len(decodedBlocks))
				for _, block := range decodedBlocks {
					if enqueue(block.value, true) {
						accepted = append(accepted, block)
						joined = append(joined, block.value)
					}
				}
				if len(joined) > 1 {
					enqueue(strings.Join(joined, " "), false)
				}
				if len(accepted) > 0 {
					enqueue(replaceDecodedBlocks(source, accepted), false)
				}
			}
			// ROT13 is intentionally last: unlike the structured decoders it can
			// transform any ordinary English text, so it must not consume the
			// shared byte budget before Base64/hex/compressed payloads are handled.
			if cfg.DecodeROT13 {
				if decoded, ok := decodeROT13Text(source); ok {
					enqueue(decoded, false)
				}
			}
		}
		frontier = next
	}
	return views
}

type decodeBudget struct {
	remainingBytes int
	blocks         int
	maxBlocks      int
}

func (b *decodeBudget) accept(size int, encodedBlock bool) bool {
	if size <= 0 || size > b.remainingBytes {
		return false
	}
	if encodedBlock && b.blocks >= b.maxBlocks {
		return false
	}
	b.remainingBytes -= size
	if encodedBlock {
		b.blocks++
	}
	return true
}

type decodedBlock struct {
	start int
	end   int
	value string
}

type encodedCandidate struct {
	start int
	end   int
	value string
	kind  string
}

func decodeEmbeddedBlocks(text string, cfg NormalizationConfig, remainingBytes, remainingBlocks, maxFragmentBytes int) []decodedBlock {
	if remainingBytes <= 0 || remainingBlocks <= 0 {
		return nil
	}
	candidates := encodedCandidates(text, cfg, maxFragmentBytes)
	decoded := make([]decodedBlock, 0, min(remainingBlocks, 4))
	for _, candidate := range candidates {
		if len(decoded) >= remainingBlocks || remainingBytes <= 0 {
			break
		}
		var value string
		var ok bool
		if candidate.kind == "hex" {
			if raw, err := hex.DecodeString(strings.TrimPrefix(strings.TrimPrefix(candidate.value, "0x"), "0X")); err == nil {
				value, ok = decodedPayloadText(raw, cfg.DecodeCompression, remainingBytes)
			}
		} else if candidate.kind == "base64" {
			for _, encoding := range []*base64.Encoding{base64.StdEncoding, base64.RawStdEncoding, base64.URLEncoding, base64.RawURLEncoding} {
				raw, err := encoding.DecodeString(candidate.value)
				if err != nil {
					continue
				}
				if value, ok = decodedPayloadText(raw, cfg.DecodeCompression, remainingBytes); ok {
					break
				}
			}
		}
		if !ok || len(value) > remainingBytes {
			continue
		}
		decoded = append(decoded, decodedBlock{start: candidate.start, end: candidate.end, value: value})
		remainingBytes -= len(value)
	}
	return decoded
}

func encodedCandidates(text string, cfg NormalizationConfig, maxFragmentBytes int) []encodedCandidate {
	candidates := make([]encodedCandidate, 0, 8)
	if cfg.DecodeHex {
		candidates = append(candidates, embeddedHexCandidates(text, maxFragmentBytes)...)
	}
	if cfg.DecodeBase64 {
		candidates = append(candidates, embeddedBase64Candidates(text, maxFragmentBytes)...)
	}
	sort.Slice(candidates, func(i, j int) bool {
		if candidates[i].start == candidates[j].start {
			return candidates[i].end < candidates[j].end
		}
		return candidates[i].start < candidates[j].start
	})
	return candidates
}

func embeddedBase64Candidates(text string, maxFragmentBytes int) []encodedCandidate {
	candidates := make([]encodedCandidate, 0, 4)
	for i := 0; i < len(text); {
		if !isASCIIBase64Data(text[i]) {
			i++
			continue
		}
		start := i
		for i < len(text) && isASCIIBase64Data(text[i]) {
			i++
		}
		for padding := 0; i < len(text) && text[i] == '=' && padding < 2; padding++ {
			i++
		}
		value := text[start:i]
		if len(value) < 12 || len(value) > maxFragmentBytes || !isBase64Candidate(value) {
			continue
		}
		candidates = append(candidates, encodedCandidate{start: start, end: i, value: value, kind: "base64"})
	}
	return candidates
}

func isASCIIBase64Data(value byte) bool {
	return (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z') || (value >= '0' && value <= '9') || value == '+' || value == '/' || value == '-' || value == '_'
}

func embeddedHexCandidates(text string, maxFragmentBytes int) []encodedCandidate {
	candidates := make([]encodedCandidate, 0, 4)
	for i := 0; i < len(text); {
		start := i
		digitStart := i
		if i+2 <= len(text) && text[i] == '0' && (text[i+1] == 'x' || text[i+1] == 'X') {
			digitStart = i + 2
			i += 2
		}
		if i >= len(text) || !isASCIIHex(text[i]) {
			i = start + 1
			continue
		}
		for i < len(text) && isASCIIHex(text[i]) {
			i++
		}
		digits := i - digitStart
		if digits < 16 || digits%2 != 0 || i-start > maxFragmentBytes {
			continue
		}
		if start > 0 && isEncodedIdentifierByte(text[start-1]) {
			continue
		}
		if i < len(text) && isEncodedIdentifierByte(text[i]) {
			continue
		}
		candidates = append(candidates, encodedCandidate{start: start, end: i, value: text[start:i], kind: "hex"})
	}
	return candidates
}

func isASCIIHex(value byte) bool {
	return (value >= '0' && value <= '9') || (value >= 'a' && value <= 'f') || (value >= 'A' && value <= 'F')
}

func isEncodedIdentifierByte(value byte) bool {
	return (value >= '0' && value <= '9') || (value >= 'a' && value <= 'z') || (value >= 'A' && value <= 'Z') || value == '_'
}

func overlapsEncodedCandidate(candidates []encodedCandidate, start, end int) bool {
	for _, candidate := range candidates {
		if start < candidate.end && end > candidate.start {
			return true
		}
	}
	return false
}

func replaceDecodedBlocks(source string, blocks []decodedBlock) string {
	var output strings.Builder
	last := 0
	for _, block := range blocks {
		if block.start < last || block.start < 0 || block.end > len(source) || block.end <= block.start {
			continue
		}
		output.WriteString(source[last:block.start])
		output.WriteString(block.value)
		last = block.end
	}
	output.WriteString(source[last:])
	return output.String()
}

func trimEncodedField(field string) string {
	field = strings.Trim(field, "\"'`()[]{}<>,.;")
	lower := strings.ToLower(field)
	for _, prefix := range []string{"base64:", "base64=", "b64:", "b64=", "hex:", "hex=", "gzip:", "gzip=", "zlib:", "zlib="} {
		if strings.HasPrefix(lower, prefix) {
			return strings.Trim(field[len(prefix):], "\"'`()[]{}<>,.;")
		}
	}
	return field
}

func isHexCandidate(value string) bool {
	value = strings.TrimPrefix(strings.TrimPrefix(value, "0x"), "0X")
	if len(value) < 16 || len(value)%2 != 0 {
		return false
	}
	for _, r := range value {
		if !((r >= '0' && r <= '9') || (r >= 'a' && r <= 'f') || (r >= 'A' && r <= 'F')) {
			return false
		}
	}
	return true
}

func isBase64Candidate(value string) bool {
	if len(value) < 12 {
		return false
	}
	padding := false
	for _, r := range value {
		switch {
		case r >= 'a' && r <= 'z', r >= 'A' && r <= 'Z', r >= '0' && r <= '9', r == '+', r == '/', r == '-', r == '_':
			if padding {
				return false
			}
		case r == '=':
			padding = true
		default:
			return false
		}
	}
	return true
}

func decodedPayloadText(raw []byte, allowCompression bool, limit int) (string, bool) {
	if len(raw) == 0 || limit <= 0 {
		return "", false
	}
	if allowCompression {
		if decompressed, ok := decompressSmallPayload(raw, limit); ok {
			return decompressed, true
		}
	}
	if len(raw) > limit || !utf8.Valid(raw) || !mostlyPrintable(string(raw)) {
		return "", false
	}
	return string(raw), true
}

func decompressSmallPayload(raw []byte, limit int) (string, bool) {
	if len(raw) < 2 || limit <= 0 {
		return "", false
	}
	var (
		reader io.ReadCloser
		err    error
	)
	if raw[0] == 0x1f && raw[1] == 0x8b {
		reader, err = gzip.NewReader(bytes.NewReader(raw))
	} else if raw[0]&0x0f == 8 && ((int(raw[0])<<8)|int(raw[1]))%31 == 0 {
		reader, err = zlib.NewReader(bytes.NewReader(raw))
	} else {
		return "", false
	}
	if err != nil {
		return "", false
	}
	defer reader.Close()
	decoded, err := io.ReadAll(io.LimitReader(reader, int64(limit)+1))
	if err != nil || len(decoded) > limit || !utf8.Valid(decoded) || !mostlyPrintable(string(decoded)) {
		return "", false
	}
	return string(decoded), true
}

func decodeROT13Text(text string) (string, bool) {
	letters := 0
	decoded := strings.Map(func(r rune) rune {
		switch {
		case r >= 'a' && r <= 'z':
			letters++
			return 'a' + (r-'a'+13)%26
		case r >= 'A' && r <= 'Z':
			letters++
			return 'A' + (r-'A'+13)%26
		default:
			return r
		}
	}, text)
	return decoded, letters >= 8 && decoded != text
}

func decodeEscapedText(text string) (string, bool) {
	if !strings.Contains(text, `\`) {
		return text, false
	}
	var out strings.Builder
	out.Grow(len(text))
	changed := false
	for i := 0; i < len(text); {
		if text[i] != '\\' || i+1 >= len(text) {
			out.WriteByte(text[i])
			i++
			continue
		}
		next := text[i+1]
		switch next {
		case 'u', 'U', 'x':
			digits := 4
			if next == 'U' {
				digits = 8
			} else if next == 'x' {
				digits = 2
			}
			end := i + 2 + digits
			if end <= len(text) {
				value, err := strconv.ParseUint(text[i+2:end], 16, 32)
				r := rune(value)
				if err == nil && utf8.ValidRune(r) && !(r >= 0xd800 && r <= 0xdfff) {
					out.WriteRune(r)
					i = end
					changed = true
					continue
				}
			}
		case 'n', 'r', 't', 'b', 'f', '\\', '/', '"':
			replacements := map[byte]byte{'n': '\n', 'r': '\r', 't': '\t', 'b': '\b', 'f': '\f', '\\': '\\', '/': '/', '"': '"'}
			out.WriteByte(replacements[next])
			i += 2
			changed = true
			continue
		}
		out.WriteByte(text[i])
		i++
	}
	return out.String(), changed
}

func stripInvisible(text string) string {
	return strings.Map(func(r rune) rune {
		switch r {
		case '\u200b', '\u200c', '\u200d', '\u2060', '\ufeff':
			return -1
		}
		if unicode.Is(unicode.Bidi_Control, r) {
			return -1
		}
		if mapped, ok := commonHomoglyphs[r]; ok {
			return mapped
		}
		return r
	}, text)
}

var commonHomoglyphs = map[rune]rune{
	'а': 'a', 'е': 'e', 'о': 'o', 'р': 'p', 'с': 'c', 'х': 'x', 'у': 'y', 'і': 'i', 'ј': 'j',
	'Α': 'a', 'Β': 'b', 'Ε': 'e', 'Ζ': 'z', 'Η': 'h', 'Ι': 'i', 'Κ': 'k', 'Μ': 'm', 'Ν': 'n', 'Ο': 'o', 'Ρ': 'p', 'Τ': 't', 'Χ': 'x',
}

func compactForScan(text string) string {
	return strings.Map(func(r rune) rune {
		if unicode.IsLetter(r) || unicode.IsNumber(r) {
			return unicode.ToLower(r)
		}
		return -1
	}, text)
}

func mostlyPrintable(text string) bool {
	if text == "" {
		return false
	}
	printable, total := 0, 0
	for _, r := range text {
		total++
		if unicode.IsPrint(r) || unicode.IsSpace(r) {
			printable++
		}
	}
	return total > 0 && printable*100/total >= 85
}
