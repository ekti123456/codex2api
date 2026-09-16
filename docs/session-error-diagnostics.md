# 会话归属、窗口等待与轮次身份错误诊断

适用于新产生的错误记录；旧日志无法补录当时没有采集的证据。仅补充诊断和报错文案，错误码、状态码、fork 路由、窗口等待预算和 UUIDv7 映射校验保持原有行为。

## window_owner_unavailable

`window_control.owner_lookups` 最多包含当前根和 fork 父会话两次查找：

- `target`：`current_root` 或 `fork_parent`。
- `identity_source`：签名携带的原始入站根 / fork 父指纹，不是账号映射后的出站 UUID。
- `root_hash`：查找目标的指纹摘要；可与对应会话自己的 `window_control.root_hash` 对照。
- `scope_hash`：包含 API Key 隔离的实际查找键摘要。同一父根的 root_hash 相同而 scope_hash 不同，表示查找范围不同。
- `persistent`、`live`：各阶段的 `found`、`missing`、`failed`、`not_checked`。
- `result`、`error_kind`：区分绑定缺失、读取失败；错误分类只记录 `timeout`、`canceled`、`storage_error`，不输出原始数据库错误。
- `account_id`：找到的内部账号编号。

父绑定缺失时提示“无法恢复 fork 父会话的账号绑定”，读取出错时提示“读取 fork 父会话账号归属失败”。`counts_evaluated: false` 表示还没有判断容量，不能把默认的使用数量当成容量检查结果。

这条链路收到的是父指纹，不能从摘要还原父线程 UUID，也不凭摘要证明父线程一定等于根会话。子线程到主根的追溯行为没有在此改动中调整。

## codex_root_account_wait_timeout

`background_window_wait` 增加：

- `waiting_for`：`ownership_validation`、`grant_confirmation`、`account_window`、`account_match_validation`；成功后为 `none`。
- `reason`：例如 `account_window_timeout`，或进入窗口等待时共享预算已经耗尽的 `shared_wait_budget_exhausted`。
- `budget_remaining_ms`、`deadline_at`：进入窗口等待阶段时剩余的实际预算与截止时间，并非每阶段另加 60 秒。
- `owner_last_seen`、`owner_last_completed`：持久归属记录中的活动时间；不等价于窗口仍有效。
- `grant_state`：是否需要授权，以及当前是否已确认。
- `initial_window`、`final_window`：等待开始与返回时的只读本地快照。

快照包括账号是否存在、容量开关、目标槽位 active / expired / missing、槽位最后活动和到期时间、扩容保留标记、待升级标记、账号当前本地槽位数，以及本地会话绑定状态。`scope: local` 明确表示本地观测；missing 不证明记录从未存在，也不证明共享缓存无数据或账号容量已满。

只在等待边界各读取一次目标账号与键，不额外扫描日志、不新增数据库或远程缓存查询、不续期或清理槽位。

## codex_session_identity_unavailable（轮次 UUID 校验）

`upstream.outbound_identity.account_mapping.invalid_turn_identity` 增加：

- `sources`：失败值出现的 `client_metadata.turn_id`、嵌套 `x-codex-turn-metadata` 或 `headers.X-Codex-Turn-Metadata` 中的字段位置。
- `reason`：`invalid_uuid`、`unsupported_uuid_version`、`unsupported_uuid_variant`。
- `uuid_version`、`uuid_variant`：能解析时记录实际版本与变体。
- `expected`：`UUIDv7/RFC4122`。
- `value_hash`、`value_length`：规范化后失败字符串的不可逆摘要和字节长度，不写入原始值或前缀。
- `stage: normalized_pre_mapping`：这些是元数据规范化后、出站映射前的检查位置，不应把复制后的多个位置误认为多个独立输入来源。

报错文本指出字段与问题类型，例如某字段为 UUIDv4。不会回显异常值，避免把被误放进 ID 字段的凭据写进错误消息。一次报告首先遇到的失败值。
