#!/usr/bin/env node
/**
 * 兑换 CDK，下载 Sub2API JSON，并导入 Codex2API。
 *
 * 凭据只从环境变量或本地兑换码文件读取，不要把真实 CDK、下载 token、X-Admin-Key
 * 写入源码或提交到 Git。建议使用 Node.js 22+（仅使用原生 fetch/FormData/Blob）。
 */

import { randomUUID } from "node:crypto";
import { existsSync, readFileSync } from "node:fs";
import { chmod, mkdir, readFile, writeFile } from "node:fs/promises";
import { dirname, join, resolve } from "node:path";
import { fileURLToPath } from "node:url";

const DEFAULTS = Object.freeze({
  redeemUrl: "https://zzledu.kdns.fr/api/cdk/redeem",
  codexBaseUrl: "http://127.0.0.1:8080",
  importPath: "/api/admin/accounts/import",
  healthPath: "/api/admin/health",
  importFormat: "json",
  batchSize: 1,
  timeoutMs: 60_000,
  retries: 3,
  maxResponseBytes: 200 * 1024 * 1024,
  statusPollAttempts: 3,
  statusPollDelayMs: 1_000,
  userAgent: "codex2api-redeem-import/1.0",
});

function loadLocalEnvFile() {
  const cwdPath = resolve("tools/redeem-and-import.local.env");
  const modulePath = resolve(dirname(fileURLToPath(import.meta.url)), "redeem-and-import.local.env");
  const rawPath = process.env.REDEEM_IMPORT_ENV_FILE || (existsSync(cwdPath) ? cwdPath : modulePath);
  const path = resolve(rawPath);
  if (!existsSync(path)) return;
  let text;
  try {
    text = readFileSync(path, "utf8").replace(/^\uFEFF/, "");
  } catch {
    return;
  }
  for (const line of text.split(/\r?\n/)) {
    const trimmed = line.trim();
    if (!trimmed || trimmed.startsWith("#")) continue;
    const match = trimmed.match(/^(?:export\s+)?([A-Za-z_][A-Za-z0-9_]*)\s*=\s*(.*)$/);
    if (!match) continue;
    let value = match[2].trim();
    if ((value.startsWith("\"") && value.endsWith("\"")) || (value.startsWith("'") && value.endsWith("'"))) {
      value = value.slice(1, -1);
    }
    if (process.env[match[1]] === undefined) process.env[match[1]] = value;
  }
}

loadLocalEnvFile();

const secretValues = new Set();

class ConfigError extends Error {}

class RequestError extends Error {
  constructor(message, { status = 0, retryable = false, body = "" } = {}) {
    super(message);
    this.name = "RequestError";
    this.status = status;
    this.retryable = retryable;
    this.body = body;
  }
}

function registerSecret(value) {
  const text = String(value ?? "").trim();
  if (text.length >= 4) secretValues.add(text);
}

function maskSecret(value) {
  const text = String(value ?? "");
  if (!text) return "";
  if (text.length <= 8) return `${text.slice(0, 2)}…${text.slice(-2)}`;
  return `${text.slice(0, 4)}…${text.slice(-4)}`;
}

function redactText(value) {
  let text = String(value ?? "");
  for (const secret of [...secretValues].sort((a, b) => b.length - a.length)) {
    text = text.split(secret).join(`[REDACTED:${maskSecret(secret)}]`);
  }
  return text;
}

function log(level, message) {
  console.error(`[${new Date().toISOString()}] ${level.padEnd(5)} ${redactText(message)}`);
}

function sleep(ms) {
  return new Promise((resolvePromise) => setTimeout(resolvePromise, ms));
}

function ensureNotAborted(config) {
  if (config?.signal?.aborted) throw new RequestError("操作已取消", { retryable: false });
}

function emitEvent(options, event) {
  if (typeof options?.onEvent !== "function") return;
  try {
    options.onEvent({ at: new Date().toISOString(), ...event });
  } catch {
    // UI callbacks must never interrupt the redemption/import pipeline.
  }
}

function envValue(...names) {
  for (const name of names) {
    const value = process.env[name];
    if (value !== undefined && value !== "") return value;
  }
  return undefined;
}

function parseBoolean(raw, fallback = false) {
  if (raw === undefined || raw === null || String(raw).trim() === "") return fallback;
  switch (String(raw).trim().toLowerCase()) {
    case "1":
    case "true":
    case "yes":
    case "y":
    case "on":
      return true;
    case "0":
    case "false":
    case "no":
    case "n":
    case "off":
      return false;
    default:
      throw new ConfigError(`布尔参数值无效: ${raw}`);
  }
}

function parseInteger(raw, name, { min = Number.MIN_SAFE_INTEGER, max = Number.MAX_SAFE_INTEGER, defaultValue } = {}) {
  if (raw === undefined || raw === null || String(raw).trim() === "") return defaultValue;
  const text = String(raw).trim();
  if (!/^[+-]?\d+$/.test(text)) throw new ConfigError(name + " 必须是整数");
  const value = Number(text);
  if (!Number.isSafeInteger(value) || value < min || value > max) {
    throw new ConfigError(name + " 必须是 " + min + ".." + max + " 范围内的整数");
  }
  return value;
}

