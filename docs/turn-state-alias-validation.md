# Turn-state 代号映射验证（2026-09-17）

Codex 上游的 `X-Codex-Turn-State` 保存为加密映射，对客户端使用随机代号。当前版本使用与原值封装外观相似的 URL-safe Base64 代号，保留原值的公开版本/时间及长度，其余部分随机生成并由持久化本机密钥签名；无符合格式的原值时使用默认 292 字符。外观相似不表示官方认可该值，发送上游前仍需还原映射。不兼容旧 `c2ts_v1_...` 代号。

管理员日志现在记录完整核对值，使用日志详情的独立 `X-Codex-Turn-State` 区域区分收到值、还原/返回的真实值、实际出站值和签发代号。未知值不会标为已验证真实值，跨作用域不泄露他人映射；普通长度完整显示，超长值及超预算诊断明确截断。映射表加密不覆盖管理员日志中的明文核对值；历史摘要无法恢复。

## 已通过的验证

使用本地模拟上游，未消耗真实模型额度。

| 场景 | 验证结果 |
| --- | --- |
| 同用户、主会话、线程和轮次重复回传 | 还原原值；头部和元数据复用相同代号 |
| 切号后客户端继续回传旧代号 | 选号前清除；发送前再次检查账号和代次；不会恢复旧账号状态 |
| 切回最初的账号 | 仍按切号代次隔离，旧代号不可重新生效 |
| 不同用户、轮次、子线程、未知代号 | 清除，不以代号决定选号 |
| HTTP/SSE、原生客户端 WebSocket、真实本地 WebSocket 上游 | 对应实际转发路径中不发送代号到上游，不向客户端暴露官方原值 |
| `/responses/compact` | 响应头使用代号，请求头还原原值；不向 compact 添加不支持的 client_metadata |
| 换号清理与完整保留 input 两种模式 | 均通过，工具和输入保持原有规则 |
| 数据库关闭重开、请求处理器重建 | 新格式的既有代号仍可还原，密钥保持一致 |
| 两个 SQLite 数据库连接并发签发 | 16 个并发请求得到同一个有效映射；共享持久化密钥 |
| 过期、密文/绑定被修改、缺失密钥 | 不使用无效状态；缺少已有映射的密钥时拒绝初始化 |
| 响应头或流中保存失败 | 不回退透传真值 |
| SSE 多行、JSON 转义、大小写、长行、小块读取 | 正确替换元数据，普通文本事件保持原字节 |
| 池化 WebSocket 握手复用 | 原握手的状态不会被重复解释成下一轮的新状态 |

专项命令通过：

```text
go test ./database ./proxy ./proxy/wsrelay -run 'TurnState|SessionContextProvenance|SessionAccountFailoverOutboundWindows|TestWebsocketSessionFailoverResetsWindowAndConnection|TestSessionPreserveInputWebsocketIngress' -count=1
go vet ./database ./proxy ./proxy/wsrelay
npm run typecheck
node --experimental-strip-types --test src/lib/usageRequestDiagnostics.test.mjs
```

此次外观和完整日志变更通过数据库、代理及 WebSocket 专项回归和 `go vet`；新增验证覆盖封装长度、局部篡改、外来签名、旧记录拒绝、真实值/代号溯源区分、完整值展示及日志预算。前端诊断展示 13 项测试、类型检查和生产构建通过。以下微基准及全量失败记录来自原始代号版本，本次未重新运行全量测试。

普通文本 SSE 事件判别微基准约 171 ns/事件、112 B/事件、2 次分配。该数字不包含网络、数据库或完整读流；数据库查询只发生在代号恢复和必要签发阶段，不按文本片段查询。

当前 Windows 环境 `CGO_ENABLED=0` 且未找到 GCC，未运行 Go race detector。未连接真实 PostgreSQL 或 OpenAI 上游，SQLite 共享连接测试不能替代生产多节点实测。

## 全量测试的已有失败

运行了 `go test ./database ./proxy ./proxy/wsrelay -count=1`。下面 12 项失败已在修改前的 `6ebc5b36` 独立工作树中逐一复现，不属于本次代号改动：

- `TestProxyRiskScoringProfilePersistenceDoesNotExposeSecrets`：Windows 临时数据库文件仍被占用。
- `TestUsageLogInsertColumnCountIncludesCompactionHistory`：已有用量字段数预期 63，实际 66。
- `TestUsageLogInsertRowsStayUnderPostgresBindLimit`：已有批量日志参数数 66000 超过 65535。
- `TestResponsesWebSocketContinuationKeepsBoundAccountPastBoundedLimit`
- `TestResponsesWebSocketContinuationDegradesWhenUpstreamRejectsPreviousResponse`
- `TestResponsesWebSocketContinuationCannotChangeExcludedSessionOwner`
- `TestResponsesWSContextSurvivesMultiTurnFallback`
- `TestResponsesWebSocketNewerSameSessionPreemptsBeforeConcurrencyAdmission`
- `TestClaudeSessionHintPreservesNewAPIRootRouting`
- `TestUpstreamPromptSafetyEndToEndHTTPAndSSE`
- `TestUpstreamPromptSafetyWebSocketFirstRefusal`
- `TestWebsocketContinuityOffRestartsIdentityAndConnection`

除 Claude 根关联断言外，后面多数测试被已有的首次会话身份/时间校验提前拒绝。本次涉及的三个专项旧测试已改用当时生成的 UUIDv7 测试会话，防止测试停留在身份校验阶段，未放宽生产校验。

## 运维边界

映射保留 7 天，活跃映射临近过期会续期。备份/共享数据库必须包含 `codex_turn_state_secret` 和 `codex_turn_states`。密钥与密文同库，不能把这一层加密理解为能抵御完整数据库泄露。升级前客户端持有的未受管原值会清除；新代号不改绑旧账号，也不会代替官方生成真实状态。
