# Grok Build 推理请求 API 逆向与 Agent 客户端适配文档

> 基于当前仓库源码还原，并用 2026-08-12 的 30 个匿名 OAuth fixture 做去敏、低成本生产探测。目标是帮助你把 Grok/xAI 推理能力接到其他 Agent 客户端，或实现一个本机 OpenAI-compatible/Responses-compatible/Anthropic-compatible 网关。
>
> 本文只描述当前客户端实际发送和接受的协议。`cli-chat-proxy.grok.com` 是 Grok Build 第一方代理，不是承诺长期稳定的公共 API；公开集成优先使用 `https://api.x.ai/v1` 和正式 API Key。不要提取、复制或向第三方客户端暴露 Grok 登录会话令牌。

## 1. 结论先行

当前项目的采样客户端具备三种线协议的编解码能力。这里的“支持”只表示客户端代码能够构造、发送和解析这种格式，不表示任意 Grok 账号、任意模型或任意 Host 都开放全部三个路由。

| 线协议格式 | 最终路径 | `apiBackend` | 实际服务商 | 上游可用性 |
|---|---|---|---|---|
| OpenAI Responses 兼容格式 | `POST /v1/responses` | `responses` | 由凭据解析后的最终 sampling Base URL Host 决定；默认可能是 xAI/Grok 或 Grok Build 代理，不是 OpenAI | 主会话通常对 backend 为 `responses` 的模型使用 |
| OpenAI Chat Completions 兼容格式 | `POST /v1/chat/completions` | `chat_completions` | 由最终 sampling Base URL Host 决定 | 主会话通常对 backend 为 `chat_completions` 的模型使用；未知 backend 当前回退到此格式 |
| Anthropic Messages 兼容格式 | `POST /v1/messages` | `messages` | 由最终 sampling Base URL Host 决定，可能是 Anthropic、第三方兼容服务或兼容代理 | **条件能力**；不能据客户端有此代码就断言任一 Grok 账号拥有此接口 |

### 1.1 必须分开的三个概念

1. **协议格式**：JSON 字段和 SSE 事件长得像哪套 API。仓库用 `async-openai` 的 Responses 类型编码/解析 `/responses`，所以它采用 OpenAI Responses API 兼容线协议。
2. **服务提供方**：由最终 URL 的 Host 决定。`https://api.x.ai/v1/responses` 是 xAI/Grok 服务；`https://cli-chat-proxy.grok.com/v1/responses` 是 Grok Build 第一方代理上的 Grok 推理服务。两者都不是 OpenAI 官方服务。只有 Host 明确属于 OpenAI 时，才是 OpenAI 服务。
3. **账号/模型可用性**：必须由当前凭据看到的目录和实际请求结果确认。源码只证明 settings、JWT tier 等会影响客户端访问门、默认选择和目录刷新时机；仓库不含代理服务端，不能据此推导“某套餐必有某协议”。

因此本文后文提到“Responses”时，默认含义是：**Grok/xAI `/v1/responses` 端点采用 OpenAI Responses API 兼容线协议**，而不是“请求发给 OpenAI”。

