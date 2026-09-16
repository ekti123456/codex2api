# 切号时完整保留 input（实验）

系统设置中的 `codex_session_failover_preserve_input` 默认关闭，位于账号切换选项下方。开启后，仅在允许账号切换并发生迁移时创建完整输入保留段；单独开启不会触发换号。

## 请求行为

- 保留客户端 input 的完整 JSON 内容，包括加密 reasoning、compaction、输入项 ID、文件引用、工具调用和动态工具定义。传输编码和空白可以改变，但 JSON 值、数组顺序及大整数不变。
- 仍更换账号认证信息、设备/会话出站身份，窗口按既有机制重建。旧 `x-codex-turn-state` 在头和 client_metadata 中清理；当前账号、当前段已验证的新值可继续使用。
- 未验证的 `previous_response_id`、`conversation` 或 `conversation_id` 会在换号前被拒绝，即使同时存在 input。网关不能证明这些 input 已包含完整历史，需客户端提供完整回放并移除旧续写引用。已验证属于当前段的 previous_response_id 仍可用于后续增量请求。
- 工具结果缺少对应调用时停止发送，不删除孤立结果来凑出可发送请求。只有文本、且没有显式续写引用的输入，无法自动判断是否省略了历史；不承诺语义完整。
- 请求协议转换后恢复原 input，再应用 payload 规则；最终出站校验阻止其他步骤修改 input。与修改 input 的 payload 规则冲突时明确报错。
- 不改写 input 中的环境日期和时区，不使用拒绝密文记忆清理，不自动删除无效加密内容重试，不自动压缩溢出输入或执行续想折叠。上游是否接受跨账号加密内容仍由真实上游决定。
- 不根据旧 compaction 来源重新选回原账号，继续使用既有根会话/出站段归属和分组、标签、权限限制。

## 持久化与诊断

设置支持 SQLite/PostgreSQL，旧库迁移默认关闭。会话记录的 `preserve_restart_input` 随账号迁移事务保存。该段后续请求和再次迁移继续保留 input；关闭全局开关只影响尚未采用该策略的会话，不会静默将已有保留会话降级为有损清理。若需重新使用默认策略，可关闭开关后新建会话。

原有 `lossy_context_restart` 是历史内部流程标记，新模式仍通过该重建入口进行身份/状态处理；实际清理模式以 `context_cleanup.mode: preserve_input` 和 `preserve_restart_input` 为准。诊断继续记录 `tools_before`、`tools_after`、`tool_preservation`，不记录工具正文或加密原文。

已知历史缺失或上游拒绝时，返回明确错误。完整保留 input 不代表补回客户端未发送的工具和历史，也不保证切号后模型不会失忆。
