import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'

const settings = readFileSync(new URL('../pages/Settings.tsx', import.meta.url), 'utf8')
const zh = JSON.parse(readFileSync(new URL('../locales/zh.json', import.meta.url), 'utf8'))
const en = JSON.parse(readFileSync(new URL('../locales/en.json', import.meta.url), 'utf8'))

const TABS = ['codex', 'claude', 'antigravity', 'grok', 'appearance', 'general']

test('settings page is split into one panel per tab driven by ?tab=', () => {
  assert.match(settings, /useSearchParams\(\)/)
  assert.match(settings, /searchParams\.get\('tab'\)/)
  assert.match(settings, /type SettingsTabKey = 'codex' \| 'claude' \| 'antigravity' \| 'grok' \| 'appearance' \| 'general'/)
  for (const tab of TABS) {
    assert.match(settings, new RegExp(`\\{ id: '${tab}', label: t\\('settings\\.nav\\.${tab}'\\)`), `tab pill ${tab}`)
    assert.match(settings, new RegExp(`\\{activeTab === '${tab}' \\? \\(`), `panel ${tab}`)
  }
  // 旧滚动定位导航已移除，避免回退成单页长滚动。
  assert.doesNotMatch(settings, /scrollToSection/)
  assert.doesNotMatch(settings, /settingsSections/)
  assert.match(settings, /role="tablist"/)
  assert.match(settings, /role="tab"/)
})

test('legacy #settings-* anchors map onto a tab', () => {
  for (const id of ['settings-overview', 'settings-traffic', 'settings-runtime', 'settings-models', 'settings-grok', 'settings-claude', 'settings-antigravity', 'settings-appearance']) {
    assert.match(settings, new RegExp(`'${id}': '(${TABS.join('|')})'`), id)
  }
})

test('channel-specific cards live in their channel tab, shared cards in general', () => {
  const panel = (tab) => {
    const start = settings.indexOf(`{activeTab === '${tab}' ? (`)
    const next = TABS.map((t) => settings.indexOf(`{activeTab === '${t}' ? (`)).filter((i) => i > start)
    const end = next.length ? Math.min(...next) : settings.length
    assert.ok(start > 0, `panel ${tab}`)
    return settings.slice(start, end)
  }
  const codex = panel('codex')
  for (const key of ['settings.probeScheduling', 'settings.globalAutoPauseTitle', 'settings.codexWebsocket', 'settings.codexOverloadPause', 'settings.responseCache.title', 'settings.codexClientTitle', 'settings2.codexModelMapping']) {
    assert.ok(codex.includes(key), `codex tab should contain ${key}`)
  }
  assert.ok(codex.includes('codex_user_agent_config') || codex.includes('codexUserAgentConfig'))
  assert.ok(!codex.includes('settings.usageLogMode'), 'usage log settings are shared, not Codex-only')
  assert.ok(panel('claude').includes('<ClaudeCodeSettingsCard />'))
  assert.ok(panel('antigravity').includes('settings.antigravityOAuth.title'))
  assert.ok(panel('grok').includes('settings.grokSettingsTitle'))
  const appearance = panel('appearance')
  assert.ok(appearance.includes('settings.display') && appearance.includes('settings.backgroundImage'))
  const general = panel('general')
  for (const key of ['settings.systemStatus', 'settings.trafficProtection', 'settings.modelCooldownTitle', 'settings.schedulingStrategy', 'settings.runtimeOptimization', 'settings.usageLogMode', 'settings.githubAccess', 'settings.imageStorage', 'settings.security', 'settings.apiEndpoints']) {
    assert.ok(general.includes(key), `general tab should contain ${key}`)
  }
  assert.ok(!general.includes('settings.codexUserAgentRaw'), 'Codex UA emulation must not stay in general runtime card')
})

test('tab and section labels exist in zh and en', () => {
  for (const locale of [zh, en]) {
    for (const key of ['codex', 'codexDesc', 'claude', 'antigravity', 'grok', 'appearance', 'general', 'generalDesc', 'codexQuota', 'codexQuotaDesc', 'codexTransport', 'codexTransportDesc', 'codexClient', 'codexClientDesc']) {
      assert.equal(typeof locale.settings?.nav?.[key], 'string', `settings.nav.${key}`)
    }
    assert.equal(typeof locale.settings?.codexClientTitle, 'string')
    assert.equal(typeof locale.settings?.codexClientDesc, 'string')
  }
})

test('codex tab points at the shared scheduling strategy in general', () => {
  const start = settings.indexOf("{activeTab === 'codex' ? (")
  const end = settings.indexOf("{activeTab === 'claude' ? (")
  const codex = settings.slice(start, end)
  assert.match(codex, /selectTab\('general', 'settings-traffic'\)/)
  assert.match(codex, /settings\.codexSchedulingHintAction/)
  assert.match(settings, /pendingSectionRef/)
  for (const locale of [zh, en]) {
    for (const key of ['codexSchedulingHintTitle', 'codexSchedulingHintDesc', 'codexSchedulingHintAction']) {
      assert.equal(typeof locale.settings?.[key], 'string', `settings.${key}`)
    }
    assert.match(locale.settings.schedulerModeDesc, /Grok/)
    assert.match(locale.settings.schedulerModeDesc, /Antigravity/)
  }
})

test('shared settings cards declare which upstream channels they apply to', () => {
  const badges = readFileSync(new URL('../components/ChannelScopeBadges.tsx', import.meta.url), 'utf8')
  assert.match(badges, /export const ALL_UPSTREAM_CHANNELS/)
  assert.match(badges, /data-channel-scope/)
  assert.match(settings, /channels\?: readonly UpstreamChannel\[\]/)
  // 通用 Tab 里每张跨渠道卡片都必须带 channels，避免再出现"看不出给谁用"的设置。
  for (const title of ['settings.trafficProtection', 'settings.schedulingStrategy', 'settings.runtimeOptimization', 'settings.autoCleanup']) {
    assert.match(settings, new RegExp(`title=\\{t\\('${title.replace('.', '\\.')}'\\)\\}[^\\n]*channels=\\{ALL_UPSTREAM_CHANNELS\\}`), title)
  }
  assert.match(settings, /title=\{t\('settings\.continuousRetryTitle'\)\}\n\s+channels=\{ALL_UPSTREAM_CHANNELS\}/)
  assert.match(settings, /title=\{t\('settings\.modelCooldownTitle'\)\}\n\s+channels=\{ALL_UPSTREAM_CHANNELS\}/)
  assert.match(settings, /settings\.imageStorage'\)\}[^\n]*channels=\{CHANNELS_CODEX_ONLY\}/)
  assert.match(settings, /settings\.globalAutoPauseTitle'\)\}[^\n]*channels=\{CHANNELS_CODEX_CLAUDE\}/)
  assert.match(settings, /settings\.billingTierPolicy'\)\}[^\n]*channels=\{CHANNELS_CODEX_ONLY\}/)
  assert.match(settings, /settings\.streamFlushPolicy'\)\}[^\n]*channels=\{CHANNELS_STREAMING\}/)
  assert.match(settings, /value: 'response_failed', channels: CHANNELS_CODEX_ONLY/)
  assert.match(settings, /<ChannelScopeBadges channels=\{CHANNELS_RELAY\} size="xs" \/>/)
  for (const locale of [zh, en]) {
    assert.match(locale.settings.channelScope, /\{\{channels\}\}/)
    assert.equal(typeof locale.settings.channelScopeAll, 'string')
  }
})
