# 回传字段与历史身份的一致性

回传隐私处理必须按 Responses 的协议位置区分身份信息与业务内容。共享的 `ResponseOpaquePayloadField` 同时供身份过滤、公开错误投影、WebSocket 流映射使用。不能因为工具结果恰好包含 `session_id`、`user` 或 `stream_id`，就把它当成网关身份元数据。

## 字段策略

| 位置 | 处理 | 原因 |
| --- | --- | --- |
| `program.code/fingerprint`、`program_output.result` | 按业务内容保留 | 代码、执行结果和不透明回放指纹需要完整回放 |
| 工具参数、输出、代码日志、搜索结果与 schema | 保留；WS 不递归检查其中的 `stream_id` | 业务字段不能触发协议分流、账号清理或 JSON 字符串过滤 |
| `mcp_call.error`、`mcp_list_tools.error` | 保留对象/字符串类型、错误类别与修正依据；过滤协议私有扩展、已知账号凭据和凭据格式 | 工具错误不使用请求级错误外壳；数值用 `json.Number` 保持精度 |
| 图像 `revised_prompt`、MCP 审批 `reason`、shell command added/done | 按相应 item/event 的业务字段保留 | 字符串可能是 JSON 或以花括号起始的代码 |
| `moderation.input/output` 错误 | 保持自身 union 结构；清理敏感控制字段 | 不重构成嵌套的请求级 `error` |
| 请求级错误 | 固定公开消息，保留已登记类型、参数路径和有效路由/顺序字段 | 嵌套 WS 错误和扁平 SSE 错误各保留其外壳；分类/重试仍读取原始上游错误 |
| `OpenAI-Model`、`X-Reasoning-Included` | 在最终使用的响应提交时转发；后一项保持“是否存在”的语义 | 供客户端判断实际模型与 reasoning token 是否已计入 |
| `X-Models-Etag` | 转换为刷新信号；目录刷新后与实际返回给该 API Key 的目录 ETag 对齐 | 网关筛选/合并后的模型目录可能与上游不同；计费使用量不参与目录作用域 |
| `conversation.id` | 下发独立 `conv_…` 别名；回带时验证并恢复官方句柄 | 保持会话继续能力，避免把上游原值直接下发 |
| 输出项 `internal_chat_message_metadata_passthrough.turn_id` | 只还原已核实的客户端轮次 | 历史轮次不能替换成当前轮；无法核实的可选 turn_id 省略，不猜测 |
| session/account/installation/request 等身份扩展 | 继续按既有策略清理 | 功能字段恢复不放开凭据和身份控制信息 |

## 历史 turn_id 的往返

第一轮客户端 `U1` 对应官方 `O1`，第二轮当前值 `U2` 对应 `O2`。第二轮回放第一轮的历史项时仍发送 `O1`，同时当前轮元数据使用 `O2`。返回第一轮的输出项恢复为 `U1`，当前轮恢复为 `U2`。

映射采集同时读取请求头、`client_metadata` 和 `input[].internal_chat_message_metadata_passthrough.turn_id`；仅触碰这一登记的历史路径。旧客户端历史中的本地字符串/计数器单独生成稳定的 UUIDv7 出站值，不降低当前轮传输元数据的 UUIDv7 校验要求。历史允许最多 4096 个不同轮次，当前控制元数据仍限制 32 项。

重复 item metadata/turn_id 按 JSON 最后一个值统一采集与发送，替换整个协议项，避免校验一个值而发出另一个。工具参数/输出内同名业务键不受影响。

`codex_protocol_ids` 保存双向关系，索引是带密钥的摘要，值使用已有持久化 AES-GCM 密钥加密。作用域包含用户、根会话、账号指纹和切号代数；A → B → A 不会复活旧代数映射。该表与持久化身份阶段具有相同生命周期，不使用 response_id 的短期过期策略。数据库和已有 `codex_turn_state_secret` 应一起备份；密钥缺失且仍有映射时启动失败。

压缩接口的正文不支持 `client_metadata`。处理器先把原始身份快照交给统一账号映射，再由最终 compact 序列化器删除正文载体；映射后的请求头和历史项因此保持一致。

## 兼容边界

- 未登记、无法核实的上游历史 turn_id 不会被当成当前轮或直接发送给用户；该可选字段省略，其他历史内容保留。
- 用户通过公开 Conversations 资源接口取得、并主动传入的原生 conversation 句柄仍按资源协议提交给上游；网关签发的别名必须匹配当前作用域才能恢复。不能将任意资源句柄改名后直接交给官方使用。
- 既有 preserve 身份策略沿用原规则；此修改不强制迁移老会话。
- 功能响应头只在 HTTP 头尚未提交时发布。若重试心跳已提交头，不能补写，也不会伪造未登记的 SSE 事件。已经升级的下游 WebSocket 同理不补写 HTTP 头。
- `/models` ETag 协调缓存是每个 Handler 的有界内存缓存；进程重启或跨副本可能多触发一次目录刷新，不用于授权或账号恢复。
- 业务输出本身不作全局字符串/同名字段替换；本策略不是任意生成文本的秘密检测器。

## 验证与参考

回归覆盖 18 类输出项的 JSON/item-done/completed/SSE 载体、HTTP SSE/JSON、compact、下游 WS、实际本地上游 WS 的默认流/命名流与错误事件、历史回放、重复字段、跨用户/账号/代数隔离、数据库重启/并发/冲突和目录 ETag。

未连接生产账号或执行真实模型质量基准。协议测试不能证明模型质量、缓存命中率或推理算力发生变化。

官方依据：[Codex item metadata](https://github.com/openai/codex/blob/434535bddfaf405a032f57be3c1096dd25ff6312/codex-rs/protocol/src/models.rs)、[Codex HTTP 响应头](https://github.com/openai/codex/blob/434535bddfaf405a032f57be3c1096dd25ff6312/codex-rs/codex-api/src/sse/responses.rs)、[程序化工具](https://developers.openai.com/api/docs/guides/tools-programmatic-tool-calling)、[MCP](https://developers.openai.com/api/docs/guides/tools-connectors-mcp)、[WebSocket](https://developers.openai.com/api/docs/guides/websocket-mode)、[官方 SSE 错误结构](https://github.com/openai/openai-python/blob/main/src/openai/types/responses/response_error_event.py)、[shell command 事件](https://github.com/openai/openai-python/blob/main/src/openai/types/responses/response_shell_call_command_done_event.py)。