function normalizeHttpUrl(raw, name) {
  const value = String(raw ?? "").trim();
  if (!value) throw new ConfigError(`${name} 不能为空`);
  let parsed;
  try {
    parsed = new URL(value);
  } catch {
    throw new ConfigError(`${name} 不是合法 URL: ${value}`);
  }
  if (parsed.protocol !== "http:" && parsed.protocol !== "https:") {
    throw new ConfigError(`${name} 必须使用 http:// 或 https://`);
  }
  if (!parsed.hostname) throw new ConfigError(`${name} 缺少主机名`);
  return value.replace(/\/+$/, "");
}

// 保留 CODEX2API_BASE_URL 可能包含的路径前缀。
function appendPath(baseUrl, path) {
  const base = `${String(baseUrl).replace(/\/+$/, "")}/`;
  return new URL(String(path).replace(/^\/+/, ""), base).toString();
}

function parseGroupIds(raw) {
  const value = String(raw ?? "").trim();
  if (!value) return [];
  let parts;
  if (value.startsWith("[")) {
    try {
      parts = JSON.parse(value);
    } catch {
      throw new ConfigError("CODEX2API_GROUP_IDS 必须是 JSON 数组或逗号分隔整数");
    }
    if (!Array.isArray(parts)) throw new ConfigError("CODEX2API_GROUP_IDS 必须是 JSON 数组或逗号分隔整数");
  } else {
    parts = value.split(",");
  }

  const ids = [];
  const seen = new Set();
  for (const part of parts) {
    const text = String(part).trim();
    if (!/^\d+$/.test(text)) throw new ConfigError("CODEX2API_GROUP_IDS 只能包含正整数");
    const id = Number(text);
    if (!Number.isSafeInteger(id) || id <= 0) throw new ConfigError("CODEX2API_GROUP_IDS 只能包含正整数");
    if (!seen.has(id)) {
      seen.add(id);
      ids.push(id);
    }
  }
  return ids;
}

function parseKeysText(raw) {
  const text = String(raw ?? "").replace(/^\uFEFF/, "").trim();
  if (!text) return [];
  if (text.startsWith("[") || text.startsWith("{")) {
    try {
      const parsed = JSON.parse(text);
      const candidate = Array.isArray(parsed) ? parsed : parsed && Array.isArray(parsed.cdks) ? parsed.cdks : null;
      if (candidate) return candidate.filter((item) => typeof item === "string").map((item) => item.trim()).filter(Boolean);
    } catch {
      // 继续按换行格式解析，便于给出后续的空列表错误。
    }
  }
  return text
    .split(/\r?\n|,|;/)
    .map((line) => line.trim())
    .filter((line) => line && !line.startsWith("#") && !line.startsWith("//"));
}

function parseArgs(argv) {
  const args = { dryRun: false, force: false, noStatusCheck: false, help: false };
  const takeValue = (index, option) => {
    const value = argv[index + 1];
    if (value === undefined || value.startsWith("--")) throw new ConfigError(`${option} 需要一个值`);
    return value;
  };

  for (let index = 0; index < argv.length; index += 1) {
    const option = argv[index];
    switch (option) {
      case "-h":
      case "--help":
        args.help = true;
        break;
      case "--dry-run":
        args.dryRun = true;
        break;
      case "--force":
        args.force = true;
        break;
      case "--no-status-check":
        args.noStatusCheck = true;
        break;
      case "--keys-file":
        args.keysFile = takeValue(index, option);
        index += 1;
        break;
      case "--base-url":
        args.baseUrl = takeValue(index, option);
        index += 1;
        break;
      case "--target-available":
        args.targetAvailable = takeValue(index, option);
        index += 1;
        break;
      case "--target-total":
        args.targetTotal = takeValue(index, option);
        index += 1;
        break;
      case "--batch-size":
        args.batchSize = takeValue(index, option);
        index += 1;
        break;
      case "--group-ids":
        args.groupIds = takeValue(index, option);
        index += 1;
        break;
      case "--import-format":
        args.importFormat = takeValue(index, option);
        index += 1;
        break;
      default:
        throw new ConfigError(`未知参数: ${option}（使用 --help 查看用法）`);
    }
  }
  return args;
}

