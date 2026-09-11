# Responses 生成与下游交付诊断

原生 Codex `/v1/responses` 的 HTTP/SSE 下游、HTTP 或 WebSocket 上游路径，在请求诊断的 `upstream.stream_delivery` 中记录终态与本地交付状态。其他协议转换路径未新增此段诊断，缺失不代表成功。

- `terminal_event`、`response_status`、`incomplete_reason`：最终进入转发回调的终态及生成状态。`incomplete` 不等于完整生成成功。
- `terminal_received_at_unix_ms`：转发回调观察到终态的时刻。
- `usage_source`：终态中是否存在上游 usage 对象，取值 `upstream` 或 `missing`，不是本地账单估算来源。
- `usage_received_after_cancel`：终态用量是否在已观察到下游请求取消后取得。
- `cancel_observed_at_unix_ms`：观察到取消的时刻，不保证是取消实际发生时间。
- `terminal_write`：`not_attempted`、`not_seen`、`accepted`、`buffered`、`failed`。
- `terminal_write_at_unix_ms`：本地写入调用／缓冲提交结果的记录时刻。
- `downstream_status`：`write_accepted`、`client_canceled`、`write_failed`、`buffered`、`terminal_not_written`。
- `write_error`：经过脱敏并限制长度的本地写回错误。

`accepted` 仅代表本地写入调用成功，不代表对端应用收到或消费了完成帧。连续重试的私有缓冲不能算作交付，只有提交到下游后才更新该状态。

现有“上游生成终态优先”的用量日志状态码和断线后最多五秒提取用量机制不变；请结合新增交付状态判断。该诊断不改变重试、连接池、账号粘性和收费策略，也不记录模型正文。

使用 NewAPI 的请求 ID 与 Codex2API `diagnostics.newapi_request_id` 对照。历史请求没有这些字段，无法据此恢复其最终帧或精确断线时序。
