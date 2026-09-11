# Codex 客户端身份与独立连接测试

## 请求头覆盖范围

普通 HTTP、compact、WebSocket、standalone 搜索和 Live 共享客户端身份解析决策：UA 由既有兼容模式、配置或下游客户端决定，生成身份的 Originator 与客户端前缀一致，透传身份保留适用的下游 Originator。

原生 Codex 模型清单、额度查询、额度明细、重置券列表和重置券消耗使用同一套配置生成 UA、Version、Originator，不再单独使用内置平台信息。辅助请求只应用这三个账号自定义头，不把 Authorization、Cookie、会话头或普通请求的其他元数据复制到维护端点。原有账号空间选择、认证、请求体和 Accept 语义不变。

模型清单显式传入 client_version 时，保留该参数原有的版本选择语义，并同步 UA 的客户端版本段和 Version；最终查询参数与最终 Version 头一致。未指定版本时采用配置解析结果。账号显式自定义头仍有最高优先级，调用者应保持自定义三元组一致。

OAuth、订阅、邀请等浏览器/认证端点不属于上述统一范围；Responses 中转账号的模型发现与身份补充策略也不在这次修改范围内。

## 单次和批量测试

原生 Codex 账号的连接测试使用独立诊断请求：

- 每次测试新建 session_id 和 turn_id，thread_id 等于本次 session_id，window_id 为 `session_id:0`。
- prompt_cache_key 和传入执行器的会话键使用本次 session_id，避免复用其他测试或用户会话。
- 通过 client_metadata 和内嵌 x-codex-turn-metadata 提供一致的当前请求快照；request_kind 为普通协议请求 `turn`。
- 不携带 parent_thread_id、parent_turn_id、root_turn_id、forked_from_thread_id，不冒充 thread_title、guardian、memory_consolidation 或 ambient_suggestions。
- 不设置 passive_feature 或被动授权；仍直接测试管理员选中的账号，不查找父账号、不因等待主根而挂起，也不重新调度其他账号。
- 有已保存/自定义设备 ID 时复用该值，不为每次测试重新生成设备 ID。
- 保留测试模型、测试内容、SSE 结果与诊断；其他提供商的原有测试协议不变。

“无父根”只描述谱系关系，不等于被动权限或某个特定后台功能。测试的客户端身份是独立生成的，但最后出站仍服从该账号的指纹收敛模式；例如 full 模式仍可能把多个测试的元数据收敛到同一账号身份。这里没有更改收敛、缓存隔离策略或永久账号粘性。

独立测连验证认证、模型响应和身份字段传输，不代表已经验证真实附属请求的父根绑定，也不证明多轮历史、压缩或工具调用都能成功。

### 批量测试结果与账号状态

“本次测试失败”不等于“账号异常”。HTTP 500/502/503/504 等服务错误、Responses 流内的 `server_is_overloaded` / `server_error`、模型不可用、无文本输出、缺失终态和传输读写失败，仍在批量结果中展示失败和原始错误详情，但不据此调用 `MarkError` 或写入账号异常状态。测试模型选择失败也不改变账号健康状态。

明确缺少认证凭据、HTTP 401/402/403 及既有账号级 400 错误继续执行原有账号错误/授权处理。HTTP 200 内的错误事件只按结构化 `error.code` / `error.type` 中的账号或凭据错误标记账号，不根据错误说明中的零散账号关键词判断。额度耗尽、429 限流与提供商特有容量错误保留原有处理；测试超时仍可记入调度健康，但不标记永久错误。

测试失败不自动清除已有封禁、冷却或错误；只有明确测试成功才执行原有恢复逻辑。历史误标记不在升级时批量清除，避免误恢复真实异常账号，可在修复部署后重新测试确认恢复。

## 工作区元数据现状

workspaces 是请求元数据中的本地目录、关联 Git 远程地址和最新提交哈希，不是代码正文或工具参数。

本次没有修改现有行为：

- 关闭收敛时不执行工作区收敛。
- 启用收敛后，现有 workspaces 路径映射到 `/workspace/` 占位路径，移除 associated_remote_urls，并替换已有非空提交哈希。
- 在约定 client_metadata / turn metadata 载体里清理 project_id、projectId、workspace_id 及相应项目头。
- 不删除用户代码、提示词或工具业务参数里的同名字段。

工作区脱敏策略是否应改成省略可选字段，应单独评估，不能和 UA 修复或测试身份生成一起无条件变更。
