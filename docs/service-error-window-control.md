# 服务错误中的窗口控制请求

`POST /v1/session-windows` 是 NewAPI 与 Codex2API 之间的窗口管理/预留/扩容控制接口。NewAPI 使用 `User-Agent: NewAPI-Window-Control/1` 调用，可能发生在查看窗口、正常聊天前的窗口预留、请求结束清理预留或确认扩容时，不是模型生成请求。

`upstream.transport=not_started`、`send_phase=before_payload` 表示本次控制请求没有向模型上游发送正文。仅凭 HTTP 400 不能判断账号异常、上游限流，或认定用户执行了某一种操作。

## 新日志

- `request_type=gateway_internal`，`request_kind=window_control`。
- `client_info["window_control.operation"]` 记录 `list`、`quote`、`release`、`upgrade`；无法解析/不支持的输入记录有限枚举，不保存未知操作原文。
- `code` 和中文 `message` 区分请求格式、倍率、额外窗口数量、预留标识、窗口容量不足、扩容确认不足、授权已失效等原因。
- 本地窗口 400 归类 `invalid_request_error`，不再默认写为 `server_error`。认证和服务端故障保留对应 HTTP 状态及错误类型。
- 顶层 `{message,code,type}` 和 OpenAI `{error:{message,code,type}}` 两种错误格式均能采集；嵌套字段优先。仍然只保存脱敏诊断字段，不保存控制请求正文、ticket、grant_id 或密钥。

窗口接口继续返回顶层 `message`，兼容 NewAPI 现有读取方式；本次不改变容量限制、计费倍率校验或扩容确认规则。

旧记录中的 `http_400 / Service request rejected` 可能是采集器漏读顶层字段造成的。未保存的原始原因无法从旧记录补回，需用更新后的新日志定位。

## 复制 JSON

服务错误页只调用全局 ToastProvider 显示提示，不把 ToastState 对象当成 React 子节点渲染。剪贴板不可用时在当前对话框内降级复制；失败只显示提示，不关闭详情、提交表单或跳转页面。
