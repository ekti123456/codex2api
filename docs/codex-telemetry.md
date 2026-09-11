# Codex 客户端遥测

集成上游 PR [#666](https://github.com/james-6-23/codex2api/pull/666) 及后续修补 `c91eea6`，保留本分支的出站会话与 WS 握手隔离方案。

## 开关与默认行为

**实验性功能，默认关闭。** 新数据库及首次迁移新增字段的旧数据库均默认为关闭；管理员已经保存的设置保留。入口为「系统设置 → Codex → 客户端遥测」，持久化字段为 `codex_telemetry_enabled`。

`CODEX_TELEMETRY_ENABLED=false` 可在部署层强制关闭，优先于管理后台。设为 `true` 不会绕过后台关闭设置。`CODEX_STATSIG_API_KEY` 可覆盖内置公开 SDK key，只在遥测开启时使用。

关闭时不创建遥测任务，待发任务不再发送；关闭前已经发出的网络请求无法撤回。重新开启不会补发关闭前积压的事件或旧回合的终态。

## 出站关闭状态

Codex 官方账号的普通 HTTP `/responses`、`/responses/compact` 和 WebSocket `response.create` 均在 `client_metadata["x-codex-turn-metadata"]` 的 JSON 对象中写入布尔值 `analytics_enabled`。如果这个载体原来是 JSON 字符串，继续保持字符串；不是在请求顶层新增同名字段。

关闭时强制写 `false`，即使入站或账号自定义头写 `true`；开启时写 `true`，但尊重入站 turn metadata 明确的 `false`，该请求不产生模拟遥测。HTTP 兼容头和默认 preserve 模式的 WS 建连快照与正文保持同一开关值。WS 池把此开关纳入握手配置：切换状态后不会复用不兼容的旧握手；需要原连接的续链仍遵守续链保护，不会偷偷换账号重发。

例：`X-Codex-Turn-Metadata` 的 JSON 内容为：

```json
{
  "session_id": "原会话 ID",
  "thread_id": "原线程 ID",
  "analytics_enabled": false
}
```

其他元数据、父子关系和压缩密文不因遥测开关改写。最终出站诊断保留这个布尔字段，便于分别核实 HTTP 头、WS 实际握手与当前帧体。该值表示网关的出站策略，不证明客户端本地真的关闭了 analytics、feedback 或 OTel。

第三方 Responses 中转、Grok 等独立执行链不注入此字段、不发送这套遥测。压缩、生图和 Agent Identity 不发送模拟遥测；Codex 官方执行链仍带出站开关状态。

## 开启后发送的内容

分析事件发送到 `https://chatgpt.com/backend-api/codex/analytics-events/events`，OTLP 指标发送到 `https://ab.chatgpt.com/otlp/v1/metrics`。异步队列容量 1024，4 个 worker，单次超时 10 秒；队列满时丢弃并限频记录日志，失败不影响代理响应。

分析请求使用最终出站账号认证、UA、Originator 和版本，会话/线程标识取最终正文，不再按旧收敛算法重新派生。沿用账号代理或本次代理覆盖，Resin 开启时沿用反代。禁止跟随遥测响应重定向，避免认证头泄露到重定向目标。

保留上游的模拟事件：首次线程初始化、标题/guardian 初始化、每轮 turn 事件和 4 个 hook 事件；动态工具调用每轮随机 40%，命中后其中 50% 同时生成命令执行事件；文件修改每轮随机 20%，并附带 accepted-line-fingerprints，`repo_hash=null`。这些事件不是对真实工具、命令、文件修改的完整观测，不应拿来作为实际行为审计，也不能保证与上游看到的请求一致。

原生 `turn/steer` RPC 无法从 Responses 可靠推断，不模拟 steer 事件；不把历史 assistant/tool 内容或 `previous_response_id` 推断为 resumed。指标首次发送 62 个启动名称，随后每 60 秒发送增量，合计覆盖 66 个名称。遥测不会上传原始 prompt、工具正文或 diff；部分统计和行为字段为合成值。
