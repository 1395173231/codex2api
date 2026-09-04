#!/usr/bin/env node
/**
 * 本地 Web 面板：展示兑换/Codex2API 状态，并启动、停止、重载自动导入任务。
 * 默认只监听 127.0.0.1；若监听非回环地址，必须设置 PANEL_TOKEN。
 */

import { createServer } from "node:http";
import { timingSafeEqual } from "node:crypto";
import { basename } from "node:path";
import {
  ConfigError,
  buildConfig,
  getCodexStatus,
  loadKeys,
  redactText,
  run,
} from "./redeem-and-import.mjs";

const DEFAULT_HOST = "127.0.0.1";
const DEFAULT_PORT = 8787;
const PANEL_HOST = String(process.env.PANEL_HOST || DEFAULT_HOST).trim();
const PANEL_TOKEN = String(process.env.PANEL_TOKEN || "").trim();
const PANEL_PORT_RAW = String(process.env.PANEL_PORT || DEFAULT_PORT).trim();
const PANEL_PORT = /^\d+$/.test(PANEL_PORT_RAW) ? Number(PANEL_PORT_RAW) : NaN;

if (!Number.isSafeInteger(PANEL_PORT) || PANEL_PORT < 1 || PANEL_PORT > 65_535) {
  throw new ConfigError("PANEL_PORT 必须是 1..65535 范围内的整数");
}

const isLoopbackHost = PANEL_HOST === "127.0.0.1" || PANEL_HOST === "::1" || PANEL_HOST.toLowerCase() === "localhost";
if (!isLoopbackHost && !PANEL_TOKEN) {
  throw new ConfigError("PANEL_HOST 不是回环地址时必须设置 PANEL_TOKEN");
}

const state = {
  startedAt: new Date().toISOString(),
  running: false,
  runStartedAt: null,
  runFinishedAt: null,
  lastError: null,
  configError: null,
  keyError: null,
  keysLoaded: 0,
  keyPreview: [],
  status: { available: null, total: null, status: null, fetchedAt: null, error: null },
  stats: null,
  events: [],
};

let config = null;
let keys = [];
let currentRun = null;
const eventClients = new Set();

function maskKey(value) {
  const text = String(value ?? "");
  if (!text) return "";
  if (text.length <= 8) return text.slice(0, 2) + "…" + text.slice(-2);
  return text.slice(0, 4) + "…" + text.slice(-4);
}

function publicUrl(raw) {
  try {
    const url = new URL(String(raw));
    url.username = "";
    url.password = "";
    url.search = "";
    url.hash = "";
    return url.toString();
  } catch {
    return "[invalid url]";
  }
}

function publicConfig() {
  if (!config) return null;
  return {
    codex2api_base_url: publicUrl(config.codexBaseUrl),
    redeem_url: publicUrl(config.redeemUrl),
    import_url: publicUrl(config.importUrl),
    group_ids: config.groupIds,
    import_format: config.importFormat,
    batch_size: config.batchSize,
    target_metric: config.targetMetric,
    target_value: config.targetValue ?? null,
    status_check: config.statusCheck,
    keys_file: config.keysFile ? basename(config.keysFile) : "CDK_KEYS",
    save_downloads: Boolean(config.saveDownloadsDir),
  };
}

function publicState() {
  return {
    ...state,
    config: publicConfig(),
    auth_required: Boolean(PANEL_TOKEN),
    keyPreview: state.keyPreview,
    events: state.events.slice(-120),
  };
}

function redactValue(value) {
  if (typeof value === "string") return redactText(value);
  if (Array.isArray(value)) return value.map(redactValue);
  if (value && typeof value === "object") {
    return Object.fromEntries(Object.entries(value).map(([key, item]) => [key, redactValue(item)]));
  }
  return value;
}

function publish(event) {
  const safeEvent = redactValue({
    at: new Date().toISOString(),
    ...event,
  });
  state.events.push(safeEvent);
  if (state.events.length > 200) state.events.splice(0, state.events.length - 200);
  const frame = `data: ${JSON.stringify(safeEvent)}\n\n`;
  for (const client of eventClients) {
    try {
      client.write(frame);
    } catch {
      eventClients.delete(client);
    }
  }
}

function sendJson(res, statusCode, payload) {
  const body = JSON.stringify(payload);
  res.writeHead(statusCode, {
    "content-type": "application/json; charset=utf-8",
    "cache-control": "no-store",
    "content-length": Buffer.byteLength(body),
  });
  res.end(body);
}

