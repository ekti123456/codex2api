# 切号时完整保留 input（实验）

系统设置中的 `codex_session_failover_preserve_input` 默认关闭，位于账号切换选项下方。开启后，仅在允许账号切换并发生迁移时创建完整输入保留段；单独开启不会触发换号。

## 请求行为

- 保留客户端 input 的完整 JSON 内容，包括加密 reasoning、compaction、输入项 ID、文件引用、工具调用和动态工具定义。传输编码和空白可以改变，但 JSON 值、数组顺序及大整数不变。
- 仍更换账号认证信息、设备/会话出站身份，窗口按既有机制重建。旧 `x-codex-turn-state` 在头和 client_metadata 中清理；当前账号、当前段已验证的新值可继续使用。
- 未验证的 `previous_response_id`、`conversation` 或 `conversation_id` 会在换号前被拒绝，即使同时存在 input。网关不能证明这些 input 已包含完整历史，需客户端提供完整回放并移除旧续写引用。已验证属于当前段的 previous_response_id 仍可用于后续增量请求。
- 带有 `call_id` 的工具结果缺少对应调用时停止发送，不删除孤立结果来凑出可发送请求。官方 Codex 的 `function_call_output` 可以省略 `call_id` 或设为 `null`，表示通知、跨任务消息等独立消息，原样保留且不要求配对；空字符串、错误字段类型及其他工具输出类型不适用此豁免。只有文本、且没有显式续写引用的输入，无法自动判断是否省略了历史；不承诺语义完整。
- 请求协议转换后恢复原 input，再应用 payload 规则；最终出站校验阻止其他步骤修改 input。与修改 input 的 payload 规则冲突时明确报错。
- 不改写 input 中的环境日期和时区，不使用拒绝密文记忆清理，不自动删除无效加密内容重试，不自动压缩溢出输入或执行续想折叠。上游是否接受跨账号加密内容仍由真实上游决定。
- 不根据旧 compaction 来源重新选回原账号，继续使用既有根会话/出站段归属和分组、权限限制；账号标签不参与切号匹配。

## 持久化与诊断

设置支持 SQLite/PostgreSQL，旧库迁移默认关闭。会话记录的 `preserve_restart_input` 随账号迁移事务保存。该段后续请求和再次迁移继续保留 input；关闭全局开关只影响尚未采用该策略的会话，不会静默将已有保留会话降级为有损清理。若需重新使用默认策略，可关闭开关后新建会话。

原有 `lossy_context_restart` 是历史内部流程标记，新模式仍通过该重建入口进行身份/状态处理；实际清理模式以 `context_cleanup.mode: preserve_input` 和 `preserve_restart_input` 为准。诊断继续记录 `tools_before`、`tools_after`、`tool_preservation`，不记录工具正文或加密原文。

已知历史缺失或上游拒绝时，返回明确错误。完整保留 input 不代表补回客户端未发送的工具和历史，也不保证切号后模型不会失忆。

### 工具结果配对失败

校验失败时，使用日志和错误日志的 `account_failover.block_reason` 为 `incomplete_tool_context`，`trigger_reason` 仍保留最初切号原因。`phase=after_switch`、账号和代次来自已有绑定，表示切号后的续接校验，不代表本次又切了一次账号。HTTP 400、原错误码、原提示和停止重试语义保持不变。

`context_cleanup.tool_pairing` 记录：

- `scope=input_top_level`：只检查本次 input 顶层协议项，不扫描工具结果正文，也不查询旧请求历史。
- `input_items`、`call_items`、`output_items`：输入项、调用项、结果项数量。调用项沿用 `_call` 后缀识别，结果包括 `_call_output` 和 `tool_search_output`。
- `missing_call_count`：找不到配对调用的结果项总数，重复结果分别计数；合法独立 `function_call_output` 不计入失败数。
- `missing_calls`：前 8 条失败项，包含从 0 开始的 `index`、字段 `path`、结果 `item_type`、参考调用类型 `expected_call_type`、原始 `call_id` 和 `call_id_state`。状态区分 `present`、`missing`、`null`、`empty`、`non_string`；参考类型用于定位，不新增类型匹配限制。
- `omitted_items`：未展开的失败项数；使用日志超出总大小预算时也会压缩明细，并设置外层 `truncated`。保留总数和首条明细。
- `value_truncated`：单项字段超出长度限制，`call_id` 最多保留 128 字节，类型最多 64 字节；普通标识原样保留以便核对。

例如 `missing_calls[0]` 为 `{"index":12,"path":"input[12].call_id","item_type":"function_call_output","expected_call_type":"function_call","call_id":"call_abc","call_id_state":"present"}`，表示本次第 13 个输入项的结果找不到可配对调用；不能据此断定调用由客户端还是中间层丢失。

失败时仍记录 `tools_before`，方便区分“工具定义缺失”和“历史调用缺失”。不记录参数、执行输出或加密内容，不改动 input，不补造调用，也不删除结果。已验证的当前段 `previous_response_id` 仍按原规则接受增量输入；服务端工具搜索结果的既有豁免保持不变。旧日志不能补出当时未采集的配对明细。

独立函数消息的规则也用于普通上下文校验、默认有损重建、HTTP/WS 请求转换和依赖缓存的判断，避免其他入口再次误拦、误删或改写为用户消息。默认模式已有的密文/账号引用清理继续生效。已保留该消息的旧会话升级后重试即可使用新规则；已经删除且客户端未重发的历史内容无法凭空恢复。
