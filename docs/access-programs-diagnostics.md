# access_programs 请求诊断

使用日志的请求诊断弹窗新增 `access_programs` 入站 / 出站对照，导出的诊断 JSON 同样保留：

```json
{
  "access_programs": {
    "inbound": {"state": "present", "value": {"cyber": "daybreak_blue"}},
    "outbound": {"state": "present", "value": {"cyber": "standard"}}
  }
}
```

- `inbound` 在读取本次客户端请求时采集，后续改写、重试不会覆盖。
- `outbound` 在 HTTP 请求发送前或当前 WS 帧发送前采集最终请求体，按上游尝试隔离；运输诊断 `upstream.access_programs` 也保留这一快照。发送失败时，快照不能证明上游已经收到，请结合 `send_phase` 判断。
- `state=present` 保留 JSON 值及类型，显式 `null`、空对象、空字符串也会记录；`state=absent` 才表示采集的请求体没有该字段。
- 缺少快照表示未记录，不等于没有字段。历史日志及未进入发送阶段的请求不能推断出站值。无效 JSON 标记为 `invalid_json`。
- 单侧字段原始 JSON 或日志转义后的 JSON 超过 1024 字节时，标记 `too_large`，只记录原始字节数和原始字段的 SHA-256；正常模式对象完整保留。该对照独立于可裁剪的客户端元数据及出站身份信息，诊断达到容量限制时优先保留。

仅增加现有管理员诊断日志及显示，不额外发送探测请求，不修改 `access_programs`，不改变计费、选号或重试行为。日志通过原有持久化及导出路径保存，无新增数据库列。
