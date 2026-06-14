# OAuth 账号编辑配置优化设计

日期：2026-06-13

## 背景

`/admin/accounts` 账号管理页当前已经支持添加 OAuth 授权账号。添加流程中，用户可以生成 OAuth 授权链接，完成浏览器授权后，将回调 URL 粘贴回管理后台，由前端解析 `code` 和 `state`，再由后端兑换 token 并新增账号。

但在“编辑账号配置”弹窗中，OAuth 授权类型账号目前只有调度、标签、分组、代理等配置能力，没有“账号设置”入口。用户如果需要重新授权，只能重新添加一个 OAuth 账号，容易产生重复账号，也无法直接更新当前账号的授权参数。

本设计目标是在编辑 OAuth 账号时增加账号设置能力，复用现有手动 OAuth 授权体验，支持重新生成授权链接并通过粘贴回调 URL 更新当前账号的 OAuth 授权参数。

## 明确范围

本次实现包含：

1. 编辑 OAuth 账号时显示“账号设置”Tab。
2. 在“账号设置”中支持生成 OAuth 授权链接。
3. 支持复制和打开授权链接。
4. 支持用户手动粘贴回调 URL。
5. 前端从回调 URL 中解析 `code` 和 `state`。
6. 后端新增接口，用授权码更新当前账号的 OAuth 授权参数。
7. 成功后刷新账号列表。

本次实现不包含：

1. 不改造 `/auth/callback` 自动回调逻辑。
2. 不支持浏览器自动回调后直接更新当前账号。
3. 不改变现有“添加 OAuth 账号”流程。
4. 不通过现有 `/oauth/exchange-code` 更新已有账号。
5. 不创建新账号。

## 架构设计

前端负责编辑弹窗中的交互和回调 URL 解析：

- 识别当前编辑账号是否为 OAuth 授权类型。
- 对 OAuth 账号显示“调度设置 / 账号设置”两个 Tab。
- 在账号设置 Tab 中提供手动重新授权流程。
- 调用现有 `/api/admin/oauth/generate-auth-url` 生成 OAuth session 和授权链接。
- 解析用户粘贴的回调 URL，提取 `code` 和 `state`。
- 调用新的后端接口更新当前账号授权参数。

后端负责授权更新：

- 复用现有 OAuth PKCE session 和 token exchange 逻辑。
- 新增“更新已有账号 OAuth 授权”的接口。
- 校验账号存在且为 OAuth 授权类型。
- 校验 OAuth session 和 state。
- 兑换新的 token。
- 写回当前账号的 refresh token、access token、id token、过期时间等 credentials。
- 更新账号代理配置和可解析出的账号信息。
- 同步内存账号状态。
- 记录账号更新事件。

## 后端接口设计

新增接口：

```http
POST /api/admin/accounts/:id/oauth/exchange-code
```

用途：使用 OAuth 授权码更新指定账号的 OAuth 授权参数。

请求体：

```json
{
  "session_id": "xxx",
  "code": "xxx",
  "state": "xxx",
  "proxy_url": "http://proxy.example.com"
}
```

字段说明：

| 字段 | 必填 | 说明 |
| --- | --- | --- |
| `session_id` | 是 | 由 `/oauth/generate-auth-url` 返回 |
| `code` | 是 | 从用户粘贴的回调 URL 中解析 |
| `state` | 是 | 从用户粘贴的回调 URL 中解析 |
| `proxy_url` | 否 | 用于 OAuth token 兑换和后续账号代理配置 |

响应示例：

```json
{
  "message": "OAuth 账号授权参数更新成功",
  "id": 123,
  "email": "user@example.com",
  "plan_type": "plus"
}
```

建议新增 handler：

```go
func (h *Handler) UpdateOAuthAccountCode(c *gin.Context)
```

建议新增路由：

```go
api.POST("/accounts/:id/oauth/exchange-code", h.UpdateOAuthAccountCode)
```

处理流程：