async function readBody(req, maxBytes = 1_024 * 1024) {
  const chunks = [];
  let size = 0;
  for await (const chunk of req) {
    size += chunk.length;
    if (size > maxBytes) throw new Error("请求体过大");
    chunks.push(chunk);
  }
  return Buffer.concat(chunks).toString("utf8");
}

async function readJsonBody(req) {
  const raw = await readBody(req);
  if (!raw.trim()) return {};
  try {
    const value = JSON.parse(raw);
    if (!value || typeof value !== "object" || Array.isArray(value)) throw new Error("必须是 JSON 对象");
    return value;
  } catch (error) {
    throw new Error("请求体不是有效 JSON: " + error.message);
  }
}

function authorized(req) {
  if (!PANEL_TOKEN) return true;
  const supplied = String(req.headers["x-panel-token"] || "");
  const left = Buffer.from(supplied);
  const right = Buffer.from(PANEL_TOKEN);
  return left.length === right.length && timingSafeEqual(left, right);
}

function requireReady() {
  if (state.configError) throw new Error(state.configError);
  if (!config || !config.adminKey) throw new Error("CODEX2API_ADMIN_KEY 未配置");
  if (keys.length === 0) throw new Error(state.keyError || "没有可用兑换码");
}

async function refreshStatus() {
  if (!config || !config.adminKey) {
    state.status = { ...state.status, error: "CODEX2API_ADMIN_KEY 未配置", fetchedAt: new Date().toISOString() };
    publish({ type: "status_error", message: state.status.error });
    return state.status;
  }
  try {
    const status = await getCodexStatus({ ...config, signal: undefined });
    state.status = { available: status.available, total: status.total, status: status.status, fetchedAt: new Date().toISOString(), error: null };
    publish({ type: "status", available: status.available, total: status.total });
  } catch (error) {
    state.status = { ...state.status, error: redactText(error.message), fetchedAt: new Date().toISOString() };
    publish({ type: "status_error", message: state.status.error });
  }
  return state.status;
}

async function reloadKeys() {
  if (!config) throw new Error(state.configError || "配置未加载");
  try {
    keys = await loadKeys(config);
    state.keyError = null;
    state.keysLoaded = keys.length;
    state.keyPreview = keys.slice(0, 12).map(maskKey);
    publish({ type: "keys_reloaded", count: keys.length });
  } catch (error) {
    state.keyError = redactText(error.message);
    state.keysLoaded = 0;
    state.keyPreview = [];
    publish({ type: "keys_error", message: state.keyError });
  }
  return { count: state.keysLoaded, error: state.keyError };
}

function numberOverride(value, name) {
  if (value === undefined || value === null || String(value).trim() === "") return undefined;
  const number = Number(value);
  if (!Number.isSafeInteger(number) || number < 0) throw new Error(name + " 必须是非负整数");
  return number;
}

async function startRun(options = {}) {
  if (state.running) throw new Error("已有任务正在运行");
  requireReady();

  const controller = new AbortController();
  const runConfig = { ...config, dryRun: false, signal: controller.signal };
  if (options.force === true) runConfig.force = true;
  const targetAvailable = numberOverride(options.targetAvailable, "targetAvailable");
  const targetTotal = numberOverride(options.targetTotal, "targetTotal");
  if (targetAvailable !== undefined && targetTotal !== undefined) throw new Error("targetAvailable 与 targetTotal 只能设置一个");
  if (targetAvailable !== undefined) {
    runConfig.targetMetric = "available";
    runConfig.targetValue = targetAvailable;
    runConfig.statusCheck = true;
  }
  if (targetTotal !== undefined) {
    runConfig.targetMetric = "total";
    runConfig.targetValue = targetTotal;
    runConfig.statusCheck = true;
  }

  state.running = true;
  state.runStartedAt = new Date().toISOString();
  state.runFinishedAt = null;
  state.lastError = null;
  state.stats = null;
  publish({ type: "run_started", key_count: keys.length, force: Boolean(runConfig.force) });

  const runKeys = keys.slice();
  const promise = run(runConfig, runKeys, { onEvent: publish })
    .then((stats) => {
      state.stats = stats;
      publish({ type: "run_complete", stats });
      return stats;
    })
    .catch((error) => {
      state.lastError = redactText(error.message);
      publish({ type: controller.signal.aborted ? "run_stopped" : "run_error", message: state.lastError });
      throw error;
    })
    .finally(() => {
      state.running = false;
      state.runFinishedAt = new Date().toISOString();
      currentRun = null;
    });

  currentRun = { controller, promise };
  // Avoid an unhandled rejection when the HTTP handler returns before the run ends.
  promise.catch(() => {});
  return { accepted: true, started_at: state.runStartedAt };
}

