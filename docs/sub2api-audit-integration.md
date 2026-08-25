# Sub2API 上游号池审计集成

本文约定 Sub2API 作为上游自建号池时，与 Codex2API/NewAPI 签名、策略元数据及风险处罚的可实现契约。

## 1. 绑定与导入

管理端为每个 Codex2API API Key 配置唯一 NewAPI 平台绑定：`platform_code`、`secret`、`enabled`、`require_signed_identity`。未绑定 Key 不接受签名，不能借用其他 Key 密钥。轮换时旧值放入 `previous_secret` 并设置 `previous_secret_expires_at`，仅在过期前接受。

Sub2API 导入接口为 `GET /api/v1/admin/accounts?page=&page_size=&platform=openai` 与 `GET /api/v1/admin/accounts/data?platform=openai`，携带管理 API Key；`base_url` 必须以 `http://` 或 `https://` 开头，末尾斜杠会移除。凭据字段包括 `refresh_token`、`access_token`、`session_token`（或 `sessionToken`）及 `id_token`。

## 2. 请求身份签名（HTTP 与 WS 首帧）

必需头：`X-NewAPI-User-ID`、`X-NewAPI-Client-IP`、`X-NewAPI-Request-ID`、`X-NewAPI-Timestamp`（Unix 秒，误差不超过 `MaxClockSkewSeconds`）、`X-NewAPI-Method`（实际方法大写）、`X-NewAPI-Path`（`URL.EscapedPath()`，不含查询串）、`X-NewAPI-Body-SHA256`（原始字节 SHA-256 小写 hex）、`X-NewAPI-Signature-Version`（省略或 `1`）及 `X-NewAPI-Signature`（小写 hex HMAC-SHA256）。Sub2API 绑定也接受等价的 `X-Sub2API-*` 命名空间（例如 `X-Sub2API-User-ID`、`X-Sub2API-Policy-Meta`）；同一请求同时出现两套头且值不一致会被拒绝。

V1 canonical（LF 连接、无末尾 LF）：
```text
v1
{timestamp}
{request_id}
{user_id}
{client_ip}
{METHOD}
{escaped_path}
{body_sha256}
```
签名为 `hex(HMAC-SHA256(secret, canonical))`。`request_id` 在 `api-key:{id}:platform:{sha256(normalized_platform)}` 作用域防重放；TTL 为 `max(2*MaxClockSkewSeconds, 60s)`，同一 ID 仅一次。

WS 沿用首个签名身份，每个逻辑 turn 刷新绑定；平台、启用状态、签名要求或密钥变化时要求重连。

## 3. 策略 policy meta

绑定启用时必须同时发送 `X-NewAPI-Policy-Meta` 与 `X-NewAPI-Policy-Meta-Signature`。Meta 是 JSON 的 Raw URL-safe Base64（无 `=`），编码后 ≤4096 字节，解码 JSON ≤3072 字节。

canonical：
```text
policy-meta-v1
{request_id}
{body_sha256}
{encoded_meta}
```
使用绑定 secret 的 HMAC-SHA256 hex。字段包括 `platform_id`、`profile`、`mode`、`provider`、`protocol`、`original_endpoint`、`original_protocol`、`requested_model`、`upstream_model`、`channel_id`、`session_fingerprint`，以及可选 `user_name`、`user_email`、`user_group`。

约束：profile 仅 `balanced|strict|research`；mode 仅 `off|shadow|warn|enforce`；provider≤32、protocol≤64、model≤128，token 仅 `[a-z0-9._/-:]`；endpoint≤256，必须以 `/` 开头且禁 `?#`、CR/LF/NUL；用户名 128、邮箱 320、用户组 100 Unicode 字符且拒绝控制字符；`session_fingerprint` 必须为 32 个小写 hex（16 字节）。只有已签名 meta 且带 `session_fingerprint` 的 Sub2API 请求会在配置的会话窗口内创建一次零风险用户/会话/API Key/IP 观察（默认窗口 5 分钟）；观察不增加风险分。禁止发送 access/refresh/session token 或提示词原文。

## 4. 风险画像与处罚边界

`off` 不拦截，`shadow` 仅记录，`warn` 告警，`enforce` 可拒绝；默认 profile 为 `balanced`，另有 `strict`、`research`。Codex2API 仅决定当前请求，NewAPI 是 strike、账号/IP 限制及封禁唯一权威。

只有两类 block 可 `Strike-Eligible=true`：上游明确 `cyber_policy` 且 `CYBStrikeEnabled=true`；或本地最高置信度终止 severe 命中且 `LocalSevereStrikeEnabled=true`。conversation-lock 重复请求、普通本地匹配、外部 review verdict、其他上游 4xx 均不得累计 strike。签名失败为认证 401，不写入风险状态；遗留头固定 `X-Codex2API-Policy-Strike: 0`、`X-Codex2API-Policy-Ban: false`。

## 5. HTTP/WS 决策响应

HTTP/首 token 前 SSE 返回 `X-Codex2API-Policy-*` 决策头（Request-ID、Reason、Action、Decision-ID、Profile、Rule-Version、Strike-Eligible、Evidence-SHA256、Severity、Signature-Version=`v1`、Response-Signature）。

决策 canonical：
```text
policy-decision-v1
{request_id}
{decision_id}
{action}
{profile}
{reason_code}
{severity}
{strike_eligible}
{rule_version}
{evidence_sha256}
```
WS 每个 turn 另带 Event-ID、Event-Signature-Version=`v1`、Event-Signature；事件 canonical 在实现中追加 `event_id`。Evidence-SHA256 仅摘要，不发送提示词原文；Sub2API 应按签名字段审计，不自行推断处罚。
