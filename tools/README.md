# CDK 自动兑换并导入 Codex2API

`redeem-and-import.mjs` 会按以下顺序执行：

1. 可选读取 `GET <CODEX2API_BASE_URL>/api/admin/health`，根据 `CODEX2API_TARGET_AVAILABLE` 或 `CODEX2API_TARGET_TOTAL` 判断是否还需要兑换。
2. 调用兑换平台 `POST /api/cdk/redeem`，每批默认提交 1 个 CDK，并为请求生成新的 `Idempotency-Key`。
3. 使用兑换响应的 `redemption_id` 与 `download_token` 下载 Sub2API JSON；支持响应中的多个 `downloads` 条目。
4. 在设置了 `group_ids` 时先校验分组是否存在，再用原生 `FormData` 上传到 `POST /api/admin/accounts/import`，自动处理 JSON 或 SSE 响应。

## 安全要求

- 不要把真实 CDK、`x-cdk-client`、下载 token 或 `X-Admin-Key` 写入源码、示例文件或 Git。
- 用户消息中展示过的凭据应视为已暴露，建议在兑换平台/Codex2API 侧立即轮换。
- `CDK_KEYS_FILE` 是敏感文件；建议放在 `tools/cdk-keys.txt`，并保持未跟踪。复制 `tools/redeem-and-import.example.env` 为 `tools/redeem-and-import.local.env` 后，脚本会自动读取其中的配置（已显式设置的环境变量优先）。
- 如果设置 `CDK_SAVE_DOWNLOADS_DIR`，下载的 Sub2API JSON 同样含有凭证，请限制目录权限并在使用后清理。

## 快速开始（PowerShell）

```powershell
# Node.js 22+
Copy-Item tools/redeem-and-import.example.env tools/redeem-and-import.local.env
Copy-Item tools/cdk-keys.txt.example tools/cdk-keys.txt
notepad tools/redeem-and-import.local.env  # 填写目标地址、管理密钥、x-cdk-client
notepad tools/cdk-keys.txt                # 把占位符替换成自己的 CDK

# 也可以直接用当前 PowerShell 会话覆盖配置
$env:CODEX2API_BASE_URL = 'https://your-codex2api.example'
$env:CODEX2API_ADMIN_KEY = '<从安全密码管理器注入>'
$env:CDK_CLIENT = '<从兑换平台请求复制 x-cdk-client>'
$env:CDK_KEYS_FILE = '.\tools\cdk-keys.txt'
$env:CODEX2API_GROUP_IDS = '[2]'
$env:CODEX2API_TARGET_AVAILABLE = '8'

# 先做无网络校验
node .\tools\redeem-and-import.mjs --dry-run

# 确认配置后执行兑换与导入
node .\tools\redeem-and-import.mjs
```

如果不需要按数量停止，可省略 `CODEX2API_TARGET_AVAILABLE`；脚本会依次处理文件中的全部 CDK。使用 `--force` 可临时忽略目标数量。默认每批 1 个码，确认兑换平台支持更大批次后再设置 `CDK_BATCH_SIZE`。

## 配置说明

| 变量 | 默认值 | 说明 |
| --- | --- | --- |
| `CDK_KEYS_FILE` | 无 | 每行一个 CDK；也可用 `CDK_KEYS` 传换行/逗号分隔字符串 |
| `CDK_REDEEM_URL` | `https://zzledu.kdns.fr/api/cdk/redeem` | 兑换接口 |
| `CDK_CLIENT` | 空 | 兑换平台要求时填写 `x-cdk-client` |
| `CODEX2API_BASE_URL` | `http://127.0.0.1:8080` | Codex2API 地址 |
| `CODEX2API_ADMIN_KEY` | 无 | 导入与状态检查的管理密钥 |
| `CODEX2API_GROUP_IDS` | 空 | `[2]` 或 `2,3`；空值不绑定分组 |
| `CODEX2API_TARGET_AVAILABLE` | 空 | 按 `/api/admin/health.available` 达到目标后停止 |
| `CODEX2API_TARGET_TOTAL` | 空 | 按 `/api/admin/health.total` 达到目标后停止；与上项二选一 |
| `CDK_BATCH_SIZE` | `1` | 每次提交的 CDK 数量，建议保守设置 |
| `CODEX2API_MAX_NEW_ACCOUNTS` | 空 | 本轮新增达到上限后停止 |
| `CDK_SAVE_DOWNLOADS_DIR` | 空 | 设置后保留下载文件；否则仅在内存中转发 |
| `CODEX2API_IMPORT_FORMAT` | `json` | 可选 `json_at` |
| `STOP_ON_ERROR` | `false` | 设为 `true` 时首个兑换/下载/导入错误立即停止 |
| `PANEL_HOST` | `127.0.0.1` | Web 面板监听地址；非回环地址必须配合 `PANEL_TOKEN` |
| `PANEL_PORT` | `8787` | Web 面板监听端口 |
| `PANEL_TOKEN` | 空 | 面板 API 令牌；远程/局域网访问时必须设置 |

状态目标使用“当前值 + 本轮已成功新增数”作为停止保护，因为新导入账号的健康探针可能尚未完成；下次运行前建议再次查看真实 `/api/admin/health`。

## 退出码

- `0`：全部批次完成，或已达到目标无需兑换。
- `1`：存在兑换、下载或导入错误（即使部分账号成功）。
- `2`：配置/参数错误，或运行环境不支持原生 `fetch`、`FormData`、`Blob`。

## Web 面板

启动本地面板（需要先设置与命令行相同的兑换/Codex2API 环境变量）：

```powershell
$env:PANEL_HOST = '127.0.0.1'
$env:PANEL_PORT = '8787'
# 如果 PANEL_HOST 设为 0.0.0.0 或局域网地址，必须再设置强随机令牌
# $env:PANEL_TOKEN = '<long-random-token>'
node .\tools\redeem-and-import-panel.mjs
# 浏览器打开 http://127.0.0.1:8787/
```

面板提供：

- `/api/admin/health` 的可用/总账号数展示；
- 兑换码数量与掩码预览（不会把完整 CDK 发送到浏览器）；
- **刷新状态、按配置执行、强制处理全部、停止任务、重载兑换码**；
- 最近兑换/下载/导入事件、逐码兑换状态与失败信息；
- `GET /api/events` SSE 接口，便于二次开发实时订阅。

面板只把脱敏配置和掩码后的兑换码返回给浏览器，不返回 `CODEX2API_ADMIN_KEY`、`CDK_CLIENT` 或下载 token。即便如此，也建议只监听回环地址；需要远程访问时请使用反向代理的 HTTPS，并设置 `PANEL_TOKEN`。