function buildConfig(args) {
  const dryRun = Boolean(args.dryRun);
  const redeemUrl = normalizeHttpUrl(envValue("CDK_REDEEM_URL") ?? DEFAULTS.redeemUrl, "CDK_REDEEM_URL");
  const baseUrl = normalizeHttpUrl(args.baseUrl ?? envValue("CODEX2API_BASE_URL") ?? DEFAULTS.codexBaseUrl, "CODEX2API_BASE_URL");
  const adminKey = String(envValue("CODEX2API_ADMIN_KEY") ?? "").trim();
  const cdkClient = String(envValue("CDK_CLIENT") ?? "").trim();

  if (!dryRun && !adminKey) throw new ConfigError("未设置 CODEX2API_ADMIN_KEY；请通过环境变量提供，不要写进脚本");
  if (adminKey) registerSecret(adminKey);
  if (cdkClient) registerSecret(cdkClient);

  const targetAvailableRaw = args.targetAvailable ?? envValue("CODEX2API_TARGET_AVAILABLE", "CODEX2API_MIN_AVAILABLE");
  const targetTotalRaw = args.targetTotal ?? envValue("CODEX2API_TARGET_TOTAL");
  const targetAvailable = parseInteger(targetAvailableRaw, "CODEX2API_TARGET_AVAILABLE", { min: 0, defaultValue: undefined });
  const targetTotal = parseInteger(targetTotalRaw, "CODEX2API_TARGET_TOTAL", { min: 0, defaultValue: undefined });
  if (targetAvailable !== undefined && targetTotal !== undefined) {
    throw new ConfigError("CODEX2API_TARGET_AVAILABLE 与 CODEX2API_TARGET_TOTAL 只能设置一个");
  }
  const targetMetric = targetAvailable !== undefined ? "available" : targetTotal !== undefined ? "total" : null;
  const targetValue = targetAvailable ?? targetTotal;
  const statusCheck = !args.noStatusCheck && parseBoolean(envValue("CHECK_CODEX2API_STATUS"), targetMetric !== null);
  if (!dryRun && statusCheck && !adminKey) throw new ConfigError("启用状态检查时必须设置 CODEX2API_ADMIN_KEY");

  const importFormat = String(args.importFormat ?? envValue("CODEX2API_IMPORT_FORMAT") ?? DEFAULTS.importFormat).trim().toLowerCase();
  if (!new Set(["json", "json_at"]).has(importFormat)) throw new ConfigError("CODEX2API_IMPORT_FORMAT 只能是 json 或 json_at");

  const config = {
    dryRun,
    force: Boolean(args.force),
    redeemUrl,
    redeemOrigin: new URL(redeemUrl).origin,
    cdkOrigin: normalizeHttpUrl(envValue("CDK_ORIGIN") ?? new URL(redeemUrl).origin, "CDK_ORIGIN"),
    cdkClient,
    codexBaseUrl: baseUrl,
    healthUrl: appendPath(baseUrl, envValue("CODEX2API_HEALTH_PATH") ?? DEFAULTS.healthPath),
    groupsUrl: appendPath(baseUrl, envValue("CODEX2API_GROUPS_PATH") ?? "/api/admin/account-groups"),
    importUrl: appendPath(baseUrl, envValue("CODEX2API_IMPORT_PATH") ?? DEFAULTS.importPath),
    adminKey,
    importFormat,
    groupIds: parseGroupIds(args.groupIds ?? envValue("CODEX2API_GROUP_IDS")),
    proxyUrl: String(envValue("CODEX2API_PROXY_URL") ?? "").trim(),
    allowDuplicate: parseBoolean(envValue("CODEX2API_ALLOW_DUPLICATE"), false),
    statusCheck,
    targetMetric,
    targetValue,
    batchSize: parseInteger(args.batchSize ?? envValue("CDK_BATCH_SIZE"), "CDK_BATCH_SIZE", { min: 1, max: 200, defaultValue: DEFAULTS.batchSize }),
    maxNewAccounts: parseInteger(envValue("CODEX2API_MAX_NEW_ACCOUNTS"), "CODEX2API_MAX_NEW_ACCOUNTS", { min: 1, defaultValue: undefined }),
    timeoutMs: parseInteger(envValue("HTTP_TIMEOUT_MS"), "HTTP_TIMEOUT_MS", { min: 1_000, max: 600_000, defaultValue: DEFAULTS.timeoutMs }),
    retries: parseInteger(envValue("HTTP_RETRIES"), "HTTP_RETRIES", { min: 1, max: 8, defaultValue: DEFAULTS.retries }),
    maxResponseBytes: parseInteger(envValue("MAX_RESPONSE_MB"), "MAX_RESPONSE_MB", { min: 1, max: 2_048, defaultValue: DEFAULTS.maxResponseBytes / (1024 * 1024) }) * 1024 * 1024,
    statusPollAttempts: parseInteger(envValue("STATUS_POLL_ATTEMPTS"), "STATUS_POLL_ATTEMPTS", { min: 0, max: 20, defaultValue: DEFAULTS.statusPollAttempts }),
    statusPollDelayMs: parseInteger(envValue("STATUS_POLL_DELAY_MS"), "STATUS_POLL_DELAY_MS", { min: 0, max: 60_000, defaultValue: DEFAULTS.statusPollDelayMs }),
    userAgent: String(envValue("CDK_USER_AGENT") ?? DEFAULTS.userAgent).trim(),
    saveDownloadsDir: String(envValue("CDK_SAVE_DOWNLOADS_DIR") ?? "").trim() || null,
    stopOnError: parseBoolean(envValue("STOP_ON_ERROR"), false),
    keysFile: args.keysFile ?? envValue("CDK_KEYS_FILE") ?? null,
  };

  if (config.saveDownloadsDir) config.saveDownloadsDir = resolve(config.saveDownloadsDir);
  if (config.keysFile) config.keysFile = resolve(config.keysFile);
  if (config.proxyUrl) normalizeHttpUrl(config.proxyUrl, "CODEX2API_PROXY_URL");
  return config;
}

async function loadKeys(config) {
  let raw = envValue("CDK_KEYS");
  if (!raw && config.keysFile) {
    try {
      raw = await readFile(config.keysFile, "utf8");
    } catch (error) {
      if (error?.code === "ENOENT") throw new ConfigError(`找不到 CDK_KEYS_FILE: ${config.keysFile}`);
      throw error;
    }
  }
  if (!raw) throw new ConfigError("没有兑换码；请设置 CDK_KEYS（换行/逗号分隔）或 CDK_KEYS_FILE");

  const keys = [];
  const seen = new Set();
  for (const key of parseKeysText(raw)) {
    if (seen.has(key)) continue;
    seen.add(key);
    keys.push(key);
    registerSecret(key);
  }
  if (keys.length === 0) throw new ConfigError("兑换码列表为空");
  if (keys.length > 10_000) throw new ConfigError("兑换码数量过多（最多 10000 个）");
  return keys;
}