官方 OpenAI 文档同样把 OpenAI 自己 Host 上的 `POST /v1/responses` 称为 Responses API，并展示 `https://api.openai.com/v1/responses`。这里借用的是该协议形状；只有最终 Host 是 OpenAI，才是 OpenAI 服务。参见 [OpenAI：Migrate to the Responses API](https://developers.openai.com/api/docs/guides/migrate-to-responses#1-update-generation-endpoints)。

主会话通常按模型目录 backend 路由；辅助模型找不到目录项时可构造隐藏 fallback 并强制 Responses，底层显式协议方法也可绕过目录。这些代码级例外仍不证明服务端授权。

最实用的接入策略：

1. 当前凭据的模型目录将目标模型标为 `apiBackend: "responses"`，且目标客户端支持 Responses：在解析凭据与双端点配置后，调用最终 sampling Base URL 下的 `/responses`。
2. 目标客户端只支持 OpenAI Chat Completions：提供 `/v1/chat/completions`，或在本机做 Chat → Responses 转换。
3. 目标客户端只支持 Anthropic：优先使用自建网关做 Messages → Responses/Chat 转换。Grok Build 代理确实存在原生 `/v1/messages` 路由，但它是按账号和余额条件开放的私有能力；本次实测还证明 `/models` 即使只声明 `responses`，部分账号也可能调用 Messages，因此目录只能作为主路由提示，原生 Messages 必须单独做低成本能力探针。
4. 使用公开 xAI API Key 时，Base URL 使用 `https://api.x.ai/v1`。
5. 必须复用 Grok Build 登录态时，用只监听回环地址的本机网关代持登录态；第三方 Agent 客户端只连接本机网关，不接触真实令牌。

## 2. 端点与 URL 拼接

### 2.1 Base URL

源码中的生产默认值：

```text
公开 xAI API:       https://api.x.ai/v1
Grok Build 代理:    https://cli-chat-proxy.grok.com/v1
```

采样客户端会去掉最终 `SamplerConfig.base_url` 末尾的 `/`，再根据**所选模型的** `apiBackend` 追加其中一条协议路径：

```text
{base_url}/chat/completions
{base_url}/responses
{base_url}/messages
```

因此 Base URL 应包含 `/v1`。模型可同时配置会话端点 `baseUrl/base_url` 和 API-key 端点 `apiBaseUrl/api_base_url`；默认内置模型在会话凭据下使用 `cli-chat-proxy.grok.com`，在全局 `XAI_API_KEY` 分支使用 `api.x.ai`。如果 Base URL 自带 query，或模型配置提供 `query_params`，客户端会把 query 放到最终路径之后；配置项覆盖 Base URL 中同名 query 参数，并进行 URL 编码。

### 2.2 相关发现端点

| 方法 | 路径 | 用途 |
|---|---|---|
| `GET` | `/v1/models` | 模型目录，响应为 `{"data":[...]}` |
| `GET` | `/v1/models-v2` | 仅在会话鉴权、闲置至少 600 秒且当前 Host 为 `cli-chat-proxy` 时，刷新当前模型的上下文窗口和最大输出；不是完整目录发现接口 |
| `GET` | `/v1/settings` | Grok Build 远程功能/策略配置；包含访问门、显示套餐和最低客户端版本，属于代理扩展 |
| `GET` | `/v1/login-config` | 登录流程选择；登录前可访问，属于代理扩展 |
| `GET` | `/v1/user?include=subscription` | OAuth 账号的实时订阅层级；属于 Grok Build 代理控制面 |
| `GET` | `/v1/billing?format=credits` | 当前额度周期、用量百分比、预付/按量余额；属于 Grok Build 代理控制面 |
| `GET` | `/v1/auto-topup-rule` | 自动充值规则；属于 Grok Build 代理控制面 |

为其他 Agent 客户端实现兼容服务时，可以暴露：

```text
GET  /v1/models
POST /v1/chat/completions
POST /v1/responses       # 上游原生支持，或由网关转换
POST /v1/messages        # 可选：上游原生支持，或由网关模拟
```

必须在接口说明或模型元数据中标记哪些路径是“上游原生”、哪些是“网关转换”。尤其不要因为本仓库包含 Messages 编解码器，就向所有下游客户端宣称当前 Grok 账号原生支持 `/v1/messages`。

仓库还包含 `code.grok.com` 上的远程 session 创建、读取、更新、删除与分享接口。它们属于会话持久化控制面，不是上述推理线协议；逆向推理网关不应把它们混入 `/v1/responses`、`/chat/completions` 或 `/messages` 契约。

## 3. 鉴权与公共请求头

### 3.1 公开 xAI API Key（推荐）

```http
Authorization: Bearer <XAI_API_KEY>
Content-Type: application/json
```

不要给 `api.x.ai` 添加 Grok 登录态专用的 `X-XAI-Token-Auth`。

### 3.2 `x-api-key` 模式

采样器支持两种鉴权方案：

```text
bearer    -> Authorization: Bearer <key>
x_api_key -> x-api-key: <key>
```

`auth_scheme` 与 `api_backend` 相互独立；Messages 不必然使用 `x-api-key`，Responses/Chat 也不必然使用 Bearer。`x_api_key` 常见于 Anthropic/第三方兼容端点。某些 Anthropic 服务还要求 `anthropic-version`，但采样器不会自动生成它；应通过模型的额外请求头显式配置。

### 3.3 Grok Build 第一方代理登录态

代理上的**推理采样请求**在当前实现中使用：

```http
Authorization: Bearer <user-session-token>
X-XAI-Token-Auth: xai-grok-cli
x-authenticateresponse: authenticate-response
x-grok-client-version: <client-version>
x-grok-client-identifier: grok-shell
x-grok-client-mode: interactive | headless
```

采用 `GrokAuthCredentials`/`ShellAuthCredentialProvider` 的部分代理服务请求可使用裸 Bearer deployment key，并在该 credential provider 内优先于用户登录态：

```http
Authorization: Bearer <deployment-key>
```

这不是所有主模型采样路径都必经的 deployment-key 规则；主模型凭据解析另有“模型 key/env key → auth provider → session token → 全局 `XAI_API_KEY`”顺序。

会话身份读取 `/models` 时的目录/管理请求头也不完全相同，已确认包含 `Authorization`、`X-XAI-Token-Auth`、`x-userid`、`x-grok-client-version`、client mode，并可选带 `x-email`；不要假设它一定带推理请求中的 `x-authenticateresponse`。

这些代理头是当前第一方实现细节。适配其他客户端时的安全做法是：

- 令牌仅保存在本机网关进程中；
- 网关仅监听 `127.0.0.1`/`::1`；
- 下游客户端使用单独的本机访问密钥；
- 不把 `Authorization`、`X-XAI-Token-Auth` 或登录态写入请求日志；
- 不允许下游传入任意上游 URL 或覆盖鉴权头。

### 3.4 Grok 跟踪与路由头

采样器给三种推理请求统一添加以下头：

| 请求头 | 当前实现 | 含义/建议 |
|---|---|---|
| `x-grok-conv-id` | 总会写入，可能为空 | 会话关联 ID；源码并不强制 UUID，旁路调用还可能使用带标签的值 |
| `x-grok-req-id` | 总会写入，可能为空 | prompt/turn 级关联 ID；同一工具循环和部分重采样会复用，不是逐 HTTP 请求唯一 ID |
| `x-grok-model-override` | 总会写入 | 与请求体 `model` 相同 |
| `x-grok-session-id` | 总会写入，可能为空 | Agent 会话 ID |
| `x-grok-agent-id` | 总会写入，可能为空 | 安装/Agent 实例标识 |
| `x-grok-turn-idx` | 可选 | 主会话为 1-based：首个真实用户 turn 通常为 `1`，每个新用户 turn 递增，同一工具循环保持不变 |
| `x-grok-deployment-id` | 可选且非空时发送 | deployment key 派生标识，不是原始密钥 |
| `x-grok-user-id` | 可选且非空时发送 | 第一方用户标识 |
| `x-compactions-remaining` | 模型配置驱动 | 剩余 compaction 次数控制信息 |
| `x-compaction-at` | 模型配置驱动 | compaction 阈值控制信息 |
| `x-grok-doom-loop-check` | 特定 Responses 重采样 | doom-loop 检查模式下为 `true` |
| `traceparent` | 可选、逐请求注入 | W3C Trace Context |

公开 xAI API 的兼容网关不应假设这些扩展头必需。代理模式可按当前客户端行为生成，但是否为服务端硬性要求仍需实测；不要允许不可信下游任意覆盖控制头。

### 3.5 流式请求头

所有流式协议都发送：

```http
Accept: text/event-stream
Content-Type: application/json
```

## 4. 通用响应头

采样器会读取以下上游响应头：

| 响应头 | 类型 | 用途 |
|---|---:|---|
| `x-grok-context-window` | `u64` | 当前模型上下文窗口 |
| `x-grok-max-completion-tokens` | `u32` | 最大输出 token 数 |
| `x-models-etag` | string | 模型目录刷新提示（opaque）；不是 `/models` HTTP `ETag` 快照字段 |
| `Retry-After` | 整数秒 | 限流/重试等待；客户端解析上限 120 秒 |
| `x-should-retry` | `true` / `false` | 服务端重试建议 |

这些都是 xAI/Grok 扩展或重试控制信息，不是标准 OpenAI 响应头。当前 Agent 会提取前三项为内部模型元数据事件，而不是承诺原样透传：`x-models-etag` 是触发重新拉取标准 `/models` 的控制面提示；session 层只接受响应头带来的 context-window 增大，不把它当作权威降级源。网关若选择向可信下游转发，应过滤下游回注，且不要把这些头描述为标准协议字段。

### 4.1 配置与请求头覆盖顺序

- API key 环境变量优先使用 `XAI_API_KEY`，旧 `GROK_CODE_XAI_API_KEY` 仅作回退；
- endpoint 可由 `GROK_CLI_CHAT_PROXY_BASE_URL`、`GROK_XAI_API_BASE_URL`、`GROK_MODELS_BASE_URL`、`GROK_MODELS_LIST_URL` 等覆盖；
- sampler 中模型 `extra_headers` 先写入，模型的 `env_http_headers` 后写入并覆盖同名头；
- shell 合并全局额外头时只填模型尚未覆盖的键，比较时不区分大小写；
- 安全边界仍应禁止下游覆盖 `Authorization`、`x-api-key` 和代理登录态头。

## 5. Chat Completions 兼容协议（服务商由 Host 决定）

### 5.1 请求

```http
POST {base_url}/chat/completions
```

项目实际支持的请求体字段：

| 字段 | 类型 | 是否必需 | 说明 |
|---|---|---|---|
| `model` | string | 对外建议必需 | 内部可由模型配置补齐 |
| `messages` | array | 是 | 完整消息历史 |
| `temperature` | number | 否 | 模型默认值可补齐 |
| `max_tokens` | integer | 否 | 注意当前实现发送的是 `max_tokens`，不是 `max_completion_tokens` |
| `top_p` | number | 否 | nucleus sampling |
| `frequency_penalty` | number | 否 | 类型支持，统一会话转换默认不设置 |
| `presence_penalty` | number | 否 | 类型支持，统一会话转换默认不设置 |
| `user` | string | 否 | 类型支持，统一会话转换默认不设置 |
| `tools` | array | 否 | OpenAI function tools |
| `tool_choice` | string/object | 否 | `auto`、`none`、`required` 或指定函数 |
| `search_parameters` | object | 否 | xAI Chat 扩展；统一会话主路径默认不设置 |
| `response_format` | object | 否 | JSON Schema 结构化输出 |
| `reasoning_effort` | string | 否 | `none|minimal|low|medium|high|xhigh|max` |
| `stream` | boolean | 流式时是 | 采样器强制为 `true` |
| `stream_options.include_usage` | boolean | 流式时是 | 采样器强制为 `true` |

最小流式请求：

```json
{
  "model": "<model-id>",
  "messages": [
    {"role": "system", "content": "You are a coding agent."},
    {"role": "user", "content": "Explain this repository."}
  ],
  "stream": true,
  "stream_options": {"include_usage": true}
}
```

带图片的用户消息：

```json
{
  "role": "user",
  "content": [
    {"type": "text", "text": "分析这张图"},
    {"type": "image_url", "image_url": {"url": "data:image/png;base64,..."}}
  ]
}
```

URL 图片也使用 `image_url.url`。工具结果可以使用相同内容块结构携带图片，但并非所有 OpenAI 兼容服务都接受 tool role 的图片。

### 5.2 函数工具定义

```json
{
  "type": "function",
  "function": {
    "name": "read_file",
    "description": "Read a UTF-8 file",
    "parameters": {
      "type": "object",
      "properties": {
        "path": {"type": "string"}
      },
      "required": ["path"],
      "additionalProperties": false
    }
  }
}
```

指定工具：

```json
{
  "type": "function",
  "function": {"name": "read_file"}
}
```

仅当 `tools` 非空时才应发送 `tool_choice`，否则部分 OpenAI 兼容服务会返回 400。

Chat Completions 的结构化输出格式：

```json
{
  "response_format": {
    "type": "json_schema",
    "json_schema": {
      "name": "structured_output",
      "schema": {
        "type": "object",
        "properties": {"answer": {"type": "string"}},
        "required": ["answer"],
        "additionalProperties": false
      },
      "strict": true
    }
  }
}
```

### 5.3 xAI Chat 实时搜索扩展

`search_parameters` 是 Chat Completions 路径上的 xAI 扩展，和 Responses 的 hosted `web_search`/`x_search` 不是同一种线格式：

```json
{
  "search_parameters": {
    "mode": "auto",
    "sources": [
      {
        "type": "web",
        "allowed_websites": ["docs.rs", "github.com"],
        "excluded_websites": [],
        "country": "US",
        "safe_search": true
      },
      {
        "type": "x",
        "included_x_handles": ["xai"],
        "excluded_x_handles": [],
        "post_favorite_count": 10,
        "post_view_count": 1000
      },
      {
        "type": "news",
        "excluded_websites": [],
        "country": "US",
        "safe_search": true
      },
      {
        "type": "rss",
        "links": ["https://example.com/feed.xml"]
      }
    ],
    "from_date": "2026-01-01",
    "to_date": "2026-08-12",
    "return_citations": true,
    "max_search_results": 20
  }
}
```

`mode` 的源码约定为 `off | on | auto`。不指定 sources 时，注释约定默认搜索 Web 与 X。响应可能在顶层和 assistant message 中返回 `citations: string[]`。目标上游不是 xAI 时，应删除此字段或转换为对应 provider 的搜索工具。

### 5.4 工具调用消息循环

模型返回：

```json
{
  "role": "assistant",
  "content": null,
  "tool_calls": [
    {
      "id": "call_abc",
      "type": "function",
      "function": {
        "name": "read_file",
        "arguments": "{\"path\":\"README.md\"}"
      }
    }
  ]
}
```

客户端执行工具后，把 assistant 调用和 tool 结果都放回下一次完整历史：

```json
[
  {"role": "user", "content": "读取 README"},
  {
    "role": "assistant",
    "content": "",
    "tool_calls": [
      {
        "id": "call_abc",
        "type": "function",
        "function": {
          "name": "read_file",
          "arguments": "{\"path\":\"README.md\"}"
        }
      }
    ]
  },
  {
    "role": "tool",
    "tool_call_id": "call_abc",
    "content": "# Project ..."
  }
]
```

`function.arguments` 是 JSON 字符串，不是对象。流式阶段它可能是不完整 JSON，必须先拼接完再解析。项目在重放历史前会校验它；非法 JSON 会替换成 `{}`，避免后续请求整体被服务端拒绝。

### 5.5 非流式响应

```json
{
  "id": "chatcmpl_...",
  "object": "chat.completion",
  "created": 1750000000,
  "model": "<model-id>",
  "choices": [
    {
      "index": 0,
      "message": {
        "role": "assistant",
        "content": "...",
        "reasoning_content": "...",
        "tool_calls": []
      },
      "finish_reason": "stop"
    }
  ],
  "usage": {
    "prompt_tokens": 100,
    "completion_tokens": 20,
    "total_tokens": 120,
    "prompt_tokens_details": {"cached_tokens": 80},
    "completion_tokens_details": {"reasoning_tokens": 10},
    "cost_in_usd_ticks": 12345
  },
  "citations": []
}
```

`cost_in_usd_ticks` 是 xAI 扩展：`1 USD = 10^10 ticks`。`0` 或负值应按“未报告”处理，而不是免费。

`finish_reason` 支持：

```text
stop | length | tool_calls | content_filter | function_call
```

### 5.6 SSE 流

每个事件是标准 SSE：

```text
data: {JSON}

data: {JSON}

data: [DONE]

```

块结构：

```json
{
  "id": "chatcmpl_...",
  "object": "chat.completion.chunk",
  "created": 1750000000,
  "model": "<model-id>",
  "choices": [
    {
      "index": 0,
      "delta": {
        "role": "assistant",
        "content": "文本增量",
        "reasoning_content": "推理增量",
        "tool_calls": [
          {
            "index": 0,
            "id": "call_abc",
            "type": "function",
            "function": {
              "name": "read_file",
              "arguments": "{\"pa"
            }
          }
        ]
      },
      "finish_reason": null
    }
  ],
  "usage": null
}
```

流式聚合规则：

1. 线协议带 `choices[].index`，但当前项目只维护一组文本/推理累加器，主路径按单 choice 工作；多 choice 会混合文本且工具 index 可能碰撞，不能无损聚合。
2. 在这个单 choice 前提下，以 `delta.tool_calls[].index` 区分并行工具调用。
3. `id`、`type` 和 `function.name` 通常只在第一个工具块出现。
4. 按收到顺序拼接 `function.arguments`。
5. 最后一个块可能没有文本，只携带 `finish_reason` 和 `usage`。
6. `[DONE]` 是规范终止标记；显式 transport error 会失败，但当前 Chat 聚合器对无错误 EOF 不强制要求 `[DONE]`/`finish_reason`，已有可见内容时仍可能产出完成结果。严格网关应自行收紧。
7. 第一段字节可能含 UTF-8 BOM，项目会剥离 `EF BB BF`。

Chat `finish_reason` 是闭合枚举，当前已知值之外的新字符串会使该响应或 chunk 反序列化失败；它不像 Messages `stop_reason` 那样保留未知值。

## 6. Responses 兼容协议（服务商由 Host 决定）

本章描述的是请求/响应格式，不是在识别服务商。源码使用 `async_openai::types::responses::CreateResponse` 等类型，并在最终解析出的 sampling Base URL 后追加 `responses`；最终请求发给谁由该 URL 的 Host 决定：

```text
https://api.x.ai/v1/responses                  -> xAI/Grok 公开 API
https://cli-chat-proxy.grok.com/v1/responses  -> Grok Build 第一方代理
https://api.openai.com/v1/responses            -> 只有这种 OpenAI Host 才是 OpenAI 服务
```

所以，“OpenAI Responses compatible”只说明线协议兼容，不说明模型、账号、计费或服务归 OpenAI。

### 6.1 当前项目实际默认行为

统一 `ConversationRequest → Responses` 路径采用以下策略；底层直接传 `CreateResponseWrapper` 的调用者仍可显式控制更多字段：

- `store: false`：客户端请求服务端不要存储；该字段本身不能证明上游的完整日志/保留策略；
- `include` 至少包含 `reasoning.encrypted_content`：保证推理上下文可在后续回合重放；
- `reasoning.summary: "concise"`；
- `model`、`temperature`、`top_p`、`max_output_tokens` 可由模型配置补齐；
- 流式时设置 `stream: true`；
- 模型配置开启时添加 xAI 扩展 `stream_tool_calls: true`；
- 不使用 `previous_response_id`，而是发送当前本地会话快照；发送前可能经过 auto-compaction、旧工具结果裁剪和旧图片替换，不能称为永久完整历史；
- Responses 线协议支持 `prompt_cache_key`，但当前主 Agent 请求构造固定为 `None`；部分 side call 会显式设置。

### 6.2 最小流式请求

```json
{
  "model": "<model-id>",
  "input": [
    {
      "type": "message",
      "role": "system",
      "content": "You are a coding agent."
    },
    {
      "type": "message",
      "role": "user",
      "content": "Inspect this repository."
    }
  ],
  "reasoning": {
    "effort": "high",
    "summary": "concise"
  },
  "include": ["reasoning.encrypted_content"],
  "store": false,
  "stream": true
}
```

目标上游与模型明确支持时，可按需设置：

```json
{
  "prompt_cache_key": "conv_<stable-uuid>",
  "max_output_tokens": 32768,
  "stream_tool_calls": true
}
```

`stream_tool_calls` 是 xAI 扩展，转发到非 xAI Responses 服务时应允许关闭。`prompt_cache_key` 是兼容网关可采用的策略，不代表 Grok Build 主会话当前会发送它。

### 6.3 `input` 内容类型

普通文本消息：

```json
{
  "type": "message",
  "role": "user",
  "content": "hello"
}
```

多模态消息：

```json
{
  "type": "message",
  "role": "user",
  "content": [
    {"type": "input_text", "text": "分析图片"},
    {
      "type": "input_image",
      "image_url": "data:image/png;base64,...",
      "detail": "auto"
    }
  ]
}
```

上面是 wire 格式，不等于 ACP 输入可原样直传任意图片。当前 Agent 的图片预处理还会执行类型/内容块校验，并以约 1.5 MiB、2,408,448 像素、最长边 2000 为边界做归一化；无法处理的旧图片可能被丢弃或替换，资产也可能落到本地 assets 管理。复刻 Grok Build 输入管线时应把“客户端图片预处理”和“上游协议允许的 image 字段”分成两层。

历史中的函数调用：

```json
{
  "type": "function_call",
  "call_id": "call_abc",
  "name": "read_file",
  "arguments": "{\"path\":\"README.md\"}"
}
```

函数结果：

```json
{
  "type": "function_call_output",
  "call_id": "call_abc",
  "output": "# Project ..."
}
```

带图片的函数结果可将 `output` 改为内容数组：

```json
{
  "type": "function_call_output",
  "call_id": "call_img",
  "output": [
    {"type": "input_text", "text": "image loaded"},
    {"type": "input_image", "image_url": "data:image/png;base64,...", "detail": "auto"}
  ]
}
```

### 6.4 函数工具

Responses 的函数工具没有 Chat Completions 外层 `function`：

```json
{
  "type": "function",
  "name": "read_file",
  "description": "Read a UTF-8 file",
  "parameters": {
    "type": "object",
    "properties": {"path": {"type": "string"}},
    "required": ["path"],
    "additionalProperties": false
  }
}
```

工具选择：

```json
"auto"
"none"
"required"
{"type": "function", "name": "read_file"}
```

### 6.5 服务端托管工具

这些工具由上游在服务端执行，不应再交给本地 Agent 执行。

Web 搜索：

```json
{
  "type": "web_search",
  "filters": {
    "allowed_domains": ["docs.rs", "github.com"]
  }
}
```

xAI X 搜索扩展：

```json
{
  "type": "x_search",
  "from_date": "2026-01-01",
  "to_date": "2026-08-13"
}
```

日期格式必须为零填充 `YYYY-MM-DD`。项目语义中 `from_date` 包含当日，`to_date` 为对应日期 00:00 UTC 的排他上界。函数工具若与托管工具重名，项目会丢弃函数工具，避免上游因重复工具名返回 400。

`x_search` 的 raw extra-tool 注入只在当前统一 Responses **流式**路径实现；统一非流式路径不会复制该扩展。不能把它当成所有 Responses 调用方式都具备的能力。

服务端工具的输出项可能是：

```text
web_search_call
custom_tool_call       # x_search
code_interpreter_call
```

高保真网关应按原始顺序保存并在下一回合重放这些服务端工具项，且不能转换成本地 `function_call_output`。但 Grok Build 当前统一转换只持久化 web/X/code-interpreter 类 BackendToolCall；MCP call 只被计数后丢弃，部分其他 typed 事件也仅解析或忽略，因此不能声称当前实现对所有 hosted tool 无损。

### 6.6 补充的 Responses 顶层字段

底层 Responses 类型还允许 `instructions`、`previous_response_id`、`metadata`、`parallel_tool_calls`、`max_tool_calls`、`service_tier`、`truncation`、`safety_identifier` 等标准字段；当前 Grok Build 统一转换不会主动设置它们。兼容网关的处理原则：

- 对同一个 Responses 上游可透明转发它真正支持的标准字段；
- 转换到 Chat/Messages 时，对无法等价表达的字段显式拒绝或记录降级，不要静默伪造；
- 当前主路径不依赖 `previous_response_id`，而是发送经 compaction/pruning 后的本地会话快照；`prompt_cache_key` 主对话当前为 `None`；
- 不要同时让网关和下游客户端各自拼一份历史，否则会重复上下文和工具结果。

### 6.7 结构化输出

```json
{
  "text": {
    "format": {
      "type": "json_schema",
      "name": "structured_output",
      "schema": {
        "type": "object",
        "properties": {"answer": {"type": "string"}},
        "required": ["answer"],
        "additionalProperties": false
      },
      "strict": true
    }
  }
}
```

### 6.8 非流式响应与完整输出

```json
{
  "id": "resp_...",
  "object": "response",
  "created_at": 1750000000,
  "model": "<model-id>",
  "status": "completed",
  "reasoning": {"effort": "high", "summary": "concise"},
  "output": [
    {
      "type": "reasoning",
      "id": "rs_...",
      "summary": [
        {"type": "summary_text", "text": "..."}
      ],
      "encrypted_content": "opaque...",
      "status": "completed"
    },
    {
      "type": "message",
      "id": "msg_...",
      "role": "assistant",
      "status": "completed",
      "content": [
        {"type": "output_text", "text": "final text", "annotations": []}
      ]
    }
  ],
  "usage": {
    "input_tokens": 100,
    "output_tokens": 30,
    "total_tokens": 130,
    "input_tokens_details": {"cached_tokens": 80},
    "output_tokens_details": {"reasoning_tokens": 15},
    "cost_in_usd_ticks": 12345,
    "context_details": {
      "input_tokens": 100,
      "output_tokens": 30
    }
  }
}
```

`usage` 的处理要区分：

- `input_tokens`/`output_tokens` 是累计计费量；
- `context_details.input_tokens + output_tokens` 是服务端多步工具循环完成后的实时上下文长度；
- 当前项目仅在 Responses **SSE 终态**的原始 JSON 窥探中读取 `context_details` 和 `cost_in_usd_ticks`；若收到完整 `context_details`，会用其覆盖展示用的 `total_tokens`，但不修改计费字段。非流式 typed 响应路径不会执行这层覆盖/元数据搬运。

### 6.9 推理项的重放

兼容网关若要跨回合尽量保持推理上下文：

1. 请求必须包含 `include: ["reasoning.encrypted_content"]`；
2. 保存 `output` 中每个 `reasoning` 项的 `id`、`summary`/`content`、`encrypted_content`；
3. 下一回合把该项按原顺序放回 `input`；
4. 重放时删除输出专用的 `status`；
5. 若 reasoning `content` 子项缺少类型，补上 `"type":"reasoning_text"`；
6. 不要把多个 reasoning 项合并成一个，也不要改变它们与服务端工具调用的相对顺序。

这是 Responses 相比 Chat Completions 最重要的保真差异。转换到 Chat 时只能把可见推理文本折叠到 `reasoning_content`，加密内容和原始项顺序会丢失。Grok Build 当前终态归一化还会合并 assistant 文本、聚合 function calls，并忽略部分未知/MCP output；上述步骤是高保真网关目标，不是当前客户端对全部 output 类型的既成保证。

### 6.10 Responses SSE 事件

关键事件：

| `type` | 关键字段 | 处理 |
|---|---|---|
| `response.created` | `response` | 初始化状态，不代表已有输出 |
| `response.in_progress` / `response.queued` | `response` | 状态/心跳 |
| `response.output_text.delta` | `delta` | 追加 assistant 可见文本 |
| `response.reasoning_summary_text.delta` | `delta` | 追加可展示推理摘要 |
| `response.reasoning_text.delta` | `delta` | 追加原始推理文本 |
| `response.output_item.added` | `output_index`, `item` | 若为 `function_call`，记录 `call_id` 和 `name` |
| `response.function_call_arguments.delta` | `output_index`, `delta` | 按 `output_index` 拼接参数 |
| `response.output_item.done` | `item` | 服务端工具完整结果的权威来源 |
| `response.web_search_call.*` | `item_id` | 服务端搜索进度 |
| `response.custom_tool_call_input.*` | `item_id`, `delta/input` | x_search 进度 |
| `response.code_interpreter_call.*` | `item_id` | 服务端代码执行进度 |
| `response.completed` | `response` | 正常终态；最终 `response.output` 是权威数据 |
| `response.incomplete` | `response` | 长度/不完整终态 |
| `response.failed` | `response.error` | 失败终态 |
| `response.error` | `code`, `message` | 流内错误 |

类型库还能解析 file search、image generation、MCP、refusal 等更多事件；其中一部分只重置 idle/展示进度或落入忽略分支。上表是当前 Agent 重点投影的事件，不是完整 Responses 协议全集。

建议策略：流式 delta 用于 UI，`response.completed.response.output` 用于最终权威终态。不要只靠 delta 重建 reasoning 或服务端工具项。Responses 路径即使收到 `[DONE]`，若此前没有 `response.completed`/`response.incomplete`，仍会按缺失正常终态处理为错误并可能进入重试。

### 6.11 完整本地工具循环示例

第一次请求：

```json
{
  "model": "<model-id>",
  "input": [
    {"type": "message", "role": "user", "content": "读取 README 并总结"}
  ],
  "tools": [
    {
      "type": "function",
      "name": "read_file",
      "description": "Read a file",
      "parameters": {
        "type": "object",
        "properties": {"path": {"type": "string"}},
        "required": ["path"]
      }
    }
  ],
  "tool_choice": "auto",
  "include": ["reasoning.encrypted_content"],
  "store": false,
  "stream": true
}
```

终态输出包含：

```json
[
  {
    "type": "reasoning",
    "id": "rs_1",
    "summary": [],
    "encrypted_content": "opaque",
    "status": "completed"
  },
  {
    "type": "function_call",
    "call_id": "call_abc",
    "name": "read_file",
    "arguments": "{\"path\":\"README.md\"}"
  }
]
```

执行工具后第二次请求应发送完整连续输入：

```json
{
  "model": "<model-id>",
  "input": [
    {"type": "message", "role": "user", "content": "读取 README 并总结"},
    {
      "type": "reasoning",
      "id": "rs_1",
      "summary": [],
      "encrypted_content": "opaque"
    },
    {
      "type": "function_call",
      "call_id": "call_abc",
      "name": "read_file",
      "arguments": "{\"path\":\"README.md\"}"
    },
    {
      "type": "function_call_output",
      "call_id": "call_abc",
      "output": "# README content..."
    }
  ],
  "tools": [
    {
      "type": "function",
      "name": "read_file",
      "description": "Read a file",
      "parameters": {
        "type": "object",
        "properties": {"path": {"type": "string"}},
        "required": ["path"]
      }
    }
  ],
  "include": ["reasoning.encrypted_content"],
  "store": false,
  "stream": true
}
```

## 7. Messages 兼容协议（条件能力，不代表所有 Grok 账号可用）

这一章记录客户端已经实现的 Messages 请求、响应和 SSE 转换能力。它**不构成** `api.x.ai` 或 Grok Build 对所有账号提供 `/v1/messages` 的证明。

仅在满足下列任一条件时才应使用本章的请求格式：

- 当前凭据返回的模型目录中，所选模型明确带有 `apiBackend: "messages"`，并在解析凭据后使用最终 sampling Base URL；
- 对 Grok Build 第一方代理执行了去敏、低成本能力探针，且当前账号/模型的 `/messages` 实际成功；
- 用户自定义了一个确实支持 Messages 协议的第三方/BYOK 模型；
- 你实现的是自己的兼容网关，并明确把 `/v1/messages` 标注为由其他上游协议转换出的模拟接口。

本仓库没有定义任何“订阅档位 → 原生协议路由”矩阵，也没有内置生产 Messages 模型。2026-08-12 的生产实测已经证明 `cli-chat-proxy` 存在该路由，但权限并不由归档标签或 `/models.api_backend` 完整表达：10 个认证有效的归档 free 账号调用 Messages 均为 403，而其中相同账号的 Responses/Chat 成功；另有部分归档 supergrok、实时 `/user.subscriptionTier` 缺失的账号 Messages 成功。故目录没有 `messages` 只表示客户端不会把该模型主路由选成 Messages，不能代表私有代理权限全集；账号池必须缓存实际探针结论及观测时间。

### 7.1 请求

```http
POST {base_url}/messages
```

字段：

| 字段 | 类型 | 是否必需 | 说明 |
|---|---|---|---|
| `model` | string | 是 | 内部为空时由配置补齐 |
| `messages` | array | 是 | 仅 `user`/`assistant` role |
| `max_tokens` | integer | 是 | 统一转换收到 `0` 时先取模型 `max_completion_tokens`，再回退 128000 |
| `system` | string/array | 否 | system 不放进 messages |
| `tools` | array | 否 | Anthropic 工具格式 |
| `tool_choice` | object | 否 | `auto`、`any`、指定 tool |
| `temperature` | number | 否 | 采样参数 |
| `top_p` / `top_k` | number | 否 | 采样参数 |
| `stream` | boolean | 流式时是 | 项目强制 true |
| `stop_sequences` | array | 否 | 停止序列 |
| `thinking` | object | 否 | 扩展思考配置 |
| `output_config` | object | 否 | effort/JSON Schema |
| `metadata.user_id` | string | 否 | provider metadata |

示例：

```json
{
  "model": "<model-id>",
  "system": [
    {
      "type": "text",
      "text": "You are a coding agent.",
      "cache_control": {"type": "ephemeral"}
    }
  ],
  "messages": [
    {
      "role": "user",
      "content": [
        {"type": "text", "text": "Read README"}
      ]
    }
  ],
  "tools": [
    {
      "name": "read_file",
      "description": "Read a file",
      "input_schema": {
        "type": "object",
        "properties": {"path": {"type": "string"}},
        "required": ["path"]
      }
    }
  ],
  "tool_choice": {"type": "auto"},
  "max_tokens": 32768,
  "thinking": {"type": "adaptive", "display": "summarized"},
  "output_config": {"effort": "high"},
  "stream": true
}
```

当前统一转换层将 reasoning effort `none`/`minimal` 省略，其余值放入 `output_config.effort`，并设置 adaptive thinking。

缓存断点不只来自上面 system 示例。当前转换还会给 system 尾块、最新可缓存消息以及前一个完整用户回合边界自动添加 `cache_control: {"type":"ephemeral"}`，并为服务端自动缓存预留一个 slot。

### 7.2 内容块

```json
{"type":"text","text":"..."}
{"type":"image","source":{"type":"base64","media_type":"image/png","data":"..."}}
{"type":"image","source":{"type":"url","url":"https://..."}}
{"type":"tool_use","id":"toolu_1","name":"read_file","input":{"path":"README.md"}}
{"type":"tool_result","tool_use_id":"toolu_1","content":"..."}
{"type":"thinking","thinking":"...","signature":"opaque"}
{"type":"redacted_thinking","data":"opaque"}
```

Messages API 的工具结果放在 `role: "user"` 消息中。并行工具结果可合并到同一个 user 内容块数组：

```json
{
  "role": "user",
  "content": [
    {"type": "tool_result", "tool_use_id": "toolu_1", "content": "result 1"},
    {"type": "tool_result", "tool_use_id": "toolu_2", "content": "result 2"}
  ]
}
```

工具 ID 在项目转换时会把字母数字、`_`、`-` 以外的字符替换为 `_`。

`redacted_thinking` 在线类型中可解析，但当前非流式归一化和流式 reducer 会丢弃 opaque data，不能承诺跨回合保真重放。

### 7.3 非流式响应

```json
{
  "id": "msg_...",
  "type": "message",
  "role": "assistant",
  "content": [
    {"type": "text", "text": "..."}
  ],
  "model": "<model-id>",
  "stop_reason": "end_turn",
  "usage": {
    "input_tokens": 100,
    "output_tokens": 20,
    "cache_creation_input_tokens": 50,
    "cache_read_input_tokens": 40
  }
}
```

停止原因：

```text
end_turn
max_tokens
tool_use
stop_sequence
refusal
pause_turn
model_context_window_exceeded
```

未知停止原因应原样保存，不能因枚举不认识而丢弃整个终态事件。

统一层把 `pause_turn` 视为一次停止，不会自动续传；`model_context_window_exceeded` 投影为 length/max-token 类截断。Messages wire usage 当前不携带 `cost_in_usd_ticks`。

### 7.4 SSE 事件

```text
message_start
content_block_start
content_block_delta
content_block_stop
message_delta
message_stop
ping
error
```

增量类型：

```json
{"type":"text_delta","text":"..."}
{"type":"input_json_delta","partial_json":"{\"path\":"}
{"type":"thinking_delta","thinking":"..."}
{"type":"signature_delta","signature":"..."}
```

按 `content_block_start.index` 建立内容块状态，再按相同 `index` 拼接 delta。`input_json_delta.partial_json` 完成前同样不能解析为 JSON。

当前 Messages 聚合器不强制看到 `message_stop` 才结束；严格兼容网关可以自行要求正常终态，避免无错误 EOF 被误当成功。

### 7.5 Messages 转换限制

- `ConversationToolChoice::None` 在当前统一转换中退化为 `{"type":"auto"}`，不能表达严格禁用工具；需要兼容网关额外处理。
- 服务端 hosted tools 没有一一对应的 Messages 表达，项目会将它们降级为 assistant 文本摘要。
- reasoning/thinking 重放需要同时保留文本和 signature；丢失 signature 会破坏后续思考上下文复用。
- 虽然类型支持 `output_config.format.json_schema`，项目注释指出在该路径直接使用 schema 可能抑制工具调用；Agent 场景通常改用一个 `StructuredOutput` 函数工具。

## 8. `/v1/models` 契约

请求：

```http
GET {base_url}/models
Authorization: Bearer <credential>
```

响应最外层：

```json
{
  "data": [
    {
      "id": "grok-model-id",
      "model": "grok-model-id",
      "name": "Display name",
      "description": "...",
      "baseUrl": "https://cli-chat-proxy.grok.com/v1",
      "apiBaseUrl": "https://api.x.ai/v1",
      "contextWindow": 256000,
      "maxCompletionTokens": 32768,
      "apiBackend": "responses",
      "reasoningEffort": "high",
      "supportsReasoningEffort": true,
      "reasoningEfforts": ["low", "medium", "high"],
      "supportsBackendSearch": true,
      "streamToolCalls": true,
      "supportedInApi": true,
      "hidden": false,
      "extraHeaders": {}
    }
  ]
}
```

解析器兼容 camelCase、snake_case，以及部分 `_meta` 内嵌字段。关键回退规则：

- 模型名：`model` → `modelId` → `id` → `_meta.model/modelId`；
- `baseUrl/base_url` 缺失时用当前推理 Base URL；`apiBaseUrl/api_base_url` 是 API-key 模式可用的第二端点；
- `contextWindow/context_window/_meta.totalContextTokens` 缺失时默认 256000；
- `apiBackend` 只识别 `responses`、`chat_completions`、`messages`，未知值回退 Chat Completions。

### 8.1 按账号动态发现协议能力

动态目录可能因身份而不同，但仓库不包含代理服务端，不能证明其生成规则。客户端能直接确认的是：`/settings.allow_access` 控制整体访问，settings 可提示默认模型，JWT tier 变化用于决定何时重拉目录。服务端是否据订阅档位开放某协议，只能从实际目录和请求结果确认。

`supportedInApi` 与 `hidden` 是**客户端 picker/ACP 可见性**提示，默认分别为 `true`/`false`。可见性公式为 `!hidden && (is_session_auth || supported_in_api)`；这里的 session auth 包括 WebLogin、OIDC 和声明第一方 issuer 的 External。它们不是服务端 ACL、路由存在性或推理成功的证明；`hidden` 模型也没有因此从内部 catalog 删除。

适配器应使用下面的保守流程：

1. 第一方动态目录使用对应的会话身份或 API-key 身份读取 `/v1/models`。BYOK、自定义 endpoint、每模型 `api_key/env_key/auth_provider` 主要依赖本地显式配置，目录抓取不会普遍复用每模型推理凭据。
2. 在当前凭据可见的模型中选择目标模型，并读取它的 `baseUrl`、`apiBaseUrl`、`apiBackend`、`supportedInApi`、`hidden` 等字段，再按实际凭据解析最终 sampling Base URL。
3. 严格按 `apiBackend` 路由：`responses` → `/responses`，`chat_completions` → `/chat/completions`，`messages` → `/messages`。
4. 目录中没有 `messages` 模型时，不把 Messages 当作该模型的默认上游协议；若确需利用 Grok Build 私有 Messages 路由，应另做一次最小能力探针并将结果按账号、模型、端点和观测时间缓存，不能靠固定套餐表开启。
5. 如果标准公开 `/models` 响应没有 `apiBackend` 扩展字段，应使用已知的供应商文档/本地显式配置；仍不能仅凭客户端实现猜成 `messages`。

`/models-v2` 不参与完整能力发现：它只在 session auth、闲置至少 600 秒且当前 Host 为 `cli-chat-proxy` 时读取，并只更新当前模型的 `context_window`/`max_completion_tokens`；BYOK/API-key 跳过。推理响应的 `x-models-etag` 变化则触发重新拉取标准 `/models`。

能力判断伪代码：

```text
catalog = GET first_party_models using corresponding_identity
model = catalog.visible_models.find(requested_model)
if model is absent:
    reject "model unavailable for current account"

sampling_base_url = resolve_base_url(model.baseUrl, model.apiBaseUrl, credentials)
switch model.apiBackend:
    responses        -> POST sampling_base_url + "/responses"
    chat_completions -> POST sampling_base_url + "/chat/completions"
    messages         -> POST sampling_base_url + "/messages"
```

不要硬编码“免费/某付费档一定有什么接口”。源码没有提供稳定、完整且可外推到未来版本的路由矩阵；当前目录是模型的首选路由证据，但最终授权仍以实际请求结果为准。实测中 `/models` 对所有有效账号都只声明 `api_backend: "responses"`，却仍有部分账号原生 Messages 成功，这正说明目录不是私有路由 ACL 全集。标准 OpenAI-compatible `/models` 可能只有 `id/object/owned_by`，缺少 `apiBackend/baseUrl` 时必须使用供应商文档、本地显式配置或受控探针，绝不能默认猜成 Messages。

如果你的本机网关只需要兼容常见客户端，返回标准 OpenAI 最小模型对象即可：

```json
{
  "object": "list",
  "data": [
    {"id": "grok-model-id", "object": "model", "owned_by": "xai"}
  ]
}
```

额外元数据可以保留，但不要依赖通用客户端读取它们。

## 9. 账号控制面：套餐、余额、额度与账号池状态

本章只适用于 Grok Build 第一方代理 `https://cli-chat-proxy.grok.com/v1` 的 OAuth 登录态。这些端点不是 OpenAI Responses API、不是 Anthropic Messages API，也不是公开 xAI API Key 的稳定计费契约。若做面向其他 Agent 客户端的本机兼容层，应由受信任的服务端持有 OAuth 凭据并调用控制面，不要把登录态交给浏览器或第三方客户端。

### 9.1 先拆开五类事实

同一个账号至少有五种彼此独立的状态，不能只存一个 `plan` 或 `status`：

| 事实 | 首选数据源 | 说明 |
|---|---|---|
| 当前订阅事实 | `GET /user?include=subscription` | live tier 独立于 JWT；明确非空值最有意义 |
| 当前凭据的展示套餐与访问门 | `GET /settings` | `subscription_tier_display` 是展示名；`allow_access` 才是 Grok Build 门控 |
| Access Token 当前性 | 401、token 时间字段、必要时的 refresh 结果 | token 失效不代表订阅消失 |
| 某推理协议当前可用性 | 对具体 endpoint + model 的实际请求 | 套餐名和 `/models` 都不能完整替代能力探针 |
| 当前额度周期和资金余额 | `GET /billing?format=credits` | 当前周期百分比、reset、预付余额、按量上限 |

这些来源不能串成一个“套餐降级链”，应按事实类型分别保存：

```text
订阅事实：只由新鲜且明确的 live /user 值确认；缺失/失败保持 unknown/stale
显示上下文：/settings.subscription_tier_display
token 快照：已验证 JWT tier（可能陈旧）
导入元数据：plan_type（仅归档标签，不确认当前订阅）
推理能力：实际 endpoint 探针 > 模型目录的主路由提示 > 套餐猜测
额度状态：live /billing + 实际 402/403 > 本地历史快照
```

`/user` 字段缺失、显式 `null`、空字符串和请求失败都不应被粗暴写成 `Free`。当前 Rust 类型会把 absent 与 `null` 都反序列化为 `None`，无法区分；安全的账号池状态是 `unknown` 或 `no_active_subscription_unconfirmed`。同样，导入 JSON 的 `plan_type` 只是归档标签，仓库生产代码并不消费它。

### 9.2 控制面公共请求头

billing 与 auto-topup 源码明确使用的请求头为：

```http
Authorization: Bearer <oauth-access-token>
X-XAI-Token-Auth: xai-grok-cli
x-userid: <authenticated-user-id>
x-grok-client-version: 1.0.1
x-grok-client-mode: headless
```

各端点有差异，不能把上述集合无条件复制到所有请求：`GET /user?include=subscription` 不发送 `x-userid`；settings/models 在认证资料含邮箱时可发送 `x-email`；settings 还发送 `x-grok-client-identifier`。billing、auto-topup 和 models 的对应源码路径不发送该 identifier。账号池仍不应使用邮箱作数据库主键或将它写入日志。

截至 2026-08-12，`GET /settings` 返回：

```json
{
  "min_client_version": "0.1.202",
  "force_update": true
}
```

这解释了低版本生成请求的 HTTP 426。控制面实现应先读取并遵守最低版本，不要永久硬编码本次值。

### 9.3 实时套餐：`GET /user?include=subscription`

请求：

```http
GET https://cli-chat-proxy.grok.com/v1/user?include=subscription
Authorization: Bearer <oauth-access-token>
X-XAI-Token-Auth: xai-grok-cli
x-grok-client-version: 1.0.1
x-grok-client-mode: headless
```

响应含用户资料；账号池只应提取所需字段，不记录整个响应：

```json
{
  "userId": "<sensitive-user-id>",
  "subscriptionTier": "GrokPro"
}
```

`subscriptionTier` 是可选字段。源码中的订阅检测规则为：非空并且不精确等于 `Free` 才算检测到 qualifying subscription。推荐标准化：

```text
非空、非 Free  -> paid_confirmed，保留原始枚举字符串
Free           -> free_confirmed
absent/null/"" -> unknown_or_no_active_subscription
401            -> credential_rejected，不改变上次套餐事实，只把它标为 stale
其他失败       -> unknown/stale，按退避重试
```

不要把 `GrokPro`、`SuperGrok`、`SuperGrokPro` 等字符串过早压成一个固定枚举。服务端 live tier、settings 展示名和 JWT claim 的命名可能不一致；本次实测就出现 `/user = GrokPro`、`/settings.subscription_tier_display = SuperGrok`。

### 9.4 远程策略与显示套餐：`GET /settings`

请求：

```http
GET https://cli-chat-proxy.grok.com/v1/settings
```

账号池关心的响应子集：

```json
{
  "allow_access": true,
  "subscription_tier_display": "Free",
  "on_demand_enabled": false,
  "default_model": "grok-4.5",
  "min_client_version": "0.1.202",
  "force_update": true,
  "usage_billing_redirect_url": null
}
```

字段语义：

| 字段 | 用法 |
|---|---|
| `allow_access` | 当前凭据能否进入 Grok Build；源码规则是仅显式 `false` 拒绝，缺失时 fail-open |
| `subscription_tier_display` | UI 展示名；OAuth 下通常从当前 JWT tier 规范化，不是独立的实时订阅事实 |
| `on_demand_enabled` | 是否展示/启用按量付费控制；直接 `/billing` 不会权威返回此 settings 字段 |
| `default_model` | 当前策略推荐的默认模型，不等于账号唯一可用模型 |
| `min_client_version` / `force_update` | 版本门控；生成请求遇到 426 时应重新取 settings |
| `usage_billing_redirect_url` | 存在时第一方 UI 可能转到外部计费页面，而不是展示内嵌 billing |

`allow_access=true` 只代表通过客户端访问门，不代表余额充足，也不证明 Responses、Chat 和 Messages 都成功。套餐升级可能先在 `/user` 出现，JWT 与 settings 稍后才更新；此时应记录 `token_tier_stale=true`，而不是把 live 付费订阅降级成 Free。

`/settings` 的失败分类也不要简化成通用 GET：401 在客户端中属于 `Rejected`；客户端可触发一次 auth refresh，但只有拿到与旧值不同的 token 才重新请求 settings。网络错误和 5xx 可按策略重试；403、429、其他 4xx，以及无法解析的 2xx 被归为 retry/未知结果，不能安装成新的 settings，也不能用失败值覆盖上一份新鲜套餐展示或访问门控。

仓库对 JWT 数字 `tier` claim 的显示映射为：

| 数字 | 标准化字符串 | 对应 live `/user` 名称 |
|---:|---|---|
| 0 | `free` | 不属于 qualifying paid tier |
| 1 | `supergrok` | `GrokPro` |
| 2 | `x_basic` | `XBasic` |
| 3 | `x_premium` | `XPremium` |
| 4 | `x_premium_plus` | `XPremiumPlus` |
| 5 | `supergrok_heavy` | `SuperGrokPro` |
| 6 | `supergrok_lite` | `SuperGrokLite` |
| 7 | `supergrok_plus` | `SuperGrokPlus` |

客户端这里只做 base64url 解码并读取 JSON，没有在这一步验证签名。账号池若用 JWT claim 作安全或路由决策，必须另外验证允许的算法、签名、issuer、audience、`exp`/`nbf` 以及 principal 绑定；否则它只能作为不可信显示提示。即使 refresh HTTP 成功，也只有新 JWT tier 与 live `/user` tier 相符时，才能认为套餐定向路由已同步。

订阅升级不是只靠 401 触发 refresh。精确链路是：`/user` 返回 200 并发现 qualifying paid tier → `single_check` 内第一次 best-effort refresh → 重拉 settings → 若 `allow_access=false` 则立即保持 gate 并停止 → 否则第二次显式 `refresh_chain` → 核对 JWT tier；claim 仍缺失或陈旧时继续后台退避刷新，匹配 live tier 后才刷新套餐定向的模型目录。账号池若复刻此链路，必须给 refresh 和 catalog 更新加 credential-generation fencing。

### 9.5 余额与周期：`GET /billing?format=credits`

真实 HTTP 请求：

```http
GET https://cli-chat-proxy.grok.com/v1/billing?format=credits
Authorization: Bearer <oauth-access-token>
X-XAI-Token-Auth: xai-grok-cli
x-userid: <authenticated-user-id>
x-grok-client-version: 1.0.1
x-grok-client-mode: headless
```

源码超时为 15 秒，不在该 handler 内自动重试。成功响应的兼容形状如下；可选字段必须按可选处理：

```json
{
  "config": {
    "creditUsagePercent": 42.5,
    "currentPeriod": {
      "type": "USAGE_PERIOD_TYPE_WEEKLY",
      "start": "2026-08-08T00:00:00Z",
      "end": "2026-08-15T00:00:00Z"
    },
    "monthlyLimit": {"val": 2000},
    "used": {"val": 1234},
    "onDemandCap": {"val": 500},
    "onDemandUsed": {"val": 0},
    "prepaidBalance": {"val": 1250},
    "isUnifiedBillingUser": true,
    "billingPeriodStart": "2026-08-01T00:00:00Z",
    "billingPeriodEnd": "2026-09-01T00:00:00Z",
    "productUsage": [
      {"product": "GrokBuild", "usagePercent": 61.2}
    ],
    "history": []
  }
}
```

直接 HTTP 与客户端内部 ACP 包装必须区分：

- 外部账号池直接请求 `/billing?format=credits`，顶层通常只有 `config`；
- 第一方 TUI 内部方法名是 `x.ai/billing`，不是 HTTP 路径；
- ACP handler 会从已缓存 `/settings` 补充 snake_case 顶层字段 `on_demand_enabled` 和 `subscription_tier`；直接 HTTP 不应期待这两个字段；
- 当前 Rust `BillingConfig` 会忽略未知字段；生产响应出现的 `productUsage`、`topUpMethod` 因而可能在 ACP 投影中丢失。直接读取 HTTP 时可以保留未知字段，但不能依赖未建模字段长期稳定。

字段解释与计算：

| 字段 | 单位/语义 | 推荐处理 |
|---|---|---|
| `creditUsagePercent` | 当前包含额度已用百分比 | 首选；clamp 到 0–100，保留原始 presence |
| `currentPeriod.type` | weekly 或 monthly 枚举字符串 | 原样保存，UI 再映射“当前周账期/当前月账期” |
| `currentPeriod.start/end` | RFC 3339 | `end` 是首选 reset；数据库保存原始带时区时间 |
| `monthlyLimit/used` | 旧版绝对额度，美元分 | 仅在新百分比缺失且 `monthlyLimit.val > 0` 时计算 `used / limit * 100` |
| `onDemandCap/onDemandUsed` | 按量上限/已用，美元分 | `onDemandUsed` 缺失时 legacy fallback 为 `max(used-monthlyLimit, 0)`；raw 值保留供审计，UI 金额按绝对值显示 |
| `prepaidBalance` | 预付余额，美元分 | 第一方 UI 用绝对值显示，以兼容会计负数；同时保留 raw 值供审计 |
| `isUnifiedBillingUser` | 是否统一共享用量池 | 不能据此自行制造 weekly + monthly 两个周期 |
| `productUsage` | 当前响应中的产品级百分比 | 开放字符串枚举；它不是模型级 token 统计，也没有独立 7D/30D 窗口 |
| `history` | 过去账期记录 | 可能是 legacy `billingCycle`，新响应也可能带 `period`；按开放对象保存 |

proto3 JSON 可能把零值 cents 编码为 `{}`。因此 `{}` 必须解析为 `val=0`，不能当响应损坏，同时应保存字段 presence 以区别 absent。`creditUsagePercent` 缺失也要保留“字段缺失”这一事实；第一方 UI 内部虽会退化成 0%，账号池契约仍应保存 `usage_pct_raw=null`、`usage_pct_source=unknown`，并向 UI 给非数值 placeholder。这个 placeholder 不参与排序、告警或聚合，也不能让服务端未返回的值看起来像明确 0%。

第一方 UI 的精确映射公式为：

```text
limit = monthlyLimit.val ?? 0
used  = used.val ?? 0

usage_pct =
  creditUsagePercent 存在 -> clamp(creditUsagePercent, 0, 100)
  否则 limit > 0          -> min(used / limit * 100, 100)
  否则                    -> 0（仅 UI fallback）

reset_at = currentPeriod.end ?? billingPeriodEnd
```

若开启按量付费，第一方 mapper 还派生 `effective_usage_pct`：包含额度尚未耗尽且新百分比存在时仍用 `usage_pct`；包含额度达到 100% 后改用派生的 `on_demand_used/onDemandCap`；旧版字段在包含额度未耗尽时用 `used/(monthlyLimit+onDemandCap)`。当前 UI 的 PAYG 剩余告警直接用 cap 与 derived used，状态 credit bar 使用 `usage_pct`；因此该兼容派生值不应覆盖原始 included usage，也不应被描述为唯一 UI 进度来源。

### 9.6 “余额”应拆成三类

后台截图里的一个“余额”列很容易混淆：

```text
included_remaining_pct = 100 - usage_pct
included_remaining_pct_display = max(100 - floor(usage_pct), 0)
prepaid_balance_usd    = abs(prepaidBalance.val) / 100
limit = monthlyLimit.val ?? 0
used = used.val ?? 0
cap_raw = onDemandCap.val ?? 0
pay_as_you_go = cap_raw > 0
derived_on_demand_used_raw = onDemandUsed.val ?? max(used - limit, 0)
on_demand_left_usd_display =
  pay_as_you_go ? max(abs(cap_raw) - abs(derived_on_demand_used_raw), 0) / 100 : null
```

前两项是同一百分比的分析值与 UI 值，都不是美元；第一方 UI 先 floor 已用百分比，所以 99.994% 显示 1% left。只有 legacy `monthlyLimit/used` 同时存在时，才能算包含额度的绝对剩余 cents。会计 raw 值与 UI display 值要分开保存，第一方摘要/告警对 prepaid、on-demand cap 和 used 使用绝对值。不要把百分比换算成虚构金额。最终是否可生成仍以实际请求为准：本次三个 `creditUsagePercent=100` 的账号都在 Responses 返回 HTTP 402 `Grok Build usage balance exhausted`。

### 9.7 自动充值：`GET /auto-topup-rule`

请求：

```http
GET https://cli-chat-proxy.grok.com/v1/auto-topup-rule
```

同 billing 请求头，源码超时 10 秒。响应：

```json
{
  "rule": {
    "enabled": true,
    "minBeforeHittingSl": {"val": 500},
    "topupAmount": {"val": 2000},
    "maxAmountPerMonth": {"val": 10000}
  }
}
```

`rule` 缺失表示没有规则；proto3 可省略 `enabled:false`，所以缺失 `enabled` 按 false；各 cents `{}` 按 0。第一方 UI 通常只在存在非零 prepaid credits 时再取 auto-topup。2026-08-12 的 19 个有效测试账号均返回 200，但都没有 `rule`。

### 9.8 7D/30D 与请求次数：不能从 billing 伪造

本次所有有效账号的 `/billing` 都只有一个 `currentPeriod`：

```text
type = USAGE_PERIOD_TYPE_WEEKLY
end - start = 7 days
history = []
```

所以：

- 可以显示“当前周账期用量”以及该周期的起止和 reset；
- 不能同时把同一条数据复制成“7D 用量”和“30D 用量”；
- `isUnifiedBillingUser=true` 也不表示响应里同时有 weekly 与 monthly 两条；
- `productUsage` 属于同一个当前账期，没有自己的 rolling window；
- billing 没有请求次数，截图中的“请求（7D）”必须来自自建网关日志；
- `x.ai/session/usage` 只是当前 Agent 进程内的 session ledger，新进程 resume 会重置，不能当账号级 7D/30D 账本。

账号池应按“逻辑 turn → generation/model call → transport attempt”分层记账。一个 turn 可因工具循环产生多次 generation，不能用 turn ID 把这些真实模型调用合并；同一 generation 的网络重试才属于 transport retry。推荐每个 generation 终态写一条可去重事件，并另存 attempt 明细：

```json
{
  "account_id": "acct_surrogate",
  "endpoint": "responses",
  "model": "grok-4.5",
  "event_id": "globally-unique-event-id",
  "logical_turn_id": "stable-logical-turn-id",
  "generation_id": "stable-within-transport-retries",
  "model_call_index": 2,
  "transport_attempt_count": 2,
  "status": "success",
  "input_tokens": 123,
  "cached_input_tokens": 50,
  "output_tokens": 45,
  "reasoning_tokens": 20,
  "cost_usd_ticks": null,
  "completed_at": "2026-08-12T12:00:00Z"
}
```

再按 UTC 半开区间 `[now-7d, now)` / `[now-30d, now)` 聚合 generation 次数、token 和已知成本。按唯一 `event_id` 幂等写入，并以 `generation_id` 合并同一 generation 的 transport retry；不同工具循环 model call 不去重。cost 缺失或 partial 不能按零计算。还要规定晚到事件、允许的时钟偏差、数据保留期，并同时保存 coverage 起点和 completeness；刚接入两天的账号不能展示成完整 30D 数据。

### 9.9 账号池推荐内部契约

建议给自己的后台暴露一个与上游隔离的内部对象，而不是把私有 OAuth 响应直接透传：

```json
{
  "account_id": "acct_surrogate",
  "credential": {
    "kind": "grok_oauth",
    "status": "valid",
    "expires_at": "2026-08-13T00:00:00Z",
    "last_http_status": 200,
    "observed_at": "2026-08-12T12:00:00Z"
  },
  "plan": {
    "live_subscription_tier": {"value": null, "source": "live_user_api", "observed_at": "2026-08-12T12:00:00Z", "freshness": "fresh"},
    "settings_display_tier": {"value": "Free", "source": "settings", "observed_at": "2026-08-12T12:00:00Z"},
    "jwt_tier": {"value": "free", "source": "verified_jwt", "observed_at": "2026-08-12T11:00:00Z"},
    "archive_label": {"value": "supergrok", "source": "import_file", "observed_at": "2026-08-12T10:00:00Z"},
    "token_tier_matches_live": null
  },
  "access": {
    "status": "allowed",
    "allow_access": true,
    "allow_access_source": "settings_explicit_true",
    "user_blocked_reason": null,
    "team_blocked_reasons": [],
    "observed_at": "2026-08-12T12:00:00Z"
  },
  "billing": {
    "status": "available",
    "usage_pct_raw": null,
    "usage_pct_source": "unknown",
    "usage_pct_ui_placeholder": "—",
    "period_type": "USAGE_PERIOD_TYPE_WEEKLY",
    "period_start": "2026-08-08T00:00:00Z",
    "reset_at": "2026-08-15T00:00:00Z",
    "prepaid_balance_cents_raw": 0,
    "on_demand_cap_cents": 0,
    "on_demand_used_cents": 0,
    "field_presence": {"prepaidBalance": true, "onDemandCap": true, "onDemandUsed": true},
    "is_unified_billing_user": true,
    "observed_at": "2026-08-12T12:00:00Z"
  },
  "capabilities": {
    "responses": {"status": "ok", "http": 200, "provider_code": null, "retry_after_seconds": null, "observed_at": "2026-08-12T12:00:01Z"},
    "chat_completions": {"status": "ok", "http": 200, "provider_code": null, "retry_after_seconds": null, "observed_at": "2026-08-12T12:00:02Z"},
    "messages": {"status": "unknown", "http": 403, "provider_code": "redacted_normalized_code", "retry_after_seconds": null, "observed_at": "2026-08-12T12:00:03Z"}
  },
  "models": {
    "routing_default": "grok-4.5",
    "selected_for_session": null,
    "available": ["grok-4.5"],
    "catalog_http_etag": "opaque-or-null",
    "last_inference_models_etag_hint": "opaque-or-null",
    "observed_at": "2026-08-12T12:00:00Z"
  },
  "rolling": {
    "requests_7d": 123,
    "requests_30d": 456,
    "usage_7d_source": "gateway_events",
    "usage_30d_source": "gateway_events",
    "coverage_started_at": "2026-07-01T00:00:00Z",
    "coverage_complete_30d": true
  },
  "last_inference_success_at": "2026-08-12T12:00:02Z",
  "last_error": null
}
```

推荐枚举分层：

```text
credential.status = valid | refresh_window | expired | refresh_required | invalid | refresh_failed
access.status     = allowed | gated | identity_blocked | unknown
billing.status    = available | exhausted | subscription_required | unknown
capability.status = ok | unauthorized | exhausted | subscription_required |
                    rate_limited | version_required | unavailable | untested
```

`allow_access` 缺失时，第一方 shell 为兼容性会 fail-open，但账号池记录仍应是 `access.status=unknown`、`allow_access_source=settings_absent_fail_open`，不能伪装成服务端显式 `allowed`。所有 proto3 标量都应另存 field presence，以区分字段缺失、`{}` 所表示的零和显式零；UI placeholder 不得参与排序、告警或 rolling 聚合。

最终列表里的单一“状态”只能是上述维度的投影，并应携带 reason、source 和 observed_at。例如：

```text
401 -> credential=refresh_required/stale；只有 IdP 明确 refresh token rejected/revoked 等证明凭据不可用时才 invalid；其他刷新永久错误记 refresh_failed 并按策略保留凭据
402 -> 只有 provider code/body 明确为余额耗尽时才 billing=exhausted，否则 unknown
403 -> access/policy/credit/subscription 之一；按安全化 provider code 分类，否则 unknown
426 -> client version_required；账号池建议重新取 settings，当前 sampler 没有专门的 426 恢复链
429 -> rate_limited 或 free_usage_exhausted，按错误体分类；不能永久禁用账号
2xx billing + 生成失败 -> 保留两个事实，不能让控制面健康覆盖推理失败
```

### 9.10 截图式后台字段映射

| 后台列 | 建议来源 | 注意事项 |
|---|---|---|
| 套餐 | live `/user?include=subscription` 的 `subscriptionTier`，展示可补 settings | 普通 `/user` 不保证返回订阅；缺失不是已确认 Free；各来源分别带时间 |
| 状态 | credential/access/billing/capability 聚合 | 至少显示最具体的失败原因 |
| 请求（7D） | 自建生成事件表 | Grok billing 不提供 request count |
| 7D 用量 | 自建 rolling 聚合；或明确标“当前周账期” | 两种口径不能混名 |
| 30D 用量 | 自建 rolling 聚合；或服务端真返回 monthly period 时标“当前月账期” | 不得复制 weekly 百分比 |
| 余额 | prepaid、on-demand、included remaining 分列 | 百分比不等于美元 |
| 模型 | `/models` 的 `available[]` + `routing_default` / `selected_for_session` | session 选择不是账号固有属性；目录不是私有协议 ACL 全集 |
| 更新时间 | 每个数据源各自 `observed_at` | 不存在统一的上游 account updated_at |

模型目录也必须按 account/principal/team、auth kind、origin/endpoint、protocol、credential generation 和 HTTP `ETag` 隔离缓存。推理响应的 `x-models-etag` 只是一条“目录可能变化”的刷新提示；完成重新读取 `/models` 前，不能把它写成当前 catalog snapshot 的 HTTP etag。仓库客户端的粗粒度本地模型缓存不适合作为并发多账号池的权威缓存。

### 9.11 2026-08-12：30 个匿名账号实测快照

测试归档实际包含 30 个 JSON，导入标签是 `20 free + 10 supergrok`。未输出 token、refresh token、邮箱、用户 ID 或文件名映射；没有尝试刷新 401 账号，因为 OAuth refresh 可能轮换 refresh token 并改变原始归档。

可复核的方法学摘要：

| 项目 | 本次设置 |
|---|---|
| 观测窗口 | 2026-08-12 20:37–20:47（Asia/Shanghai；各行以实际 `observed_at` 为准） |
| 归档证据 | ZIP SHA-256 `F029C056102C5417AA7964C381BDC10BF3EF894F394C52A492E36A84186CD353`；30 个 JSON |
| 源码基线 | `SOURCE_REV = 5d08d7e4123092567ccd584cd9f99afa2972065c` |
| 上游 | `https://cli-chat-proxy.grok.com/v1`，客户端版本 `1.0.1`，headless |
| 只读控制面 | `/user?include=subscription`、`/settings`、`/billing?format=credits`、`/auto-topup-rule`、`/models`；单请求超时 20 秒，最高并发 6 |
| 生成探针 | 模型 `grok-4.5`；Responses 使用 `max_output_tokens:1`、low reasoning、`store:false`；Chat/Messages 使用最大输出 1；最高并发 5，单请求超时 45 秒 |
| 分类 | 首先按 HTTP 状态，再只用去敏后的 provider 错误类别区分 balance exhausted、subscription/free-usage requirement、unauthorized 等；不保存完整错误体 |
| 安全 | 不刷新凭据；不记录账号映射、原始响应、prompt 内容、token/email/user ID；探针会消耗少量额度，故结果是探针后的即时快照 |

该 hash 只用于确认测试输入版本，不应据此公开或分发凭据归档。账号池的正式测试记录还应按每个 probe 保存匿名 event ID、开始/完成时间、endpoint、HTTP、provider code、`Retry-After` 和超时分类。

控制面结果：

| 观测 | 数量 |
|---|---:|
| Access Token 通过 `/user`、`/settings`、`/billing` 认证 | 19 |
| Access Token 返回 401 | 11 |
| live `/user.subscriptionTier = GrokPro` | 3 |
| live tier 缺失、但 token 当前有效 | 16 |
| `/settings.allow_access = true` | 19 |
| settings 显示 `SuperGrok` | 3 |
| settings 显示 `Free` | 16 |
| billing 当前周期为 weekly 且 unified | 19 |
| billing history 非空 | 0 |
| auto-topup rule 存在 | 0 |

本次 billing 的 19 个成功响应均没有 legacy `monthlyLimit/used`。15 个未显式返回 `creditUsagePercent`，1 个为 1%，3 个为 100%；后 3 个正好在 Responses 探针返回余额耗尽。只有少数响应带 `productUsage`，观测到的产品字符串包括 `GrokBuild`、`GrokChat`、`GrokImagine`，不能把它们当模型 ID。

同一轮最小生成探针：

| 归档标签 | Responses | Chat Completions | Messages |
|---|---|---|---|
| 20 free | 10 成功、10×401 | 10 成功、10×401 | 10×403、10×401 |
| 10 supergrok | 6 成功、3×402、1×401 | 6 成功、3×403、1×401 | 5 成功、1×403、3×402、1×401 |
| 合计 | **16 成功** | **16 成功** | **5 成功** |

因此“到底几个账号能用”必须说明口径：当前有 **19/30 个凭据能通过认证**，其中 **16/30 个当前能完成 Responses/Chat 推理**；另外 3 个凭据有效但 Grok Build 用量已耗尽。原生 Messages 当前只有 5 个成功，且不能由 live tier 或 `/models` 单独预测。这个快照会随 token 到期、账期 reset、消费和服务端策略改变，不应成为永久套餐规则。

所有 19 个有效账号的 `/models` 当前返回同一主项：

```json
{
  "id": "grok-4.5",
  "object": "model",
  "owned_by": "xAI",
  "model": "grok-4.5",
  "context_window": 500000,
  "api_backend": "responses",
  "reasoning_effort": "high",
  "supports_reasoning_effort": true
}
```

即使 Messages 成功的账号，目录仍声明 `api_backend: responses`。推理响应还观测到 `x-grok-context-window: 500000` 和 `x-models-etag`；后者应作为 opaque 的刷新提示保存，不要解释其内部格式，也不要与 `/models` HTTP `ETag` 混为 catalog snapshot etag。

### 9.12 探测、轮询与凭据安全

推荐调度：

1. 导入账号时默认只读 `/user?include=subscription`、`/settings`、`/billing`、`/models`。生成探针会消费额度并改变被测状态，必须显式 opt-in，设置总预算、低并发、最短输出，并把 probe event 纳入用量账本。
2. 对 consumer grok.com xAI OAuth，普通 turn 完成后异步刷新 billing，不要求推理结果成功；credit-limit、free-usage、reauth/reconnect 等提前返回分支另行处理。这与第一方 TUI 行为一致，不能外推到 API key/team/external auth。
3. `usage_pct >= 99` 时可每 30 秒刷新当前活跃账号；低于阈值不应高频轮询全池。
4. 免费/付费墙待升级账号可约 60 秒检查 `/user`；第一方付费墙短期流程为 5 秒一次、最多 10 分钟，不应成为长期全池频率。
5. models 的仓库缓存 TTL 是 300 秒；settings 使用约 5 分钟是账号池设计建议而非现有完整 settings TTL。401、426、HTTP `ETag`/`x-models-etag` 提示变化、账号切换或订阅变化时应按来源失效并重新验证。
6. 401 后是否自动 refresh 是有状态写操作：refresh token 可能轮换。只有具备跨进程单账号互斥、原子且 crash-safe 持久化、CAS/credential-generation fencing 的凭据服务才应执行；旧 refresh 的迟到结果不得覆盖新凭据，一旦已向 IdP 发送 refresh token，也不能假定旧 token 可安全回滚或复用。本次 fixture 测试没有刷新。
7. 429、5xx 和网络错误采用带抖动退避；不让多个探针同时打同一账号。

安全边界：

- OAuth access/refresh token 只在服务端加密保存，逐账号隔离；
- UI 和其他 Agent 客户端只拿账号池自签发的短期本机/内部凭据；
- 日志只记录不可逆 surrogate ID、HTTP 状态、字段 presence、事件 type 和时间；
- 不记录上游完整错误体，因为它未来可能包含身份信息；
- 禁止下游覆盖上游 Host、Authorization、`X-XAI-Token-Auth` 和 `x-userid`；
- 缓存 key 必须包含 principal/team、auth kind、origin/endpoint、protocol、credential generation；切换 token 后不得复用旧套餐、模型或能力结论。

## 10. 错误契约与重试

### 10.1 HTTP 错误体

客户端兼容两类主要格式。

OpenAI 嵌套格式：

```json
{
  "error": {
    "message": "Incorrect API key provided",
    "type": "invalid_request_error",
    "code": "invalid_api_key",
    "param": null
  }
}
```

代理扁平格式：

```json
{
  "code": "invalid-argument",
  "error": "Request rejected"
}
```

还会容忍 `message`、`detail`、`msg`、`description` 等常见 provider 变体。HTML/纯文本错误页不应原样返回给最终用户；按 HTTP 状态生成安全短消息。

### 10.2 流内错误

HTTP 已经 200 后仍可能收到：

```text
data: {"error":{"message":"...","type":"stream_error","code":"..."}}
```

或 Responses 的 `response.failed` / `response.error`。网关必须把它们作为失败终态，不能继续发 `[DONE]` 假装成功。

### 10.3 建议重试分类

当前项目的默认规则：

- 通用默认预算 `DEFAULT_MAX_RETRIES = 15`，计数达到 15 时 Fatal，即默认最多约 14 次 generic retry；覆盖顺序为 `GROK_MAX_RETRIES` > 模型配置 > 默认，但具体调用方仍可显式传入更小预算；
- 重试：429、除 525/526 外的 5xx、可重试 transport 错误、EventStreamError/StreamError 和空响应；`IdleTimeout` 明确不重试；
- 429 默认阈值为 2 次**尝试**，按当前 `next_attempt >= 2` 实现即初始失败后最多实际重试 1 次；优先使用 `Retry-After`，解析上限 120 秒；
- generic 路径首轮会重建 HTTP/1.1 客户端，退避约 2、4、8、16 秒，之后单次约 30 秒并带抖动；generic `Retry-After` 先截断至 30 秒再抖动；
- `x-should-retry: false` 是 veto；`true` 不会强制重试原本不可重试的状态；
- 不重试：400、401、403、404、408、422、序列化错误、上下文超限、最大输出截断和 idle timeout；
- 413 或明确的图片处理错误：剥离图片后最多进行一次恢复重试；
- 401 是令牌失效/错误，403 是已经鉴权但无权限，不能把 403 当成刷新令牌信号。

本机兼容网关建议用更保守的小预算，例如普通 5xx 最多 2～3 次，避免第三方客户端自身重试与网关重试叠加。

默认 `retry_only_before_output = false`，所以失败 attempt 已投影的 token/tool delta 之后仍可能整轮重试，造成下游重复片段。转换网关应选择“观察到输出后停止重试”，或为每次 attempt 建立重置/去重边界。

## 11. 三种协议的字段映射

| 统一语义 | Chat Completions | Responses | Messages |
|---|---|---|---|
| system | `messages.role=system` | `input` message role=system | 顶层 `system` |
| user text | `role=user, content` | input message | `role=user` 内容块 |
| assistant text | `role=assistant` | input/output message | `role=assistant` 内容块 |
| 本地工具定义 | `tools[].function` | `tools[]` 顶层 function | `tools[].input_schema` |
| 工具调用 ID | `tool_calls[].id` | `function_call.call_id` | `tool_use.id` |
| 工具参数 | JSON 字符串 | JSON 字符串 | JSON 对象；流式为 partial JSON |
| 工具结果 | `role=tool` | `function_call_output` | user 的 `tool_result` |
| 可见推理 | `reasoning_content` | reasoning summary/text | thinking 块 |
| 加密推理 | 无可靠表达 | `encrypted_content` | thinking signature/redacted data |
| 图片 | `image_url` | `input_image` | image source |
| 最大输出 | `max_tokens` | `max_output_tokens` | `max_tokens` |
| JSON Schema | `response_format` | `text.format` | `output_config.format`，Agent 工具场景有限制 |
| 完成原因 | `finish_reason` | response `status` + output | `stop_reason` |

## 12. 本机兼容网关建议契约

### 12.1 对下游暴露

```text
http://127.0.0.1:<port>/v1/models
http://127.0.0.1:<port>/v1/chat/completions
http://127.0.0.1:<port>/v1/responses
http://127.0.0.1:<port>/v1/messages
```

这里列出的是**自建网关可以选择实现的下游表面**，不是在声明上游 Grok 账号原生拥有全部四个端点。网关启动或刷新模型目录后，应为每个下游模型记录：

```text
native_backend     = responses | chat_completions | messages
exposed_protocols  = 原生协议 + 网关确实实现且测试过的转换协议
```

例如，某个实际账号的目录只有 Responses 模型时，可以由网关暴露 `/v1/messages` 并执行 Messages → Responses 转换；但文档和诊断信息必须写成“网关模拟的 Messages-compatible 接口”，不能据此声称该账号原生支持 `/v1/messages`。

下游只使用一个本机密钥：

```http
Authorization: Bearer <LOCAL_GATEWAY_TOKEN>
```

网关内部再映射为上游公开 API Key、第一方登录态或 deployment key。

### 12.2 推荐内部标准模型

不要以 Chat 消息作为内部唯一真相，否则会丢失 Responses reasoning 和 hosted tools。内部应至少保存：

```ts
type TurnItem =
  | { type: "message"; role: "system" | "user" | "assistant"; content: unknown }
  | { type: "reasoning"; raw: unknown }
  | { type: "function_call"; callId: string; name: string; arguments: string }
  | { type: "function_call_output"; callId: string; output: unknown }
  | { type: "hosted_tool_call"; raw: unknown };
```

高保真网关应以 Responses 输出顺序为权威存储，按客户端能力投影到 Chat 或 Messages。不要误认为 Grok Build 当前归一化层已经无损保留 MCP/未知 hosted-tool output。

### 12.3 转换流水线

```text
下游请求
  -> 鉴权、限流、请求体上限
  -> 解析为统一 TurnItem
  -> 选择模型和上游协议
  -> 注入上游鉴权与 x-grok 跟踪头
  -> 发送请求
  -> 解析 HTTP/SSE
  -> 一边投影下游 delta，一边构建权威终态
  -> 尽量保存原始 Responses 项/工具状态，并记录无法保真的类型
  -> 转发 usage、finish reason 和安全化错误
```

### 12.4 Chat → Responses 核心映射

```text
system/user/assistant message -> Responses message
assistant.tool_calls[]        -> function_call
tool message                  -> function_call_output
Chat tools[].function         -> Responses function tool
max_tokens                    -> max_output_tokens
response_format               -> text.format
```

额外规则：

- `tool_call_id` 必须能找到历史 function call；找不到返回 400，不要猜；
- 同一次 assistant 的并行工具调用保留原顺序；
- 工具参数只在拼接完整后校验 JSON；
- 转换服务生成的 `call_id`、response ID、conversation ID 必须在会话内稳定；
- 若下游 Chat 客户端无法接收 reasoning，可丢弃可见 reasoning delta，但内部必须保留 Responses 原始 reasoning 项。

Grok Build 自身执行工具时，批准的普通工具会并发运行并按完成顺序排水；exit/plan 类尾部工具后置。权限拒绝或取消会取消同批后续调用并写入合成结果，普通执行错误则通常转换成 tool result 反馈模型后继续采样。若目标是复刻 Agent 行为，不能简单假定工具总按声明顺序串行执行。

### 12.5 Responses → Chat SSE 投影

建议映射：

```text
response.output_text.delta
  -> choices[0].delta.content

response.reasoning_summary_text.delta / response.reasoning_text.delta
  -> choices[0].delta.reasoning_content

response.output_item.added(function_call)
  -> choices[0].delta.tool_calls[index].{id,type,function.name}

response.function_call_arguments.delta
  -> choices[0].delta.tool_calls[index].function.arguments

response.completed
  -> final chunk: finish_reason + usage
  -> data: [DONE]
```

完成原因映射：

```text
completed + function_call -> tool_calls
completed                 -> stop
incomplete                -> length
failed/error              -> 下游错误事件或断流错误，不映射为 stop
```

## 13. 可直接验证的请求示例

### 13.1 其他 Agent 客户端的配置值

直接使用公开 xAI API：

```text
Provider/API type: OpenAI compatible
Base URL:          https://api.x.ai/v1
API Key:           <正式 XAI_API_KEY>
Model:             先从 GET /v1/models 选择
API endpoint:      按目标模型的官方 xAI 文档/目录确认；若模型原生支持 Responses 且需要 reasoning/tool replay，可优先 Responses
Streaming:         enabled
```

使用本机兼容网关：

```text
Provider/API type: OpenAI compatible
Base URL:          http://127.0.0.1:<port>/v1
API Key:           <LOCAL_GATEWAY_TOKEN>
Model:             网关在 GET /v1/models 暴露的 ID
```

常见环境变量形式：

```bash
export OPENAI_BASE_URL="https://api.x.ai/v1"
export OPENAI_API_KEY="$XAI_API_KEY"
```

不同 Agent 客户端的 Base URL 约定并不统一：有些要求输入带 `/v1` 的 API 根，有些会自行追加 `/v1`。本仓库采样器要求 Base URL 已带 `/v1`，只再追加 `responses`、`chat/completions` 或 `messages`。接入前查看目标客户端最终请求 URL，避免 `/v1/v1/...` 或漏掉 `/v1`。

Anthropic SDK/客户端尤其常把“服务根 URL”和“API 根 URL”分开配置。目标是确保最终请求精确落到：

```text
https://<host>/v1/messages
```

若目标客户端只有以下开关，选择方法是：

| 客户端能力 | 选择 |
|---|---|
| `OpenAI Responses compatible` / `/responses` | 当官方文档或丰富目录确认目标模型为 Responses backend 时使用；Host 仍是 xAI/Grok 或代理 |
| `OpenAI` / `Chat Completions` | 使用 `/chat/completions` |
| `Anthropic` / `Claude Messages` | 目标模型标为 `messages`，或 Grok Build 私有 Messages 能力探针已成功时，才可原生直连；否则连接实现了转换的本机网关 |
| 仅支持自定义 HTTP，但不支持 SSE | 先用非流式；Agent 工具体验会下降 |
| 不能处理工具调用 | 只能用于纯聊天，不能完整驱动 coding agent |

### 13.2 curl 验证

使用公开 API Key 的 Responses 请求：

```bash
curl -N https://api.x.ai/v1/responses \
  -H "Authorization: Bearer $XAI_API_KEY" \
  -H "Content-Type: application/json" \
  -H "Accept: text/event-stream" \
  -d '{
    "model": "<model-id>",
    "input": [{"type":"message","role":"user","content":"Say hello"}],
    "include": ["reasoning.encrypted_content"],
    "store": false,
    "stream": true
  }'
```

Chat Completions：

```bash
curl -N https://api.x.ai/v1/chat/completions \
  -H "Authorization: Bearer $XAI_API_KEY" \
  -H "Content-Type: application/json" \
  -H "Accept: text/event-stream" \
  -d '{
    "model": "<model-id>",
    "messages": [{"role":"user","content":"Say hello"}],
    "stream": true,
    "stream_options": {"include_usage": true}
  }'
```

模型名称应先通过正式 `/v1/models` 或你的账户文档确认，不要把仓库内部 slug 当成公开 API 一定可用的模型 ID。

## 14. 实现检查清单

- [ ] Base URL 包含 `/v1`，路径不会重复为 `/v1/v1/...`
- [ ] 公开 API Key 与 Grok 登录态严格分仓保存
- [ ] 真实上游令牌不会进入第三方客户端或访问日志
- [ ] SSE 支持 CRLF/LF、跨 TCP 分片、UTF-8 BOM 和 `[DONE]`
- [ ] Chat 工具参数按 tool index 拼接
- [ ] Responses 工具参数按 output index 拼接
- [ ] Messages 内容按 block index 拼接
- [ ] `response.completed.response.output` 作为 Responses 最终权威数据
- [ ] 可保真的 reasoning/hosted tool 保持原顺序；MCP/未知项的当前丢失限制已显式记录
- [ ] 401 与 403 分开处理
- [ ] 402 余额耗尽、403 条件能力/订阅要求和 426 客户端版本门控分开处理
- [ ] `Retry-After` 和 `x-should-retry` 被尊重
- [ ] 请求体设置上限，图片 data URI 不进入日志
- [ ] 下游断开时取消上游请求
- [ ] 多工具并行调用不会相互串参数
- [ ] usage 中计费 token 与实时上下文 token 不混用
- [ ] 套餐事实、凭据状态、访问门、billing 和逐协议能力分别存储且各带 `observed_at`
- [ ] weekly/monthly 当前账期没有被伪装成 rolling 7D/30D；请求次数来自自建事件表
- [ ] refresh token 轮换使用单账号互斥、原子且 crash-safe 持久化；已发送给 IdP 的旧 token 不回滚复用；日志不含 access/refresh token
- [ ] 转换损失有明确策略，而不是静默伪造字段

## 15. 源码依据索引

| 主题 | 源码 |
|---|---|
| 三协议 HTTP 路径、请求头、SSE、错误处理 | `crates/codegen/xai-grok-sampler/src/client.rs` |
| 鉴权方案、采样配置 | `crates/codegen/xai-grok-sampler/src/config.rs` |
| Chat 请求/响应/流式类型 | `crates/codegen/xai-grok-sampling-types/src/types.rs` |
| 统一会话模型 | `crates/codegen/xai-grok-sampling-types/src/conversation.rs` |
| Chat 转换 | `crates/codegen/xai-grok-sampling-types/src/conversation/chat_completions.rs` |
| Responses 转换、推理/工具重放 | `crates/codegen/xai-grok-sampling-types/src/conversation/responses.rs` |
| Messages 转换与缓存断点 | `crates/codegen/xai-grok-sampling-types/src/conversation/messages.rs` |
| Messages 线协议类型 | `crates/codegen/xai-grok-sampling-types/src/messages.rs` |
| Provider 错误兼容解析 | `crates/codegen/xai-grok-sampling-types/src/provider_error.rs` |
| 重试分类 | `crates/codegen/xai-grok-sampler/src/retry.rs` |
| 模型目录、settings 请求和解析 | `crates/codegen/xai-grok-shell/src/remote/client.rs` |
| live 套餐 `/user?include=subscription` | `crates/codegen/xai-grok-shell/src/agent/subscription_check.rs`、`src/auth/model.rs` |
| billing、auto-topup 请求与响应类型 | `crates/codegen/xai-grok-shell/src/extensions/billing.rs` |
| billing 百分比、reset、PAYG 映射 | `crates/codegen/xai-grok-pager/src/app/effects/helpers.rs`、`src/views/credit_bar.rs` |
| 进程内 session usage（非账号累计） | `crates/codegen/xai-grok-shell/src/extensions/usage.rs`、`src/extensions/notification.rs` |
| 套餐/JWT tier 一致性与目录刷新 | `crates/codegen/xai-grok-shell/src/agent/mvp_agent/mod.rs` |
| 生产代理/WS 地址 | `crates/codegen/xai-grok-env/src/lib.rs` |
| 第一方登录态与 deployment key 头 | `crates/codegen/xai-grok-shell/src/util/grok_auth_credentials.rs` |
| 主会话请求构造、历史裁剪与 `prompt_cache_key` | `crates/codegen/xai-chat-state/src/actor/request_builder.rs` |
| 模型双端点、凭据与请求头覆盖 | `crates/codegen/xai-grok-shell/src/agent/config.rs` |
| `/models-v2` idle 刷新、etag 全目录刷新 | `crates/codegen/xai-grok-shell/src/session/acp_session_impl/session_setup.rs` |
| 工具并发、权限拒绝与错误反馈 | `crates/codegen/xai-grok-shell/src/session/acp_session_impl/tool_calls.rs` |

## 16. 置信度与未验证项

高置信度（源码直接定义或测试覆盖）：

- 客户端具备三个推理路径的构造/解析能力，以及各自的 HTTP 方法和流式模式；这不等于每个 Host、账号和模型都开放三个路径；
- `/responses` 使用 OpenAI Responses 兼容类型，但服务提供方由最终 sampling Base URL Host 决定；
- 主会话通常由 `apiBackend` 决定逐模型路由；`supportedInApi`/`hidden` 只直接证明客户端可见性规则；
- Bearer/`x-api-key` 两种鉴权；
- Grok 跟踪头；
- Chat/Responses/Messages 的核心字段、工具循环和 SSE 终态；
- 统一 Responses 路径的 `store:false`、加密 reasoning include，以及经 compaction/pruning 后的本地快照重放；
- `/models` 的解析字段；
- `/user`、`/billing`、`/auto-topup-rule` 的方法、路径、请求头、超时和客户端字段映射；
- 错误与重试策略。

本次已用匿名账号验证，但仍需按部署持续重新验证：

- 当前账号、模型与端点组合是否仍允许 Responses、Chat 或原生 Messages；本次已证明 `/models` 不是私有 Messages ACL 全集，任何套餐都不能靠名称预设能力；
- `/billing` 当前周期将来是否为 weekly 或 monthly、是否开始返回 history，以及新套餐是否引入新字段；
- 当前生产代理对每个扩展头是否仍全部强制；
- 某个具体模型支持哪套协议、reasoning effort、图片和 hosted tools；
- `x_search`、`stream_tool_calls` 等 xAI 扩展在公开 API 与代理中的可用范围；
- 服务端未在本仓库定义的套餐配额、计费、限流和内容安全策略；
- 第一方代理未来版本是否改变私有响应事件或模型目录字段。

因此正式适配器应保留抓取“去敏后的状态、事件 type、字段名、HTTP 状态与响应头”的调试模式，但永远不要记录令牌、完整提示词、图片 Base64、encrypted reasoning 或工具敏感输出。
