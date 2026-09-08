# 使用日志请求诊断

使用统计的“请求类型”列展示每条新日志的网关判定，点击读取该日志的诊断详情。桌面表格和移动端卡片均支持，列可隐藏。详情仅开放给管理员，可以复制脱敏 JSON。

## 分类与证据

| 类型 | 含义 |
| --- | --- |
| `user` | 已解析用户根，来源为 user 或未指定 |
| `related_internal` | 调度时允许被动调用，且身份关联到根 |
| `independent_internal` | 调度时允许被动调用，但没有关联到用户根 |
| `related_unclassified` | 关联请求，没有取得被动授权 |
| `compaction` | 压缩端点或已解析的 compaction 请求 |
| `gateway_internal` | 网关自身标记了 internal_reason 的请求 |
| `unknown` | 证据不足，不能确定类型 |

这是网关实际判定，不是客户端身份认证。UA、指纹、模型名称或提示词不用于诊断层重新推断类型。旧日志显示“未记录”，不回填猜测值；无请求上下文的新日志标记 `capture_status=not_available`。

`incoming` 分别保存允许记录的 HTTP 头、turn metadata、client_metadata（包含有界嵌套来源）、已验证 NewAPI 元数据。缺失、非字符串类型和过大的元数据有明确标记；别名分开保留，避免掩盖冲突。WS 使用已解包的单帧请求元数据，握手头仍单独展示。

`resolved` 保存身份解析结果，包括来源、根状态、关联关系、是否持有用户根、根 ID/指纹、窗口/亲和键摘要、被动授权、窗口豁免、是否进入无根调度、是否要求主根账号。`audit` 和 `dispatch` 分别记录内容审核和模型调度阶段的被动授权及模型豁免开关；未执行阶段不伪造结果。`passive_models_allowed` 表示具备豁免资格，不表示该账号原来的模型列表一定不允许此模型。阶段判定变化时显示警告。

## 选号与窗口

- `root_account_lookup`：`not_checked` 未走到已有查询，`missing` 查询未找到，`found` 查询得到账号。仅复用已有查询结果，不新增查询。
- `selection`：`matches_observed_root` 最终账号与曾观察到的根/亲和账号相同（不单独证明强制锁号）；`recent_account` 命中最近账号；`unlinked_scheduling` 无根普通调度；`scheduled` 其他正常调度；`no_account` 未选中账号。
- `recent_account.result`：`not_attempted` 未尝试；`not_applicable` 不满足条件；`missing` 无记录；`cache_error` 读取失败；`invalid_record` 记录无效；`expired` 超出回溯时间；`after_request_start` 候选记录不早于本次请求；`account_unavailable` 候选未通过现有调度检查；`selected` 已取到候选。
- `candidate_rejections` 是现有选号 trace 收集的本轮候选排除原因，可能包含多个账号的结果，不应全部归因于最终账号；空列表不证明候选都可用。
- `user_window`：`not_reached` 未进入检查；`disabled` 用户限制未启用；`account_windows_disabled` 所选账号窗口未启用；`passive_exempt` 现有分支豁免；`identity_missing` 缺少稳定身份/主体；`identity_conflict` 身份冲突拒绝；`reused` 复用已有窗口；`related_no_new_window` 关联请求不创建新窗口；`created` 本次创建；`cooldown_rejected` 创建 CD 拒绝；`limit_rejected` 数量上限拒绝。
- `account_window` 是所选账号的窗口准入快照：`disabled` 未启用；`rejected` 用户侧准入拒绝；`exempt_or_unstable` 不具备可计数身份或被豁免；`root_reused` 关联请求沿用根；`reused` 之前已有该账号绑定；`admitted` 通过本次准入。它不是事后重新查询的最终窗口持久化结果。

请求保存开始/完成时间（UTC）、请求关联 ID、已验证 NewAPI 请求 ID、重试序号。每次重试清除上一轮最近账号和准入结果；每个 WS 帧清除上一帧诊断，日志落库的是不可变 JSON 快照。日志原有 `created_at` 是写日志时间，不替代请求开始时间。

## 性能与隐私

- 数据库仅新增 `request_type`、`request_diagnostics` 两列；SQLite/PostgreSQL 均为兼容旧数据的增量迁移，无历史回填、额外索引或关联查询。
- 常规列表只 SELECT 轻量 `request_type`，不读取完整诊断。点击后 `GET /api/admin/usage/logs/:id/diagnostics` 按已有主键读取一条，查询超时 2 秒；关闭/切换弹窗取消旧前端请求，不轮询诊断。
- 请求链路只取已有解析/授权/缓存/调度结果，不为诊断再次查根、查窗口、扫描账号或访问数据库。
- 入口元数据只提取一次，不反序列化整个请求正文。单个元数据对象上限 16 KiB，嵌套和来源数量有界；完整落库快照上限 12 KiB，超限优先删除入口字段并标记截断。正文大小只影响定位 client_metadata 的扫描耗时，不随正文大小保留诊断副本。
- 沿用已有日志批量缓冲写入及 full/errors/off 模式。不需要保存的请求跳过诊断 JSON 序列化；不会改变用量计费、审核或选路。
- 不保存 Authorization、Cookie、API Key、完整请求头或提示词。标准 UUID/窗口 UUID 保留用于关联，自定义身份只保存摘要；签名 NewAPI 字段也只取固定白名单。

验证命令：`go test ./proxy ./database ./admin -run '^TestUsageRequestDiagnostics'`；性能基准：`go test ./proxy -run '^$' -bench '^BenchmarkUsageRequestDiagnostics$' -benchmem`。

本机 AMD Ryzen 7 5800H / Windows amd64 的微基准：32 KiB 正文约 23 μs/op，1 MiB 正文约 426 μs/op，均约 5 KiB、35 次分配/op。元数据刻意放在正文末尾，包含入口提取、决策快照和序列化，不包含原有路由或数据库 I/O；不是线上端到端延迟保证。
