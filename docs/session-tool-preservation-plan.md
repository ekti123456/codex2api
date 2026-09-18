# 切号后工具与上下文保留修复方案

状态：已实现，验证记录见文末。2026-09-15。

后续调整（2026-09-18）：切号候选只要求账号分组集合一致，标签不再参与；本文原“同标签”要求属于历史方案。独立 `function_call_output`（`call_id` 缺省或 `null`）按官方协议保留，参见 `session-preserve-input.md`。

调查证据：`F:/codex/reports/failover-tool-audit-20260915/README.md`，包含官方 Codex 0.154.0 源码、官方文档、诊断探针和五份客户端样本回放。

## 1. 修复目标与范围

让切号、恢复已有切号状态及会话序号重建后的请求完整保留客户端提供的工具和可移植上下文。继续隔离真正属于旧上游账号的续链引用、密文和文件引用。

本次必须完成：

1. 修复 `additional_tools` 被当作空消息整项删除。
2. 修复工具 schema、动态工具搜索参数被递归当作消息/账号引用处理。
3. 同步修正迁移前检查、切号清理和加密错误重试三条路径。
4. 覆盖 HTTP/WS、首次迁移、后续 restored 请求和数据库恢复后的迁移状态。
5. 补充脱敏的工具与清理诊断，并检测清理阶段意外损坏工具的情况。

这是协议兼容修复，默认生效，不增加让用户选择是否保留工具的开关。不改变既有扩容优先、换号候选分组/标签、计费及窗口策略；不自动提交或推送。

## 2. 已确认的故障机制

官方 Responses Lite 每次请求重建如下前缀，顶层通常不再携带这些工具：

```json
{
  "type": "additional_tools",
  "role": "developer",
  "tools": ["实际为工具定义对象"]
}
```

`cleanSessionRestartContext` 将任意有 role 的对象视为消息；该项没有 content，整项以 empty_message 删除。持久化的 LossyContextRestart 使后续请求继续重复此删除。

目录说明和技能正文是普通文字消息，因此可以保留并被模型复述；执行工具声明已经消失，两者并不矛盾。

## 3. 明确各类数据的处理边界

| 内容/位置 | 修复后的规则 |
|---|---|
| 顶层 tools、tool_choice | 清理阶段保持不变，包括 namespace、描述、schema、custom grammar、defer_loading 和工具限制 |
| additional_tools.tools | 完整保留；不能当空消息；保留它相对其他保留项的顺序；外层 id 仍按既有出站项 ID 策略处理 |
| tool_search_output.tools | 完整保留；保留 execution、status、call_id；不能当作普通文本结果丢弃 |
| 工具定义内部 | 作为声明数据，不按 role/file_id/encrypted_content/type 等属性名推断消息或上游依赖 |
| tool_search_call.arguments | 合法结构化业务参数，保持原 JSON 值；不递归执行消息清理 |
| function_call.arguments / custom_tool_call.input | 保留调用参数和输入；维持既有合法格式与配对规则 |
| skills 目录、已注入正文、AGENTS/基础指令、环境信息 | 保留普通消息内容；本次清理不改变客户端权限或技能执行规则 |
| message | 仅 type=message，或无 type 且 role 为合法消息角色时，才按消息规则处理 content |
| 消息/工具输出中的结构化媒体 | 只在对应协议内容项中识别真正的 input_file、input_image 等上游引用；保留内联数据和原有可用 URL |
| reasoning/compaction 等旧密文上下文 | 继续按账号及迁移段校验；不能为了保留工具而恢复旧账号密文 |
| previous_response_id、conversation、turn-state | 继续使用原有来源验证及迁移处理；新账号段已验证引用仍可保留 |
| configuration_update、消息 phase、正常附加元数据 | 不因递归字段名碰撞而删除；保留其协议含义和位置 |
| 未知协议项/扩展字段 | 不猜测业务对象为消息，也不任意递归改写；遇到明确但无法判断归属的上游状态应沿现有错误路径报告，不静默删改或绕过校验 |

“清理阶段保持不变”是与紧邻该阶段的输入比较，不是要求入口正文与最终出站逐字一致。普通前处理仍可能规范化 schema、映射身份或应用已配置的工具策略；必须能区分这些有意变更与清理造成的误删。

## 4. 实现结构