function printHelp() {
  console.log(`用法（Node.js 22+）：
  node tools/redeem-and-import.mjs [选项]

必需配置（非 --dry-run）：
  CODEX2API_ADMIN_KEY       Codex2API 的 X-Admin-Key
  CDK_KEYS_FILE             兑换码文件（每行一个；也可用 CDK_KEYS）

常用配置：
  CODEX2API_BASE_URL         目标地址，默认 http://127.0.0.1:8080
  CDK_REDEEM_URL             兑换接口，默认 https://zzledu.kdns.fr/api/cdk/redeem
  CDK_CLIENT                 兑换平台要求的 x-cdk-client（可选，平台若校验则必须填）
  CODEX2API_GROUP_IDS        例如 [2] 或 2,3
  CODEX2API_TARGET_AVAILABLE 低于该可用账号数才继续兑换，达到后停止
  CODEX2API_TARGET_TOTAL      低于该总账号数才继续兑换（与 TARGET_AVAILABLE 二选一）
  CDK_BATCH_SIZE             每次兑换几个码，默认 1；为避免超额消耗，建议保守设置
  CDK_SAVE_DOWNLOADS_DIR     可选：把下载的凭证 JSON 归档到该目录

选项：
  --keys-file PATH           覆盖 CDK_KEYS_FILE
  --base-url URL             覆盖 CODEX2API_BASE_URL
  --target-available N       覆盖 CODEX2API_TARGET_AVAILABLE
  --target-total N           覆盖 CODEX2API_TARGET_TOTAL
  --batch-size N             覆盖 CDK_BATCH_SIZE
  --group-ids VALUE          覆盖 CODEX2API_GROUP_IDS
  --import-format FORMAT     json 或 json_at
  --force                    忽略目标数量，处理全部兑换码
  --no-status-check          不读取 Codex2API 状态
  --dry-run                  只校验配置并显示计划，不发网络请求
  -h, --help                显示帮助

PowerShell 示例：
  $env:CODEX2API_BASE_URL = 'https://your-codex2api.example'
  $env:CODEX2API_ADMIN_KEY = '从安全密码管理器临时注入'
  $env:CDK_CLIENT = '从兑换平台请求中复制 x-cdk-client'
  $env:CDK_KEYS_FILE = '.\\tools\\cdk-keys.txt'
  $env:CODEX2API_GROUP_IDS = '[2]'
  $env:CODEX2API_TARGET_AVAILABLE = '8'
  node .\\tools\\redeem-and-import.mjs
`);
}

function isRetryableStatus(status) {
  return status === 408 || status === 425 || status === 429 || status >= 500;
}

function retryDelayMs(attempt, retryAfterHeader) {
  const retryAfter = Number.parseFloat(String(retryAfterHeader ?? ""));
  if (Number.isFinite(retryAfter) && retryAfter >= 0) return Math.min(30_000, retryAfter * 1_000);
  return Math.min(10_000, 400 * 2 ** Math.max(0, attempt - 1));
}

async function readResponseBuffer(response, maxBytes) {
  const contentLength = Number.parseInt(response.headers.get("content-length") ?? "", 10);
  if (Number.isSafeInteger(contentLength) && contentLength > maxBytes) {
    throw new RequestError(`响应体超过限制（${Math.round(maxBytes / 1024 / 1024)} MiB）`, { status: response.status });
  }
  if (!response.body) {
    const arrayBuffer = await response.arrayBuffer();
    if (arrayBuffer.byteLength > maxBytes) throw new RequestError("响应体超过大小限制", { status: response.status });
    return Buffer.from(arrayBuffer);
  }

  const reader = response.body.getReader();
  const chunks = [];
  let total = 0;
  try {
    while (true) {
      const { done, value } = await reader.read();
      if (done) break;
      total += value.byteLength;
      if (total > maxBytes) {
        await reader.cancel();
        throw new RequestError(`响应体超过限制（${Math.round(maxBytes / 1024 / 1024)} MiB）`, { status: response.status });
      }
      chunks.push(Buffer.from(value));
    }
  } finally {
    reader.releaseLock();
  }
  return Buffer.concat(chunks, total);
}

async function readResponseText(response, maxBytes) {
  return (await readResponseBuffer(response, maxBytes)).toString("utf8");
}

