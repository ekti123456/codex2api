# Codex 出站身份与 WS 握手

## 默认行为

Codex HTTP `/responses`、HTTP `/responses/compact` 和 WS `response.create` 默认使用 `CODEX_OUTBOUND_SESSION_MODE=preserve`。合法的当前请求身份保持原值：

| 载体 | 出站结果 |
| --- | --- |
| 入站 session_id=S、thread_id=T | 本地权限、签名和账号粘性仍使用入站身份 |
| HTTP / WS Session-Id、Thread-Id | S、T；不再拿隔离缓存键充当 Session-Id |
| 正文 client_metadata 及其内嵌元数据 | 保留 S、T 及原有对象 / JSON 字符串载体类型 |
| 父线程、fork 来源、窗口前缀及序号、逐轮 ID | 保留原有关系，不把子线程折叠成父线程 |
| prompt_cache_key | 独立缓存分区；有已验证 NewAPI 用户时额外按用户隔离 |

当头是旧 WS 升级快照、正文带新一帧的规范元数据时，以当前帧为准。既有平铺投影按规范元数据归一；不从缓存键、scope_hash、thread_id 或 window_number 推算缺失的父会话。不将 window_id:0 当作第一轮的可靠证据。

preserve 模式优先于旧 session/full 会话收敛及会话头形态开关：只保留这些档位的设备级处理，不重写 session/thread/window/lineage。off 仍不改设备；device 仍按账号收敛设备。UA、环境处理、项目字段清理照旧。会话、线程和任务头不允许账号自定义头覆盖成与正文矛盾的值。设备头与设备元数据继续用同一份指纹快照。

不修改 input、压缩项 id、encrypted_content、工具输出和不透明续链令牌。HTTP 已有的 previous_response_id 展开 / 清理机制不变。Responses 中转及其他提供商不使用此 Codex 身份改写。

## WS 三层处理

| 类别 | 处理 |
| --- | --- |
| Session-Id、Thread-Id、X-Client-Request-Id | 发送真实当前身份，纳入连接配置匹配；身份变化不能复用不兼容连接 |
| X-Codex-Window-Id、X-Codex-Turn-Metadata | 建连时发送当前快照；后续窗口、轮次随每帧更新，不因序号推进就重连 |
| 父线程、subagent、memgen、线程来源 | 保留真实任务标记并参与配置匹配，避免普通请求继承后台任务连接 |
| X-Codex-Turn-State | 仅放当前帧，不固化到握手 |
| 工具清单及较大元数据 | 完整内容留在帧；握手删除工具清单，超过 8 KiB 时仅保留有界身份 / 轮次字段 |

设备身份改变也会触发配置不兼容，不把旧设备握手与新设备正文混用。窗口 / turn 的握手快照与后续帧不同是正常的，不等于会话身份不一致。

busy 同会话溢出功能保留，只使用同账号、同隔离分区及兼容握手配置的兄弟连接，本地 #ovf 后缀不写入 session_id。已知连接本地 previous_response_id 仍必须使用原连接；连接丢失、占用或配置不兼容时停止并提示恢复完整上下文，不自动发往兄弟连接重放。

参考官方客户端 `rust-v0.154.0` 的 [握手构建](https://github.com/openai/codex/blob/rust-v0.154.0/codex-rs/core/src/client.rs) 和 [元数据投影](https://github.com/openai/codex/blob/rust-v0.154.0/codex-rs/core/src/responses_metadata.rs)。这不意味着网关所有字段与官方字节级一致，也不保证消除上游 500。

## 隔离与冲突保护

- WS 池继续包含已验证归属、下游连接、账号、原始线程、URL、出口和握手配置维度。内部键不写回正文。
- 响应上下文缓存与 response_id 账号归属额外区分共享 API Key 下的已验证用户；拒绝读取旧的共享 Key 缓存作为兜底。加密内容拒绝记忆也区分用户。
- 相同上游账号下，session/thread/父线程/fork 身份的归属使用 SHA-256 摘要落库到 `codex_identity_claims`；同一已验证用户重复使用允许，其他用户碰撞直接报 `codex_session_identity_conflict`（400，不重试、不换号）。重复导入相同 ChatGPT 账号也共享冲突约束。
- 归属登记事务化，无 TTL；服务重启及共享同一数据库的多实例仍有效。数据库异常时拒绝发送，不退化为随机改 ID。只保存归属和身份摘要，不存认证凭据、原始 ID 或正文。
- 无已验证 NewAPI 用户时只能识别已认证下游凭据，不能声称区分同一凭据背后的不同真人。无数据库的嵌入式 Handler 使用有界进程内表，满时拒绝新登记，不驱逐旧归属；它不具备跨进程持久性。直接调用底层执行器须由宿主提供归属上下文，管理员账号测试独立生成诊断会话。

## 部署与回退

- `preserve` 默认；之前未发布的 `aligned` 名称及未知值也按 preserve 处理。
- `observe`、`legacy` / `off` 显式回到旧出站会话与握手策略；只用于兼容回退，不保证头体一致。新增用户缓存隔离不随回退撤销。
- 现有账号绑定、黑名单不迁移、不清空。新归属表从启用后的请求开始登记，不能追溯此前未知的跨用户冲突。
- 部署会改变旧会话的出站握手身份与用户缓存命名空间。旧连接本地上下文不能迁移；部分 previous_response_id 续链可能需要客户端补全上下文或新建会话。不要在两种模式实例间交替路由同一续链；建议在维护窗口部署或按会话灰度。
- 不会因为迁移失败而绕过永久绑定自动换号。

## JSON 日志

“最终出站身份”默认展示脱敏 JSON，`format_version=2` 按实际字段位置保存：

```json
{
  "format_version": 2,
  "session_consistency": "matched",
  "ws_handshake": {
    "headers": {
      "Session-Id": "S",
      "Thread-Id": "T",
      "X-Codex-Turn-Metadata": "{\"session_id\":\"S\",\"thread_id\":\"T\",\"window_number\":0}"
    }
  },
  "body": {
    "client_metadata": {
      "session_id": "S",
      "thread_id": "T",
      "x-codex-turn-metadata": "{\"session_id\":\"S\",\"thread_id\":\"T\",\"window_number\":1}"
    },
    "prompt_cache_key": "hash:..."
  }
}
```

以上 S/T 为说明用占位；日志中非标准标识仍遵循既有脱敏规则。它是字段白名单快照，不是完整请求抓包；不会记录 prompt、Cookie、Authorization、密文或 turn state 明文。JSON 字符串默认仍为字符串，可勾选“解析元数据”便于阅读，此时展示及复制是解码视图而非原始字段类型。

旧日志的 `body.turn_metadata` 和 `body.links` 是历史解析分组，界面明确标为历史摘要，不伪装成真实上游层级。新日志改为 `body.client_metadata["x-codex-turn-metadata"]` 和 `body.prompt_cache_key`。

`session_consistency` 只比较实际捕获的会话字段：matched / mismatched / missing_header / missing_body；不等于请求成功。WS 复用展示实际建连时握手，而不是把本次期望头冒充已经重发的握手。
