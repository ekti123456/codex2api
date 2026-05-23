package database

import (
	"context"
	"time"
)

// APIKeyWindowUsage 表示一个 API Key 在某时间窗口内的累计使用量。
// 仅排除 499 客户端取消请求,保持与 GetUsageStats 一致的语义。
type APIKeyWindowUsage struct {
	Requests   int64   `json:"requests"`
	Tokens     int64   `json:"tokens"`
	UserBilled float64 `json:"user_billed"`
}

// GetAPIKeyWindowUsage 聚合指定 API Key 在 [now-window, now] 时间窗口内的使用情况。
// 用于 API Key 级别的滑动窗口限额校验(rpm/rpd/cost_5h/cost_7d/token_5h/token_7d)。
// 索引 idx_usage_logs_api_key_created_at 让该查询在数据量大时仍 O(log n)。
func (db *DB) GetAPIKeyWindowUsage(ctx context.Context, apiKeyID int64, window time.Duration) (*APIKeyWindowUsage, error) {
	if apiKeyID <= 0 || window <= 0 {
		return &APIKeyWindowUsage{}, nil
	}
	since := time.Now().Add(-window)
	usage := &APIKeyWindowUsage{}
	query := `
		SELECT
			COUNT(*),
			COALESCE(SUM(total_tokens), 0),
			COALESCE(SUM(user_billed), 0)
		FROM usage_logs
		WHERE api_key_id = $1
		  AND created_at >= $2
		  AND status_code <> 499
	`
	err := db.conn.QueryRowContext(ctx, query, apiKeyID, db.timeArg(since)).Scan(
		&usage.Requests, &usage.Tokens, &usage.UserBilled,
	)
	if err != nil {
		return nil, err
	}
	return usage, nil
}