async function requestWithRetry(url, initFactory, config, label) {
  let lastError = null;
  for (let attempt = 1; attempt <= config.retries; attempt += 1) {
    ensureNotAborted(config);
    const controller = new AbortController();
    const timeout = setTimeout(() => controller.abort(), config.timeoutMs);
    const abortExternal = () => controller.abort();
    if (config.signal) config.signal.addEventListener("abort", abortExternal, { once: true });
    try {
      const init = initFactory();
      init.signal = controller.signal;
      const response = await fetch(url, init);
      if (!isRetryableStatus(response.status) || attempt === config.retries) return response;

      const body = await readResponseText(response, Math.min(config.maxResponseBytes, 1024 * 1024));
      lastError = new RequestError(label + " HTTP " + response.status + ": " + redactText(body).slice(0, 240), {
        status: response.status,
        retryable: true,
        body,
      });
      log("WARN", label + " 第 " + attempt + "/" + config.retries + " 次失败，将重试（HTTP " + response.status + "）");
      await sleep(retryDelayMs(attempt, response.headers.get("retry-after")));
    } catch (error) {
      if (config.signal?.aborted) throw new RequestError("操作已取消", { retryable: false });
      if (error instanceof RequestError && error.status && !error.retryable) throw error;
      const timedOut = error?.name === "AbortError";
      lastError = new RequestError(label + (timedOut ? "超时" : "网络错误") + ": " + redactText(error?.message ?? error), { retryable: true });
      if (attempt === config.retries) throw lastError;
      log("WARN", label + " 第 " + attempt + "/" + config.retries + " 次失败，将重试");
      await sleep(retryDelayMs(attempt));
    } finally {
      if (config.signal) config.signal.removeEventListener("abort", abortExternal);
      clearTimeout(timeout);
    }
  }
  throw lastError ?? new RequestError(label + " 失败");
}

function parseJsonText(text, label) {
  const trimmed = String(text ?? "").replace(/^\uFEFF/, "").trim();
  if (!trimmed) throw new Error(`${label} 返回空响应`);
  try {
    return JSON.parse(trimmed);
  } catch (error) {
    throw new Error(`${label} 返回的不是有效 JSON: ${redactText(trimmed).slice(0, 240)} (${error.message})`);
  }
}

async function readJsonResponse(response, config, label) {
  const text = await readResponseText(response, config.maxResponseBytes);
  if (!response.ok) {
    throw new RequestError(`${label} HTTP ${response.status}: ${redactText(text).slice(0, 500)}`, { status: response.status, body: text });
  }
  return parseJsonText(text, label);
}