### A. 统一协议识别，先修根因

- 在 proxy 包内增加共享的上下文项分类/遍历辅助函数，供三个调用方使用。
- 复用或统一 `isResponsesMessageInputItem` 的语义，清除“任何 role 非空对象都是消息”的判断。
- 遍历显式的协议容器：input 项、message.content、合法工具 output 内容项；工具声明、JSON Schema、参数和文本属于叶子数据，不进行账号引用清理。
- 保留数字精度、数组顺序和输入不可变性；避免全量 JSON 解码导致任意工具 schema 的数字精度变化。
- 仍保留现有深度/大小保护，但不能把深层 schema 截成残缺工具后继续发送。
- 将“还有可用上下文”的判断与“input 数组不为空”分开：仅剩工具声明/配置项，不能被误判为成功恢复了用户历史。具体按现有有效续链和可用输入语义判断，并保留正常工具结果续接。

主要位置：

- `proxy/session_context_restart.go`
- `proxy/session_context_provenance.go`
- `proxy/translator.go` 中 `stripInvalidEncryptedContentValue` 及相关辅助函数
- 新的共享协议分类文件及对应测试

### B. 三条路径同步修复

1. **迁移前检查**：仅检查真实协议依赖，不将工具 schema 属性视为 file_id 或 encrypted_content。合法工具不得使请求误报 opaque_upstream_context。
2. **出站重建清理**：保留工具声明与调用参数，清理实际旧账号状态；适用于首次切号和每次 restored 请求。
3. **加密错误重试**：只移除指定协议层级的失效密文；不能借机删除 schema 的 encrypted_content 属性。保持现有重试次数限制。

HTTP handler、HTTP executor、WS relay 和原生 WS 的共同处理结果需要一致。逐一核对多次处理顺序，要求相同请求再次清理时不继续损坏内容。

### C. 工具完整性校验

- 在上下文清理紧邻的前后生成工具声明投影，覆盖顶层 tools、additional_tools.tools、tool_search_output.tools 及嵌套 namespace。
- 比较工具定义、tool_choice 及保留项相对顺序；仅外层历史项 id 的已有规范化不计为工具变化。
- 完整性比较在请求内完成，不依赖缓存、数据库历史工具或其他用户请求。不向没有工具的请求擅自补工具。
- 若清理阶段意外删减或修改工具，停止发送被损坏正文，返回明确的网关内部错误，例如 HTTP 500 / `codex_session_tool_context_lost`，错误信息说明工具上下文处理异常。
- 该错误不能触发自动换号或无限重试；也不能回退发送包含未经处理旧密文的原请求。对原本 tools 缺省/为空、合法的前处理工具策略不误报。
- 使用增量摘要/一次遍历等方式控制长上下文成本，不为校验复制数 MB 的全文。

### D. 脱敏诊断

优先复用现有用量/服务错误诊断 JSON，字段按需输出，兼容旧记录；不新增独立的原始请求存储。

记录三个阶段：

1. `ingress`：客户端原始请求。
2. `before_cleanup` / `after_cleanup`：对具体清理操作的前后投影，用于判断此次清理是否损坏工具。
3. `outbound`：实际写往上游的最终正文，包含本次重试版本。

建议内容：

- 顶层工具数、additional_tools 载体数、tool_search_output 载体数。
- namespace 数及实际叶子工具数，按 function/custom/其他工具类型统计。
- 工具定义规范化摘要、tool_choice 模式及必要的脱敏摘要。
- tool_preservation 状态、检查阶段/本次处理序号；工具 schema 摘要比较只针对清理阶段。
- 被删项的 input 索引、协议 type、处理原因；仅使用固定协议路径，避免把用户自定义字段名或参数值写入日志。
- removed 继续保留聚合计数；详细项目列表设上限，超过时记录省略条数。同一阶段再次报告不能不加区分地累计成两次删除。

不得记录完整工具描述、schema、工具参数、提示词、文件内容、令牌。也不通过记录工具名称清单间接暴露私有集成。

接入位置：`responses_input_diagnostics.go`、`outbound_identity_diagnostics.go`、`database/service_errors.go` 及现有诊断展示/导出链路。确认新增字段在用量日志和服务错误中都可读；已有详情不能显示时才补最小展示改动。