1. 解析账号 ID。
2. 解析并校验请求体。
3. 查询账号是否存在。
4. 校验账号是否为 OAuth 授权类型。
5. 根据 `session_id` 读取 OAuth session。
6. 校验 `state` 是否匹配。
7. 使用现有 `doOAuthCodeExchange` 兑换 token。
8. 校验授权服务器返回了 `refresh_token`。
9. 使用现有 `normalizeTokenCredentialSeed` 和 `tokenCredentialMap` 构造 credentials。
10. 更新当前账号的 refresh token、credentials、proxy URL 和可解析出的账号信息。
11. 同步内存 store。
12. 记录账号事件，例如 `updated/oauth_reauth`。
13. 删除已使用的 OAuth session。
14. 返回更新结果。

## 前端界面设计

编辑 OAuth 账号时，编辑弹窗顶部显示：

```text
[ 调度设置 ] [ 账号设置 ]
```

“调度设置”Tab 保持现有行为不变。

“账号设置”Tab 显示 OAuth 重新授权流程：

```text
账号设置

当前账号：
user@example.com

代理 URL：
[ http://proxy.example.com                       ] [测试代理]

第一步：生成授权链接
说明：点击后生成新的 OAuth 授权链接，请在浏览器中打开并完成授权。

[生成授权链接]

生成后显示：

授权链接：
[ https://auth.openai.com/oauth/authorize?... ] [复制]

第二步：填写回调 URL
说明：授权完成后，请复制浏览器地址栏中的回调 URL 并粘贴到这里。

[ http://localhost:1455/auth/callback?code=xxx&state=xxx ]

[重新生成授权链接]                         [更新授权参数]
```

弹窗底部主按钮根据当前 Tab 切换：

| 当前 Tab | 底部按钮 |
| --- | --- |
| 调度设置 | 保存 |
| OpenAI Responses 账号设置 | 保存 |
| OAuth 账号设置 | 更新授权参数 |

成功更新 OAuth 授权参数后，关闭弹窗并刷新账号列表。

## 前端状态设计

编辑 OAuth 流程使用独立状态，不复用添加账号 OAuth 状态，避免添加和编辑流程互相污染。

建议新增状态：

```ts
const [editOAuthStep, setEditOAuthStep] =
  useState<"generate" | "exchange">("generate");

const [editOAuthSession, setEditOAuthSession] =
  useState<{ session_id: string; auth_url: string } | null>(null);

const [editOAuthProxyUrl, setEditOAuthProxyUrl] = useState("");
const [editOAuthCallbackUrl, setEditOAuthCallbackUrl] = useState("");
const [editOAuthGenerating, setEditOAuthGenerating] = useState(false);
const [editOAuthUpdating, setEditOAuthUpdating] = useState(false);
```

打开编辑弹窗时初始化：

```ts
setEditOAuthStep("generate");
setEditOAuthSession(null);
setEditOAuthCallbackUrl("");
setEditOAuthProxyUrl(account.proxy_url ?? "");
```

关闭编辑弹窗时清空：

```ts
setEditOAuthStep("generate");
setEditOAuthSession(null);
setEditOAuthCallbackUrl("");
setEditOAuthProxyUrl("");
setEditOAuthGenerating(false);
setEditOAuthUpdating(false);
```

## 前端 API 设计

在 `frontend/src/api.ts` 中新增：

```ts
updateOAuthAccount: (
  id: number,
  data: {
    session_id: string;
    code: string;
    state: string;
    proxy_url?: string;
  },
) =>
  request<OAuthExchangeResponse>(`/accounts/${id}/oauth/exchange-code`, {
    method: "POST",
    body: JSON.stringify(data),
  });
```

继续复用已有：

```ts
generateOAuthURL
```

不修改现有：

```ts
exchangeOAuthCode
```

`exchangeOAuthCode` 继续只表示“兑换授权码并新增 OAuth 账号”。

## OAuth 账号识别设计

优先使用后端已有字段：

```ts
account.account_type === "oauth"
```

前端封装判断：

```ts
const isOAuthAccount = (account: AccountRow | null) =>
  account?.account_type === "oauth";
```