function safeFilename(raw, fallback) {
  const value = String(raw ?? "").trim();
  const leaf = value.replace(/^.*[\\/]/, "").replace(/[\u0000-\u001F<>:"|?*]/g, "_").trim();
  return leaf || fallback;
}

function filenameFromContentDisposition(header) {
  const value = String(header ?? "");
  const utf8 = value.match(/filename\*=UTF-8''([^;]+)/i);
  if (utf8) {
    try {
      return decodeURIComponent(utf8[1]);
    } catch {
      return utf8[1];
    }
  }
  const plain = value.match(/filename\s*=\s*"([^"]+)"/i) ?? value.match(/filename\s*=\s*([^;]+)/i);
  return plain?.[1]?.trim() ?? "";
}

function extractDownloadDescriptors(payload) {
  const raw = Array.isArray(payload?.downloads) && payload.downloads.length > 0 ? payload.downloads : [payload];
  const seen = new Set();
  const descriptors = [];
  for (const item of raw) {
    const redemptionId = String(item?.redemption_id ?? payload?.redemption_id ?? "").trim();
    const token = String(item?.download_token ?? payload?.download_token ?? "").trim();
    if (!redemptionId || !token) continue;
    const key = `${redemptionId}\u0000${token}`;
    if (seen.has(key)) continue;
    seen.add(key);
    registerSecret(token);
    descriptors.push({
      redemptionId,
      downloadToken: token,
      filename: String(item?.filename ?? payload?.filename ?? "").trim(),
      count: Number.isSafeInteger(item?.count) ? item.count : Number.isSafeInteger(payload?.count) ? payload.count : undefined,
    });
  }
  return descriptors;
}

async function redeemBatch(keys, config) {
  ensureNotAborted(config);
  const idempotencyKey = randomUUID();
  const body = JSON.stringify({ cdks: keys });
  const response = await requestWithRetry(
    config.redeemUrl,
    () => {
      const headers = {
        Accept: "application/json, */*",
        "Accept-Language": "en,zh-CN;q=0.9,zh;q=0.8",
        "Cache-Control": "no-cache",
        "Content-Type": "application/json",
        "Idempotency-Key": idempotencyKey,
        Origin: config.cdkOrigin,
        Pragma: "no-cache",
        Referer: config.cdkOrigin + "/",
        "User-Agent": config.userAgent,
      };
      if (config.cdkClient) headers["x-cdk-client"] = config.cdkClient;
      return { method: "POST", headers, body };
    },
    config,
    "兑换请求",
  );
  const payload = await readJsonResponse(response, config, "兑换请求");
  const descriptors = extractDownloadDescriptors(payload);
  const status = String(payload?.status ?? "").toLowerCase();
  if (descriptors.length === 0 && payload?.ok !== true && status !== "redeemed" && Number(payload?.count ?? 0) <= 0) {
    throw new Error(`兑换未返回可下载凭证（status=${status || "unknown"}）`);
  }
  return { payload, descriptors, idempotencyKey };
}

function buildDownloadUrl(config, redemptionId) {
  return appendPath(config.redeemOrigin, `/api/cdk/redemptions/${encodeURIComponent(redemptionId)}/download`);
}

async function downloadDescriptor(descriptor, config) {
  ensureNotAborted(config);
  const response = await requestWithRetry(
    buildDownloadUrl(config, descriptor.redemptionId),
    () => ({
      method: "GET",
      headers: {
        Accept: "application/json, */*",
        "Accept-Language": "en,zh-CN;q=0.9,zh;q=0.8",
        "Cache-Control": "no-cache",
        "x-cdk-download-token": descriptor.downloadToken,
        Origin: config.cdkOrigin,
        Pragma: "no-cache",
        Referer: config.cdkOrigin + "/",
        "User-Agent": config.userAgent,
      },
    }),
    config,
    `下载 ${descriptor.redemptionId}`,
  );
  const bytes = await readResponseBuffer(response, config.maxResponseBytes);
  if (!response.ok) {
    throw new RequestError(`下载 HTTP ${response.status}: ${redactText(bytes.toString("utf8")).slice(0, 500)}`, { status: response.status, body: bytes.toString("utf8") });
  }

  const parsed = parseJsonText(bytes.toString("utf8"), `下载 ${descriptor.redemptionId}`);
  if (!Array.isArray(parsed) && (!parsed || typeof parsed !== "object")) {
    throw new Error("下载内容不是 Sub2API JSON 对象或数组");
  }
  const headerFilename = filenameFromContentDisposition(response.headers.get("content-disposition"));
  const filename = safeFilename(descriptor.filename || headerFilename, `cdk-accounts-${descriptor.count ?? "download"}.json`);
  let savedPath = null;
  if (config.saveDownloadsDir) {
    await mkdir(config.saveDownloadsDir, { recursive: true });
    savedPath = join(config.saveDownloadsDir, `${Date.now()}-${randomUUID().slice(0, 8)}-${filename}`);
    await writeFile(savedPath, bytes, { mode: 0o600 });
    try {
      await chmod(savedPath, 0o600);
    } catch {
      // chmod is best effort on Windows and mounted filesystems.
    }
  }
  return { bytes, filename, savedPath, descriptor };
}

function parseSseResponse(text) {
  const events = [];
  const normalized = String(text ?? "").replace(/^\uFEFF/, "");
  for (const line of normalized.split(/\r?\n/)) {
    if (!line.startsWith("data:")) continue;
    const data = line.slice(5).trim();
    if (!data || data === "[DONE]") continue;
    try {
      events.push(JSON.parse(data));
    } catch {
      // 忽略非 JSON 的 keep-alive 帧。
    }
  }
  const summary = [...events].reverse().find((event) => event?.type === "complete") ?? events.at(-1) ?? {};
  return { kind: "sse", events, summary, raw: text };
}

function parseImportResponse(text, contentType) {
  const trimmed = String(text ?? "").trim();
  if (String(contentType ?? "").toLowerCase().includes("text/event-stream") || /^data:/m.test(trimmed)) {
    return parseSseResponse(text);
  }
  return { kind: "json", summary: parseJsonText(text, "Codex2API 导入"), events: [], raw: text };
}

function importStats(result) {
  const summary = result?.summary ?? {};
  const numberOrZero = (...values) => {
    for (const value of values) {
      const number = Number(value);
      if (Number.isFinite(number)) return number;
    }
    return 0;
  };
  return {
    success: numberOrZero(summary.success, summary.imported),
    updated: numberOrZero(summary.updated),
    duplicate: numberOrZero(summary.duplicate, summary.skipped),
    failed: numberOrZero(summary.failed, summary.errors),
    total: numberOrZero(summary.total, summary.requested_count),
  };
}

async function importDownloadedJson(download, config) {
  ensureNotAborted(config);
  const response = await requestWithRetry(
    config.importUrl,
    () => {
      const form = new FormData();
      form.append("format", config.importFormat);
      if (config.groupIds.length > 0) form.append("group_ids", JSON.stringify(config.groupIds));
      if (config.proxyUrl) form.append("proxy_url", config.proxyUrl);
      if (config.allowDuplicate) form.append("allow_duplicate", "true");
      form.append("file", new Blob([download.bytes], { type: "application/json" }), download.filename);
      return {
        method: "POST",
        headers: {
          Accept: "application/json, text/event-stream, */*",
          "X-Admin-Key": config.adminKey,
        },
        body: form,
      };
    },
    config,
    `导入 ${download.filename}`,
  );
  const text = await readResponseText(response, config.maxResponseBytes);
  if (!response.ok) {
    throw new RequestError(`导入 ${download.filename} HTTP ${response.status}: ${redactText(text).slice(0, 500)}`, { status: response.status, body: text });
  }
  const result = parseImportResponse(text, response.headers.get("content-type"));
  return { result, stats: importStats(result) };
}

async function getCodexStatus(config) {
  ensureNotAborted(config);
  const response = await requestWithRetry(
    config.healthUrl,
    () => ({
      method: "GET",
      headers: { Accept: "application/json", "X-Admin-Key": config.adminKey },
    }),
    config,
    "读取 Codex2API 状态",
  );
  const payload = await readJsonResponse(response, config, "读取 Codex2API 状态");
  const available = Number(payload?.available);
  const total = Number(payload?.total);
  if (!Number.isFinite(available) || !Number.isFinite(total)) {
    throw new Error(`Codex2API 状态响应缺少 available/total: ${redactText(JSON.stringify(payload)).slice(0, 300)}`);
  }
  return { available, total, status: String(payload?.status ?? "") };
}

async function validateImportGroups(config) {
  if (!config.groupIds || config.groupIds.length === 0) return;
  ensureNotAborted(config);
  const response = await requestWithRetry(
    config.groupsUrl,
    () => ({ method: "GET", headers: { Accept: "application/json", "X-Admin-Key": config.adminKey } }),
    config,
    "校验 Codex2API 分组",
  );
  const payload = await readJsonResponse(response, config, "校验 Codex2API 分组");
  const groups = Array.isArray(payload?.groups) ? payload.groups : [];
  const available = new Set(groups.map((group) => Number(group?.id)).filter((id) => Number.isSafeInteger(id) && id > 0));
  const missing = config.groupIds.filter((id) => !available.has(id));
  if (missing.length > 0) throw new Error("Codex2API 不存在分组 ID: " + missing.join(", "));
}

function projectedTargetValue(status, config, importedThisRun = 0, targetBaseline = null) {
  const actual = Number(status?.[config.targetMetric] ?? 0);
  const projected = targetBaseline === null ? actual : targetBaseline + importedThisRun;
  return Math.max(actual, projected);
}

function targetReached(status, config, importedThisRun = 0, targetBaseline = null) {
  if (config.force || !config.statusCheck || !config.targetMetric || config.targetValue === undefined) return false;
  return projectedTargetValue(status, config, importedThisRun, targetBaseline) >= config.targetValue;
}

function targetDeficit(status, config, importedThisRun = 0, targetBaseline = null) {
  if (config.force || !config.statusCheck || !config.targetMetric || config.targetValue === undefined) return Infinity;
  return Math.max(0, config.targetValue - projectedTargetValue(status, config, importedThisRun, targetBaseline));
}

function makeEmptyStats() {
  return {
    batches: 0,
    codesSubmitted: 0,
    redeemedCount: 0,
    downloadedFiles: 0,
    imported: 0,
    updated: 0,
    duplicate: 0,
    failed: 0,
    errors: 0,
    stoppedByTarget: false,
  };
}

function printDryRun(config, keys) {
  console.log(JSON.stringify({
    dry_run: true,
    redeem_url: config.redeemUrl,
    codex2api_base_url: config.codexBaseUrl,
    import_format: config.importFormat,
    group_ids: config.groupIds,
    key_count: keys.length,
    batch_size: config.batchSize,
    estimated_batches: Math.ceil(keys.length / config.batchSize),
    status_check: config.statusCheck,
    target_metric: config.targetMetric,
    target_value: config.targetValue ?? null,
    save_downloads_dir: config.saveDownloadsDir,
  }, null, 2));
}

async function pollStatus(config) {
  if (!config.statusCheck || config.force || config.statusPollAttempts === 0) return null;
  let latest = null;
  for (let attempt = 0; attempt < config.statusPollAttempts; attempt += 1) {
    if (attempt > 0) await sleep(config.statusPollDelayMs);
    latest = await getCodexStatus(config);
    log("INFO", `Codex2API 状态: available=${latest.available}, total=${latest.total}`);
  }
  return latest;
}

async function run(config, keys, options = {}) {
  const stats = makeEmptyStats();
  const emit = (event) => emitEvent(options, event);
  const cancelled = () => Boolean(config?.signal?.aborted);
  const failOrContinue = (error) => {
    const authFailure = error?.status === 401 || error?.status === 403;
    if (config.stopOnError || cancelled() || authFailure) throw error;
  };

  if (config.dryRun) {
    printDryRun(config, keys);
    emit({ type: "dry_run", key_count: keys.length });
    return stats;
  }

  emit({ type: "started", key_count: keys.length });
  await validateImportGroups(config);
  let status = null;
  let importedThisRun = 0;
  let targetBaseline = null;

  for (let offset = 0; offset < keys.length; ) {
    ensureNotAborted(config);
    if (config.maxNewAccounts !== undefined && stats.imported >= config.maxNewAccounts) {
      log("INFO", "已达到 CODEX2API_MAX_NEW_ACCOUNTS=" + config.maxNewAccounts + "，停止继续兑换");
      emit({ type: "stopped", reason: "max_new_accounts" });
      break;
    }

    if (config.statusCheck && !config.force) {
      status = await getCodexStatus(config);
      if (targetBaseline === null && config.targetMetric) targetBaseline = Number(status[config.targetMetric] ?? 0);
      log("INFO", "兑换前 Codex2API 状态: available=" + status.available + ", total=" + status.total);
      emit({ type: "status", available: status.available, total: status.total, phase: "before_redeem" });
      if (targetReached(status, config, importedThisRun, targetBaseline)) {
        stats.stoppedByTarget = true;
        log("INFO", "已达到目标账号数量，停止继续兑换");
        emit({ type: "stopped", reason: "target_reached", available: status.available, total: status.total });
        break;
      }
    }

    let count = Math.min(config.batchSize, keys.length - offset);
    const deficit = targetDeficit(status, config, importedThisRun, targetBaseline);
    if (Number.isFinite(deficit) && deficit > 0) count = Math.min(count, deficit);
    if (config.maxNewAccounts !== undefined) count = Math.min(count, config.maxNewAccounts - stats.imported);
    if (count <= 0) break;

    const batch = keys.slice(offset, offset + count);
    offset += count;
    stats.codesSubmitted += batch.length;
    emit({ type: "batch_start", batch: stats.batches + 1, count: batch.length, keys: batch.map(maskSecret) });
    log("INFO", "开始兑换第 " + (stats.batches + 1) + " 批（" + batch.length + " 个码：" + batch.map(maskSecret).join(", ") + "）");

    let redemption;
    try {
      redemption = await redeemBatch(batch, config);
      stats.batches += 1;
      const responseCount = Number(redemption.payload?.count);
      stats.redeemedCount += Number.isFinite(responseCount) ? responseCount : batch.length;
      const details = Array.isArray(redemption.payload?.details)
        ? redemption.payload.details.map((item) => ({ code: maskSecret(item?.code), status: String(item?.status ?? "") }))
        : [];
      emit({ type: "redeemed", batch: stats.batches, downloads: redemption.descriptors.length, count: Number.isFinite(responseCount) ? responseCount : batch.length, details });
      log("INFO", "兑换成功：下载任务 " + redemption.descriptors.length + " 个，requested=" + (redemption.payload?.requested_count ?? batch.length) + ", count=" + (redemption.payload?.count ?? "?"));
    } catch (error) {
      stats.errors += 1;
      stats.failed += batch.length;
      emit({ type: "error", phase: "redeem", message: redactText(error.message) });
      log("ERROR", "兑换批次失败：" + error.message);
      failOrContinue(error);
      continue;
    }

    if (redemption.descriptors.length === 0) {
      log("WARN", "兑换响应没有可下载任务，跳过该批");
      continue;
    }

    for (const descriptor of redemption.descriptors) {
      ensureNotAborted(config);
      let download;
      try {
        download = await downloadDescriptor(descriptor, config);
        stats.downloadedFiles += 1;
        emit({ type: "downloaded", filename: download.filename, count: descriptor.count ?? null });
        log("INFO", "下载完成：" + download.filename + (download.savedPath ? "（已归档到 " + download.savedPath + "）" : ""));
      } catch (error) {
        stats.errors += 1;
        stats.failed += 1;
        emit({ type: "error", phase: "download", message: redactText(error.message) });
        log("ERROR", "下载失败：" + error.message);
        failOrContinue(error);
        continue;
      }

      try {
        const imported = await importDownloadedJson(download, config);
        stats.imported += imported.stats.success;
        importedThisRun += imported.stats.success;
        stats.updated += imported.stats.updated;
        stats.duplicate += imported.stats.duplicate;
        stats.failed += imported.stats.failed;
        if (imported.stats.failed > 0) stats.errors += 1;
        emit({ type: "imported", filename: download.filename, ...imported.stats });
        log("INFO", "导入完成：新增 " + imported.stats.success + "，更新 " + imported.stats.updated + "，重复 " + imported.stats.duplicate + "，失败 " + imported.stats.failed);
      } catch (error) {
        stats.errors += 1;
        stats.failed += 1;
        emit({ type: "error", phase: "import", message: redactText(error.message) });
        log("ERROR", "导入失败：" + error.message);
        failOrContinue(error);
      }
    }

    if (config.statusCheck && !config.force) {
      status = await pollStatus(config);
      if (status) {
        if (targetBaseline === null && config.targetMetric) targetBaseline = Number(status[config.targetMetric] ?? 0);
        emit({ type: "status", available: status.available, total: status.total, phase: "after_import" });
      }
      if (status && targetReached(status, config, importedThisRun, targetBaseline)) {
        stats.stoppedByTarget = true;
        log("INFO", "导入后已达到目标账号数量，停止继续兑换");
        emit({ type: "stopped", reason: "target_reached", available: status.available, total: status.total });
        break;
      }
    }
  }

  ensureNotAborted(config);
  emit({ type: "complete", stats: { ...stats } });
  return stats;
}

function printSummary(stats) {
  console.log(JSON.stringify({
    batches: stats.batches,
    codes_submitted: stats.codesSubmitted,
    redeemed_count: stats.redeemedCount,
    downloaded_files: stats.downloadedFiles,
    imported: stats.imported,
    updated: stats.updated,
    duplicate: stats.duplicate,
    failed: stats.failed,
    errors: stats.errors,
    stopped_by_target: stats.stoppedByTarget,
  }, null, 2));
}

export {
  ConfigError,
  appendPath,
  buildConfig,
  extractDownloadDescriptors,
  getCodexStatus,
  importStats,
  loadKeys,
  makeEmptyStats,
  parseArgs,
  parseGroupIds,
  parseImportResponse,
  parseKeysText,
  redactText,
  run,
  safeFilename,
  validateImportGroups,
};

const isMain = process.argv[1] && resolve(process.argv[1]) === fileURLToPath(import.meta.url);
if (isMain) {
  if (typeof fetch !== "function" || typeof FormData !== "function" || typeof Blob !== "function") {
    console.error("需要 Node.js 18+ 的 fetch/FormData/Blob；建议使用 Node.js 22+");
    process.exitCode = 2;
  } else {
    try {
      const args = parseArgs(process.argv.slice(2));
      if (args.help) {
        printHelp();
      } else {
        const config = buildConfig(args);
        const keys = await loadKeys(config);
        const stats = await run(config, keys);
        printSummary(stats);
        process.exitCode = stats.errors > 0 ? 1 : 0;
      }
    } catch (error) {
      const prefix = error instanceof ConfigError ? "配置错误" : "执行失败";
      log("ERROR", `${prefix}：${error.message}`);
      process.exitCode = error instanceof ConfigError ? 2 : 1;
    }
  }
}
