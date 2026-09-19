import test from 'node:test'
import assert from 'node:assert/strict'
import { readFileSync } from 'node:fs'
import { turnStateRemainingMs, turnStateDisplayStatus, parseTurnStateModels, parseTurnStateTargetLengths, validTurnStateModels, validTurnStateTargetLengths } from './codexTurnStateStatus.ts'

const now = Date.parse('2026-09-19T10:00:00Z')
const item = { model: 'gpt-5.5', status: 'ready', ready: true, expires_at: '2026-09-19T10:05:00Z', captured_at: '2026-09-19T10:00:00Z' }

test('countdown uses server expiry, never capture or save time', () => {
  assert.equal(turnStateRemainingMs(item, now), 300000)
  assert.equal(turnStateRemainingMs({ ...item, captured_at: '2026-09-19T11:00:00Z' }, now), 300000)
  assert.equal(turnStateRemainingMs(item, now + 300000), 0)
  assert.equal(turnStateRemainingMs({ expires_at: 'invalid' }, now), 0)
  assert.equal(turnStateRemainingMs({}, now), 0)
})

test('a ready row becomes expired locally and collection failures remain visible', () => {
  assert.equal(turnStateDisplayStatus(item, now), 'ready')
  assert.equal(turnStateDisplayStatus(item, now + 300000), 'expired')
  assert.equal(turnStateDisplayStatus({ ...item, status: 'refreshing' }, now + 300000), 'refreshing')
  assert.equal(turnStateDisplayStatus({ ...item, status: 'paused', pause_reason: 'rate_limited' }, now + 300000), 'paused')
  assert.equal(turnStateDisplayStatus({ ...item, status: 'error' }, now), 'error')
  assert.equal(turnStateDisplayStatus({ ...item, status: 'disabled' }, now), 'disabled')
})

test('model input preserves distinct exact model names and deduplicates case', () => {
  assert.deepEqual(parseTurnStateModels(' GPT-5.5, gpt-5.4\ngpt-5.5  '), ['gpt-5.5', 'gpt-5.4'])
  assert.deepEqual(parseTurnStateModels(' , '), [])
})

test('models remain required while disabled and reject wildcard or oversized lists', () => {
  assert.equal(validTurnStateModels([]), false)
  assert.equal(validTurnStateModels(['gpt-5.5']), true)
  assert.equal(validTurnStateModels(['gpt-*']), false)
  assert.equal(validTurnStateModels(['模型']), false)
  assert.equal(validTurnStateModels(['x'.repeat(129)]), false)
  assert.equal(validTurnStateModels(Array.from({ length: 33 }, (_, i) => 'gpt-' + i)), false)
})

test('target lengths are parsed, normalized, and validated as a small integer set', () => {
  assert.deepEqual(parseTurnStateTargetLengths(' 292, 332\n292 '), [292, 332])
  assert.deepEqual(parseTurnStateTargetLengths(' , '), [])
  assert.equal(validTurnStateTargetLengths([292, 332]), true)
  assert.equal(validTurnStateTargetLengths([]), false)
  assert.equal(validTurnStateTargetLengths([99]), false)
  assert.equal(validTurnStateTargetLengths([4097]), false)
  assert.equal(validTurnStateTargetLengths([292.5]), false)
  assert.equal(validTurnStateTargetLengths(Array.from({ length: 9 }, (_, i) => 292 + i)), false)
})

test('three locales cover all turn-state statuses and configuration fields', () => {
  const locales = ['en', 'zh', 'zh-TW'].map(locale => JSON.parse(readFileSync(new URL('../locales/' + locale + '.json', import.meta.url), 'utf8')).turnState)
  const flatten = (value, prefix = '') => Object.entries(value).flatMap(([key, val]) => typeof val === 'string' ? [prefix + key] : flatten(val, prefix + key + '.')).sort()
  assert.deepEqual(flatten(locales[0]), flatten(locales[1]))
  assert.deepEqual(flatten(locales[0]), flatten(locales[2]))
  for (const locale of locales) {
    assert.equal(typeof locale.schedulingEnabled, 'string')
    assert.equal(typeof locale.schedulingHint, 'string')
    for (const status of ['missing', 'ready', 'refreshing', 'paused', 'expired', 'error', 'disabled']) assert.equal(typeof locale.status[status], 'string')
    for (const reason of ['rate_limited', 'credits_unavailable']) assert.equal(typeof locale.pauseReason[reason], 'string')
  }
})

test('settings expose an independent scheduling flag gated by the overall feature switch', () => {
  const types = readFileSync(new URL('../types.ts', import.meta.url), 'utf8')
  const settings = readFileSync(new URL('../components/CodexTurnStateSettings.tsx', import.meta.url), 'utf8')
  assert.match(types, /interface CodexTurnStateSettings[\s\S]*scheduling_enabled: boolean/)
  assert.match(settings, /checked=\{config\.scheduling_enabled\}/)
  assert.match(settings, /disabled=\{busy \|\| !config\.enabled\}/)
  assert.match(settings, /change\(\{ scheduling_enabled \}\)/)
})