## 5. 工具调用配对及剩余上下文问题

本次同步完成正常 `tool_search_call` / `tool_search_output` 配对覆盖；保留旧 `tool_search_call_output` 的现有兼容行为，不一概靠 `_call_output` 后缀推断官方类型。不对合法 hosted tool search 的空 call_id 强行配对。

为无匹配调用的动态工具搜索结果、缺失工具搜索结果、普通/自定义工具结果分别补测；不能生成不符合 schema 的假结果，也不能把声明数组当作普通输出文本。仅在能确认语义的情况下补兼容逻辑，不能以“补配对”为由新引入历史丢失。

以下属于独立的上下文恢复设计，记录但不混入本次工具修复：

- 旧 reasoning 明文摘要是否提取为普通上下文。
- 唯一历史为旧密文 compaction 时的恢复与阻止迁移策略。
- 孤儿工具结果转换为可读历史的跨路径统一策略。
- encrypted_function_args 的官方语义及跨账号来源校验。

这些问题需要各自证据和验收条件，不能未经验证直接全保留或全删除。

## 6. 测试与验收

### 单元与回归

| 测试 | 必须满足 |
|---|---|
| 官方 additional_tools + role=developer、无 content | 整项保留，不产生 empty_message |
| 多个 additional_tools、namespace、custom grammar、defer_loading | 内容和相对顺序不变 |
| schema 属性 role/file_id/encrypted_content/type/content | 不误删、不误判账号依赖，含嵌套、数组和组合 schema |
| tool_search_output 与结构化 arguments | 声明与参数保留；真实配对语义正确 |
| 真空消息与真旧密文/文件引用混合 | 该删的仍删；工具和内联资料保留 |
| 新账号段已验证引用 | 保留；不能因旧段发生过切号永久全部剥离 |
| skills、基础指令、环境文字、phase、配置项 | JSON 语义保持；既有身份映射只影响应映射字段 |
| 重复清理、重试处理 | 内容幂等，诊断区分处理次数，不重复累加假删除 |
| 工具完整性校验 | 能识别整组/单工具/描述/schema/顺序损坏；对无工具及合法前处理变化不误报 |
| 仅工具声明/配置，无可用恢复上下文 | 不以工具存在冒充上下文恢复成功 |
| 五份既有客户端模拟抓包 | additional_tools 均保持 1→1；其余保留输入项一致 |

### 状态与传输集成

- HTTP 入站 → HTTP 出站。
- HTTP 入站 → WS 出站（用户这批日志的路径）。
- 原生 WS 入站 → WS 出站；覆盖当前支持的 fallback 路径。
- 普通会话容量满触发切号、额度用尽触发切号、连续性校验关闭后的重建，至少分别验证策略接入。
- 首次切号 → 后续多轮 restored → 数据库重开后的恢复，工具均保留。
- 扩容优先、同分组同标签选账号、代数/会话 ID 重映射、新旧引用校验的既有测试保持通过。
- 非切号请求与 relay 账号不受新逻辑误伤。
- 用本地 mock 上游检查真正收到的 HTTP 正文/WS 帧，不只调用清理函数；检查模拟 function_call 事件能够到达下游。不调用真实用户环境工具。

检查顺序：先让回归用例在旧代码中失败 → 实现修复 → 定向测试 → 受影响包测试和 vet → 需要时前端类型检查/构建。增加一个接近已见 4 MB 请求的长上下文用例，检查处理耗时/分配量；出现新问题再扩大验证，不无理由重复全量测试。

## 7. 实施顺序与发布验收

1. 写修复后的回归断言，覆盖官方格式、嵌套 schema 和动态工具参数。
2. 实现共享协议边界，修复三个调用方，并验证正确清理旧依赖。
3. 补齐工具搜索配对兼容、完整性校验和脱敏诊断。
4. 完成跨传输、持久化恢复及五份客户端样本回放，检查 diff 与构建。
5. 汇总文件、行为和测试结果供审查；用户要求推送后，才按已授权分支推送。

升级后继续沿用现有会话绑定，不清空 LossyContextRestart 或强制再换号。**旧会话下一次请求确实重新携带完整工具声明时，应能恢复正确透传。** 对只提供增量、未携带工具且又无法恢复历史的请求不能承诺自动恢复，更不能借用别的会话工具补齐。