function htmlPage() {
  return `<!doctype html>
<html lang="zh-CN">
<head>
<meta charset="utf-8">
<meta name="viewport" content="width=device-width,initial-scale=1">
<title>Codex2API CDK 导入面板</title>
<style>
:root{color-scheme:dark;--bg:#0b1020;--panel:#131a2e;--line:#273250;--text:#edf2ff;--muted:#9aa8c7;--ok:#43d19e;--warn:#f5c451;--bad:#ff778d;--accent:#78a9ff}
*{box-sizing:border-box}body{margin:0;background:radial-gradient(circle at 20% 0,#18284e 0,#0b1020 45%);font:14px/1.5 system-ui,-apple-system,"Segoe UI",sans-serif;color:var(--text)}
main{max-width:1180px;margin:0 auto;padding:28px 18px 48px}h1{font-size:26px;margin:0 0 6px}h2{font-size:16px;margin:0 0 14px}.sub{color:var(--muted);margin-bottom:22px}.grid{display:grid;grid-template-columns:repeat(4,minmax(0,1fr));gap:12px;margin-bottom:16px}.card,.panel{background:rgba(19,26,46,.92);border:1px solid var(--line);border-radius:14px;box-shadow:0 12px 30px #0003}.card{padding:16px}.label{color:var(--muted);font-size:12px}.value{font-size:30px;font-weight:700;margin-top:4px}.panel{padding:18px;margin-top:16px}.actions{display:flex;flex-wrap:wrap;gap:9px;align-items:center}.button{border:1px solid #3b5791;border-radius:9px;background:#1b2a4b;color:var(--text);padding:9px 13px;cursor:pointer}.button:hover{background:#263d6b}.button.danger{border-color:#874254;background:#412334}.button.primary{border-color:#4b79ce;background:#2753a1}.button:disabled{opacity:.45;cursor:not-allowed}input{background:#0d1428;border:1px solid var(--line);border-radius:8px;color:var(--text);padding:9px 10px;min-width:170px}code{color:#b8d0ff}.status{margin-top:12px;color:var(--muted)}.ok{color:var(--ok)}.warn{color:var(--warn)}.bad{color:var(--bad)}.two{display:grid;grid-template-columns:1fr 1fr;gap:16px}.kv{display:grid;grid-template-columns:150px 1fr;gap:6px;color:var(--muted);word-break:break-all}.kv b{color:var(--text);font-weight:500}.keys{font-family:ui-monospace,SFMono-Regular,Consolas,monospace;color:#b8d0ff;white-space:pre-wrap}.log{background:#080d1b;border:1px solid #1c2742;border-radius:9px;padding:12px;height:280px;overflow:auto;white-space:pre-wrap;font:12px/1.55 ui-monospace,SFMono-Regular,Consolas,monospace}.notice{padding:10px 12px;border-radius:9px;background:#2b2131;color:var(--warn);margin:10px 0;display:none}.small{font-size:12px;color:var(--muted)}@media(max-width:800px){.grid{grid-template-columns:repeat(2,minmax(0,1fr))}.two{grid-template-columns:1fr}.kv{grid-template-columns:120px 1fr}}
</style>
</head>
<body><main>
<h1>Codex2API CDK 导入面板</h1><div class="sub">兑换 → 下载 Sub2API JSON → 导入账号池。面板默认只监听本机。</div>
<div id="notice" class="notice"></div>
<div class="grid">
  <div class="card"><div class="label">可用账号</div><div id="available" class="value">-</div></div>
  <div class="card"><div class="label">账号总数</div><div id="total" class="value">-</div></div>
  <div class="card"><div class="label">兑换码</div><div id="keys" class="value">-</div></div>
  <div class="card"><div class="label">任务状态</div><div id="running" class="value" style="font-size:20px">空闲</div></div>
</div>
<div class="panel"><h2>操作</h2><div class="actions">
  <button id="refresh" class="button">刷新状态</button>
  <button id="run" class="button primary">按配置执行</button>
  <button id="force" class="button">强制处理全部</button>
  <button id="stop" class="button danger" disabled>停止任务</button>
  <button id="reload" class="button">重载兑换码</button>
  <label class="small">本次目标可用数 <input id="target" type="number" min="0" placeholder="留空用环境变量"></label>
  <label id="tokenWrap" class="small" style="display:none">面板令牌 <input id="token" type="password" placeholder="X-Panel-Token"></label>
</div><div id="statusText" class="status">等待状态</div></div>
<div class="two">
  <div class="panel"><h2>配置（已脱敏）</h2><div id="config" class="kv"></div></div>
  <div class="panel"><h2>兑换码预览（已掩码）</h2><div id="keyPreview" class="keys">-</div><div class="small">真实兑换码不会发送到面板页面。</div></div>
</div>
<div class="panel"><h2>运行日志</h2><div id="log" class="log"></div></div>
</main>
<script>
const $=(id)=>document.getElementById(id);
const tokenKey='codex2api.panel.token';
if(localStorage.getItem(tokenKey)) $('token').value=localStorage.getItem(tokenKey);
function headers(){const t=$('token').value.trim();if(t)localStorage.setItem(tokenKey,t);else localStorage.removeItem(tokenKey);return t?{'X-Panel-Token':t}:{};}
async function api(path,options={}){const h={...(options.headers||{}),...headers()};if(options.body)h['content-type']='application/json';const r=await fetch(path,{...options,headers:h});let d={};try{d=await r.json();}catch{}if(!r.ok){const e=new Error(d.error||('HTTP '+r.status));e.status=r.status;e.auth_required=Boolean(d.auth_required);throw e;}return d;}
function esc(v){return String(v??'').replace(/[&<>"']/g,(c=>({'&':'&amp;','<':'&lt;','>':'&gt;','"':'&quot;',"'":'&#39;'}[c])));}
function fmtTime(v){return v?new Date(v).toLocaleString():'-';}
function render(s){
 $('available').textContent=s.status.available??'-';$('total').textContent=s.status.total??'-';$('keys').textContent=s.keysLoaded??0;
 $('running').textContent=s.running?'运行中':'空闲';$('running').className='value '+(s.running?'warn':'ok');
 $('stop').disabled=!s.running;$('run').disabled=s.running;$('force').disabled=s.running;
 const n=$('notice');const messages=[];if(s.configError)messages.push('配置错误：'+s.configError);if(s.keyError)messages.push('兑换码：'+s.keyError);if(s.status.error)messages.push('状态：'+s.status.error);if(s.lastError)messages.push('最近错误：'+s.lastError);n.textContent=messages.join(' | ');n.style.display=messages.length?'block':'none';
 $('statusText').textContent=(s.status.fetchedAt?'最近刷新：'+fmtTime(s.status.fetchedAt):'尚未读取状态')+(s.runStartedAt?'；任务开始：'+fmtTime(s.runStartedAt):'');
 const c=s.config||{};$('config').innerHTML=Object.entries(c).map(([k,v])=>'<span>'+esc(k)+'</span><b>'+esc(Array.isArray(v)?JSON.stringify(v):v??'-')+'</b>').join('');
 $('keyPreview').textContent=(s.keyPreview||[]).join('\\n')||'-';
 $('log').textContent=(s.events||[]).map(e=>'['+fmtTime(e.at)+'] '+(e.type||'event')+(e.message?' — '+e.message:'')+(e.stats?' '+JSON.stringify(e.stats):'')).join('\\n');$('log').scrollTop=$('log').scrollHeight;
 if(s.auth_required)$('tokenWrap').style.display='inline-flex';
}
async function refresh(){try{render(await api('/api/state'));}catch(e){if(e.auth_required||e.status===401)$('tokenWrap').style.display='inline-flex';$('notice').textContent=e.message;$('notice').style.display='block';}}
$('refresh').onclick=async()=>{try{await api('/api/refresh',{method:'POST'});await refresh();}catch(e){alert(e.message);}};
$('reload').onclick=async()=>{try{await api('/api/reload-keys',{method:'POST'});await refresh();}catch(e){alert(e.message);}};
$('run').onclick=async()=>{if(!$('target').value&&!confirm('未设置本次目标且环境变量也可能为空，可能会处理全部兑换码。继续？'))return;try{await api('/api/run',{method:'POST',body:JSON.stringify({force:false,targetAvailable:$('target').value||undefined})});await refresh();}catch(e){alert(e.message);}};
$('force').onclick=async()=>{if(!confirm('强制处理全部兑换码，确认继续？'))return;try{await api('/api/run',{method:'POST',body:JSON.stringify({force:true})});await refresh();}catch(e){alert(e.message);}};
$('stop').onclick=async()=>{try{await api('/api/stop',{method:'POST'});await refresh();}catch(e){alert(e.message);}};
$('token').onchange=refresh;
refresh();setInterval(refresh,2000);
</script></body></html>`;
}

