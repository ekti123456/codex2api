# 压缩请求结构诊断

`request_kind=compaction` 是客户端用途标签，不等同于正文里的 `input[].type=compaction_trigger`。普通 `/responses` 不因标签自动注入触发器，避免把客户端摘要请求变成另一种协议。只有显式开启的旧 `/responses/compact` 转换路径可以补入触发器。

已有触发器仍归一到输入末尾、去重并规范化类型拼写，但保留该触发器的其他字段。多个触发器保留最后一个对象，不把不同触发器字段混合；非触发项顺序保持不变。

`compaction`、`context_compaction`、`compaction_summary` 历史项在请求预处理及响应上下文缓存回放中都保留 `id`，不透明加密内容继续保留。普通消息、reasoning、工具调用等项维持既有 ID 清理行为；已识别的可移植压缩包和明文摘要仍按既有兼容规则转换为 developer 消息。账号绑定、加密内容失效处理及重试规则不变。这是传输完整性修正，不代表已证明它导致上游 500。

## 查看位置

- `diagnostics.responses_input`：首次捕获的入站正文结构，在请求信息中展示。
- `diagnostics.upstream.responses_input`：本次出站尝试的结构，在上游诊断中展示。HTTP 在发送前采集编码前的最终 JSON，WS 在环境改写后、写业务帧前采集；发送状态仍以 `send_phase` 为准。WS 建连失败、未进入写帧时可能没有此项。
- 历史日志不能补采。统计结果只包含固定分类、字节数、计数和布尔值，不保存提示词、工具内容、摘要、加密载荷、凭据或原始响应 ID。

| 字段 | 含义 |
| --- | --- |
| `mode` | `protocol_trigger`：有直接输入级触发项；`compact_endpoint`：专用压缩端点；`metadata_only`：仅元数据标记压缩；`history_only`：只有压缩历史；`ordinary`：以上信号均未命中。按此顺序判定，不从提示词推断用途 |
| `json_bytes`、`input_kind`、`input_items` | JSON 字节数、input 类型及数组/对象项数；不是 token 数，也不是压缩后的网络字节数 |
| `metadata_compaction` | 正文规范元数据或请求头标记压缩，支持对象和 JSON 字符串 |
| `protocol_trigger_count`、`trigger_at_end` | 直接触发项数量及最后一项是否为触发器；不递归识别工具输出里的同名文本 |
| `compaction_items`、`encrypted_compaction_items`、`encrypted_compaction_bytes` | 压缩历史项数量、带非空加密内容的数量及载荷字节总数；不能证明加密内容有效 |
| `compaction_items_with_id` | 带 ID 的压缩历史项数量；可对比通用 ID 清理前后变化，不能单凭变化认定请求失败原因 |
| `previous_response_id_present` | 续链 ID 是否非空；不记录 ID 明文 |
| `summary_prefix_items`、`tool_output_placeholder_items` | 固定摘要前缀和工具输出占位文本的出现次数；客户端也可能提供相同文本，不等于证明是本网关生成 |

用同一请求的入站与出站结构对比，再结合上游错误码、请求 ID、连接信息和发送阶段定位问题。结构差异不是上游风控证据，也不能据此承诺消除 `server_is_overloaded`。
