# Turn-State 清理与回传映射

Turn-State 的字段读取、删除和替换统一在 `proxy/codex_turn_state_fields.go`。各场景保留自己的信任策略，不再独立实现正文/头部的字段删除。

## 覆盖范围

- HTTP 专用状态头，以及 metadata 头内的 JSON。
- 请求/响应协议封装中的状态字段、`client_metadata`、`metadata`、`headers`、`response`。
- metadata 内嵌对象、数组、JSON 字符串，包括 `x-codex-turn-metadata` 和下划线写法。
- 状态键的大小写变体、JSON 转义键、重复字段和重复 metadata 容器。
- 重复键采用同一套最后一个值生效的解析语义，校验、写回保持一致。
- 非字符串状态、非单元素字符串数组被移除；无法检查的过深控制子树被丢弃。

协议控制字段之外的 input、output、生成文本、工具参数和 schema 不作状态扫描，避免误改同名业务数据。

## 入站与出站

客户端回传的别名 A 必须通过当前用户/会话/轮次、所有者、账号身份、代次校验，才能还原为上游值 R。未知值、直接传入的 R、无校验上下文的值全部清理。

首次绑定、切号预检、会话重启、执行器边界、WS 新帧/握手重置复用同一字段处理器。HTTP 正文在最终编码前再次处理；HTTP 头在自定义头装配后再次处理；WS 在帧 metadata 投影后再次处理。握手不携带 Turn-State，当前帧中已验证的状态仍可保留。

WS 新帧上下文中类型化的 nil 不视为首次绑定，防止误删合法续轮状态。

## 回传客户端

HTTP 响应头、普通 JSON/compact 正文、SSE 事件正文和 WS 转换后的事件均走统一状态遍历。包括上述 client_metadata 和深层 metadata，不只处理头字典。同一请求/账号/代次的同一个 R 复用同一个 A。

没有映射上下文时删除状态；映射持久化失败时停止放行相关响应数据，不回退为原值。已有响应头白名单和 response ID 映射继续生效。

响应包装器继续转发传输观测及 SSE 终态检测，避免过滤层改变断流重试规则。

## 相关一致性修复

有损重启的 encrypted_content、file_id、item_reference.id 及普通输入项 id，先按实际序列化语义规范化，再做信任校验，消除“校验重复键第一个值、发送最后一个值”的不一致。完整保留 input 模式维持原有策略。

## 验证入口

- `codex_turn_state_fields_test.go`：清理位置矩阵、重复/转义键、嵌套字符串、超深 metadata、无绑定上下文、跨账号/代次、回传一致性、持久化失败、自定义头。
- `codex_turn_state_alias_test.go`：HTTP / 原生 WS 入站、实际 HTTP 上游、切号与 compact。
- `wsrelay/session_failover_epoch_test.go`：实际 WS 上游、连接复用、切号与返回原账号，并检查握手/帧的嵌套状态。
- `session_context_restart_test.go`：其他上下文重复键的一次清理及幂等性。
- 现有断流、首轮、响应隐私测试继续验证兼容性。
