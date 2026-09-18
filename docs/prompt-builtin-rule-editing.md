# 内置检测规则编辑

在 `/admin/prompt-filter/rules` 的内置规则行点击编辑，可修改正则、权重（1–1000）、分类和严格模式。规则标识用于关联日志和原有策略，保持不变。已有的组合条件、排除条件和 signal-only 审计分类继续使用内置定义。

编辑后显示“已修改”。再次打开编辑框可“恢复默认”，恢复当前版本随附的规则定义；启停状态独立保留。沿用表单中的正则测试工具。

覆盖配置写入 `system_settings.prompt_filter_builtin_overrides`，保存后热更新，重启通过系统配置恢复。普通设置表单不会覆写该列。引擎缓存包含覆盖配置，因此编辑后的请求不会复用旧规则引擎。

管理接口 `PUT /api/admin/prompt-filter/rules/builtin/:name` 接收 `expected`（编辑前的 name、pattern、weight、category、strict）和 `rule`（新的同结构内容）。`rule: null` 恢复默认。正则无效返回 400；编辑快照过期或并发写入冲突返回 409，不覆盖其他修改。规则接口同时返回生效定义、default 默认定义及 overridden 标记。