通过条件：模型上游收到的执行工具定义不再被清理损坏；旧账号隔离测试仍通过；日志可以明确看出工具在哪个阶段出现或消失。普通响应 200 本身不作为修复成功证据。

## 8. 实施结果

- 新增 `response_context_fields.go`：明确声明、参数、消息、媒体及工具结果的遍历边界；未知 legacy 容器仍按保守规则检查，不借此扩大可跨账号透传的旧引用范围。
- 切号清理、迁移前引用检查、加密错误重试共用上述边界；已修复官方 additional_tools、动态工具 schema 和结构化搜索参数误删。加密重试使用 UseNumber，避免清理时损失工具 schema 的大整数精度。
- 工具声明投影包含 namespace、function/custom、描述/schema、tool_choice 和载体顺序；清理前后不同则停止发送，报 `codex_session_tool_context_lost`。该错误走不可重试的网关内部错误路径；已提交的流通过终止事件报告。
- 补充官方 tool_search_output 的配对类型和缓存回放。声明不会转换成普通文本；带明确 call_id 的客户端搜索结果在缺少对应调用时明确拒绝重建；hosted 搜索无 call_id 不强制配对。旧 tool_search_call_output 保留既有处理行为。
- 新增入站/最终出站 `tools` 摘要，以及清理记录 `tools_before`、`tools_after`、`tool_preservation`、`items`、`omitted_items`。只输出计数和摘要，未记录名称、schema、参数或正文。
- 入口摘要位于 `responses_input.tools`，实际出站摘要位于 `upstream.responses_input.tools`；不把本地计算摘要伪装为实际出站正文中的字段。
- 清理明细最多 32 项，路径使用原 input 索引。`pass` 表示处理次数，`details_pass` 表示当前保留明细对应的处理次数；后续对已清理正文的无改动处理不会再次累计删除量，也不会冲掉此前的清理证据。
- 未添加配置开关或数据库表结构迁移。旧日志缺少新增可选字段时仍兼容；前端已有诊断 JSON/导出链路可查看新增字段。

### 验证结果

- 旧代码回归失败已记录：`F:/codex/.tmp/tool-preservation-before.log`。
- 修复后协议、清理、深层 schema、数字精度、动态工具配对、错误及脱敏诊断、重复处理测试通过。
- 实际 HTTP 和 WS 帧验证通过：HTTP→HTTP、HTTP→WS、原生 WS→HTTP fallback、原生 WS→WS；模拟 exec_command 调用事件能返回下游。
- 切号→后续恢复→数据库关闭重开并重建 Handler 的 HTTP→WS 流程通过，账号身份和迁移段仍正确；连续性校验关闭后的多次重建保留工具。
- 五份既有客户端模拟样本全部通过：additional_tools 均为 1→1，叶子工具分别为 12→12、12→12、5→5、12→12、12→12，其余输入项不变。结果在 `F:/codex/reports/failover-tool-audit-20260915/fixed-replay-results.log`。
- 4 MiB 合成上下文的三次基准约 204 ms/次、77 MB 分配/次（Windows、Ryzen 7 5800H、包含整个清理函数）。这是总处理成本观察，未作修复前同条件性能对照，不能解读为本次新增开销或线上延迟承诺。
- `go vet` 受影响包通过；Linux amd64 构建通过，产物 `F:/codex/.tmp/codex2api-tool-preservation`。
- 最终关联回归通过：proxy、wsrelay、database、admin 中会话迁移、容量/额度触发、工具、连续性、缓存回放、加密重试、出站诊断及服务错误相关测试；结果在 `F:/codex/.tmp/tool-preservation-verified.log`。
- 全量运行 proxy/wsrelay/database/admin/auth：wsrelay、auth 通过；proxy/database/admin 共六项失败。以 HEAD 原始代码 overlay 复跑，六项全部复现：Claude 会话提示断言、使用日志列数旧预期、两项 SQLite 临时文件占用、两项统计回填测试。均不是本次新增失败。日志：`F:/codex/reports/failover-tool-audit-20260915/baseline-failures.log`。未把全量测试表述为通过，也未顺带修改这些独立问题。

仍保持第 5 节列明的独立上下文恢复议题，不把旧密文重放、明文摘要提取或 encrypted_function_args 政策混入工具兼容修复。