async function handleApi(req, res, requestUrl) {
  if (!authorized(req)) {
    sendJson(res, 401, { error: "需要 PANEL_TOKEN（请求头 X-Panel-Token）", auth_required: true });
    return;
  }

  if (req.method === "GET" && requestUrl.pathname === "/api/state") {
    sendJson(res, 200, publicState());
    return;
  }

  if (req.method === "GET" && requestUrl.pathname === "/api/events") {
    res.writeHead(200, {
      "content-type": "text/event-stream; charset=utf-8",
      "cache-control": "no-cache, no-store",
      connection: "keep-alive",
      "x-accel-buffering": "no",
    });
    res.write(`data: ${JSON.stringify({ type: "state", state: publicState() })}\n\n`);
    eventClients.add(res);
    req.on("close", () => eventClients.delete(res));
    return;
  }

  if (req.method !== "POST") {
    sendJson(res, 405, { error: "只支持 GET/POST" });
    return;
  }

  try {
    if (requestUrl.pathname === "/api/refresh") {
      await refreshStatus();
      sendJson(res, 200, publicState());
      return;
    }
    if (requestUrl.pathname === "/api/reload-keys") {
      const result = await reloadKeys();
      sendJson(res, 200, { ...result, state: publicState() });
      return;
    }
    if (requestUrl.pathname === "/api/stop") {
      if (!currentRun) {
        sendJson(res, 200, { accepted: false, message: "当前没有运行中的任务", state: publicState() });
        return;
      }
      currentRun.controller.abort();
      publish({ type: "stop_requested" });
      sendJson(res, 202, { accepted: true, state: publicState() });
      return;
    }
    if (requestUrl.pathname === "/api/run") {
      const body = await readJsonBody(req);
      const result = await startRun({
        force: body.force === true,
        targetAvailable: body.targetAvailable,
        targetTotal: body.targetTotal,
      });
      sendJson(res, 202, { ...result, state: publicState() });
      return;
    }
    sendJson(res, 404, { error: "接口不存在" });
  } catch (error) {
    sendJson(res, 400, { error: redactText(error.message), state: publicState() });
  }
}

