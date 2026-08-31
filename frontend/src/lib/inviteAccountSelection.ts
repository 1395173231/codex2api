import type { AccountRow } from '../types'

const BLOCKED_INVITE_STATUSES = new Set(['unauthorized', 'error', 'banned'])

// 邀请页的两个账号选择器共享这条可见性规则。封禁/错误账号以及被禁用的账号
// 不应再进入候选列表；锁定、限流和其他临时调度状态不影响账号作为受邀方，
// 也不代表其 referral 凭证已经失效，因此继续保留。
export function isInviteAccountSelectable(account: AccountRow): boolean {
  if (account.enabled === false) return false

  const status = (account.status || '').trim().toLowerCase()
  if (BLOCKED_INVITE_STATUSES.has(status)) return false

  return (account.health_tier || '').trim().toLowerCase() !== 'banned'
}

export function isCodexInviteSenderCandidate(account: AccountRow): boolean {
  if (!isInviteAccountSelectable(account)) return false

  // 中转号与 AT-only 账号没有可持续用于 referral 的 Codex OAuth 凭证。
  return !account.openai_responses_api && !account.at_only
}

export function inviteRecipientCandidates(
  rows: AccountRow[],
  excludeEmail?: string,
): AccountRow[] {
  const excluded = (excludeEmail ?? '').trim().toLowerCase()
  const seen = new Set<string>()
  const candidates: AccountRow[] = []

  for (const row of rows) {
    if (!isInviteAccountSelectable(row)) continue

    const email = row.email?.trim()
    if (!email) continue

    const key = email.toLowerCase()
    if (key === excluded || seen.has(key)) continue

    seen.add(key)
    candidates.push(row)
  }

  return candidates
}