编辑弹窗判断：

```ts
const canEditAccountSettings =
  editingAccount?.openai_responses_api || isOAuthAccount(editingAccount);
```

如果实现时发现后端没有稳定返回 `account_type: "oauth"`，则补充后端账号列表响应，确保 OAuth 账号明确返回该类型。

## 数据流设计

```text
用户打开 OAuth 账号编辑弹窗
  ↓
点击“账号设置”
  ↓
输入或确认代理 URL
  ↓
点击“生成授权链接”
  ↓
前端调用 POST /api/admin/oauth/generate-auth-url
  ↓
后端创建 OAuth session
  ↓
返回 auth_url / session_id
  ↓
前端展示授权链接
  ↓
用户打开授权链接并完成授权
  ↓
浏览器跳转到 localhost:1455/auth/callback?code=xxx&state=xxx
  ↓
用户复制该回调 URL
  ↓
用户粘贴到编辑弹窗
  ↓
前端解析 code / state
  ↓
前端调用 POST /api/admin/accounts/:id/oauth/exchange-code
  ↓
后端校验账号、session、state
  ↓
后端兑换 token
  ↓
后端更新当前账号授权参数
  ↓
前端提示成功
  ↓
刷新账号列表并关闭弹窗
```

## 错误处理设计

前端提示：

| 场景 | 提示 |
| --- | --- |
| 未生成授权链接就更新 | 请先生成授权链接 |
| 回调 URL 解析不到 `code` 或 `state` | 回调 URL 无效，请确认包含 code 和 state |
| OAuth session 过期 | OAuth 会话不存在或已过期，请重新生成授权链接 |
| state 不匹配 | state 不匹配，请重新发起授权 |
| 后端兑换失败 | 授权码兑换失败：具体错误 |
| 非 OAuth 账号 | 当前账号不是 OAuth 授权类型，不能重新授权 |

后端状态码：

| 场景 | HTTP 状态 |
| --- | --- |
| 账号 ID 非法 | 400 |
| 请求格式错误 | 400 |
| 账号不存在 | 404 |
| 非 OAuth 账号 | 400 或 409 |
| session 不存在或过期 | 400 |
| state 不匹配 | 400 |
| OAuth token 兑换失败 | 502 |
| 数据库更新失败 | 500 |

## 测试设计

后端测试：

1. `UpdateOAuthAccountCode` 拒绝非法账号 ID。
2. 账号不存在时返回 404。
3. 非 OAuth 账号拒绝重新授权。
4. `session_id`、`code` 或 `state` 缺失时返回 400。
5. session 不存在或过期时返回 400。
6. state 不匹配时返回 400。
7. OAuth token exchange 成功后：
   - 不新增账号。
   - 更新当前账号 refresh token。
   - 更新 credentials。
   - 更新 proxy URL。
   - 返回 email / plan type。
   - 记录账号事件。

前端验证：

```bash
npm run typecheck
```

后端验证：

```bash
go test ./admin
```

完整验证可选：

```bash
go test ./...
npm run build
```

## 兼容性和风险控制

- 现有添加 OAuth 账号流程保持不变。
- 现有 `/oauth/exchange-code` 行为保持不变，继续新增账号。
- 现有 `/auth/callback` 自动回调行为保持不变，继续服务添加账号流程。
- 新接口必须校验账号类型，避免非 OAuth 账号误写 OAuth credentials。
- 前端添加和编辑 OAuth 状态分离，避免互相污染。
- 后端更新已有账号时不能调用 `InsertAccount`，避免产生重复账号。
- 不在日志、toast 或响应中输出 refresh token、access token、id token 等敏感信息。

## 修改总结

本设计通过新增 OAuth 账号编辑专用接口和前端账号设置 Tab，让用户可以在编辑已有 OAuth 账号时手动重新授权，并将新的授权参数写回当前账号。该方案保持添加账号流程不变，避免新增重复账号，同时将自动回调更新排除在本次范围外，降低实现复杂度和误操作风险。