const server = createServer(async (req, res) => {
  try {
    const requestUrl = new URL(req.url || "/", `http://${req.headers.host || "localhost"}`);
    if (req.method === "GET" && requestUrl.pathname === "/healthz") {
      sendJson(res, 200, { ok: true, running: state.running });
      return;
    }
    if (req.method === "GET" && requestUrl.pathname === "/") {
      const body = htmlPage();
      res.writeHead(200, {
        "content-type": "text/html; charset=utf-8",
        "cache-control": "no-store",
        "content-length": Buffer.byteLength(body),
      });
      res.end(body);
      return;
    }
    if (requestUrl.pathname.startsWith("/api/")) {
      await handleApi(req, res, requestUrl);
      return;
    }
    sendJson(res, 404, { error: "not found" });
  } catch (error) {
    if (!res.headersSent) sendJson(res, 500, { error: redactText(error.message) });
    else res.end();
  }
});

async function initialize() {
  try {
    config = buildConfig({ dryRun: false, force: false, noStatusCheck: false });
  } catch (error) {
    state.configError = redactText(error.message);
    // Build a non-operational config so the panel can still display instructions.
    try {
      config = buildConfig({ dryRun: true, force: false, noStatusCheck: true });
    } catch {
      config = null;
    }
  }
  if (config) await reloadKeys();
}

await initialize();
server.on("error", (error) => {
  console.error("Codex2API 面板启动失败：" + error.message);
  process.exitCode = 1;
});
server.listen(PANEL_PORT, PANEL_HOST, () => {
  const displayHost = PANEL_HOST === "::1" ? "[::1]" : PANEL_HOST;
  console.log("Codex2API 面板已启动：http://" + displayHost + ":" + PANEL_PORT);
  if (PANEL_TOKEN) console.log("面板已启用 PANEL_TOKEN 鉴权");
  if (config?.adminKey) void refreshStatus();
});

function shutdown() {
  if (currentRun) currentRun.controller.abort();
  for (const client of eventClients) {
    try { client.end(); } catch { /* ignore */ }
  }
  eventClients.clear();
  server.close(() => process.exit(0));
}
process.on("SIGINT", shutdown);
process.on("SIGTERM", shutdown);
