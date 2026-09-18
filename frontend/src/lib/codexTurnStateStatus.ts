import type { CodexTurnStateStatus } from '../types.ts'

export function turnStateRemainingMs(item: Pick<CodexTurnStateStatus, 'expires_at'>, now = Date.now()): number {
  const expires = Date.parse(item.expires_at ?? '')
  return Number.isFinite(expires) ? Math.max(0, expires - now) : 0
}

export function turnStateDisplayStatus(item: CodexTurnStateStatus, now = Date.now()): CodexTurnStateStatus['status'] {
  if (item.status === 'ready' && turnStateRemainingMs(item, now) === 0) return 'expired'
  return item.status
}

export function validTurnStateModels(models: string[]): boolean {
  return models.length >= 1 && models.length <= 32 && models.every(model => /^[\x21-\x7e]{1,128}$/.test(model) && !model.includes('*'))
}

export function parseTurnStateModels(value: string): string[] {
  return [...new Set(value.split(/[\s,]+/).map(model => model.trim().toLowerCase()).filter(Boolean))]
}
