# Proposal: Sub2API 会话风险画像与签名命名空间

## 背景

Codex2API 现有 NewAPI 签名链路已经能够校验调用方用户、客户端 IP、请求 ID、session fingerprint 和 policy meta，但 Sub2API 作为上游自建号池时缺少一份明确的接入契约，且干净请求不会出现在风险画像中。这样管理员无法在账号受损前看到池内分发用户与会话的基线，也无法稳定区分共享 API Key 下的不同用户。

## 目标

- 允许 `platform_code=sub2api` 的绑定使用 `X-Sub2API-*` 头，同时复用现有 NewAPI V1 HMAC、body digest、重放保护和密钥轮换。
- 对已验签且带 `session_fingerprint` 的 Sub2API 请求建立用户、会话、API Key、客户端 IP 四类画像。
- 画像观察为零风险分，按 API Key、用户、会话窗口去重，不改变现有 prompt block/warn、CY strike 或账号调度。
- 给 Sub2API 提供字段约束、签名 canonical、隐私和处罚边界文档。

## 非目标

- 不信任 Sub2API 自报的风险分、处罚结果、上游账号选择或 prompt 原文。
- 不新增自动封禁、IP 限制或池内账号惩罚。
- 不替换现有 NewAPI 绑定数据库/API，也不改变未绑定 API Key 的行为。

## 风险与回滚

- 观察写入为异步 best-effort，数据库或运行态缓存异常只跳过观察，不阻断请求。
- 命名空间冲突会按认证失败处理，避免 canonical 与别名头择一导致身份混淆。
- 回滚时删除观察调用和 `session_observed` 写入即可；既有风险事件与签名协议保持兼容。
