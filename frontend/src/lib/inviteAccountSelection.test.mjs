import assert from 'node:assert/strict'
import test from 'node:test'

import {
  inviteRecipientCandidates,
  isCodexInviteSenderCandidate,
  isInviteAccountSelectable,
} from './inviteAccountSelection.ts'

function account(overrides = {}) {
  return {
    id: 1,
    name: 'account',
    email: 'account@example.com',
    plan_type: 'pro',
    status: 'active',
    proxy_url: '',
    created_at: '',
    updated_at: '',
    enabled: true,
    ...overrides,
  }
}

test('invite selectors exclude disabled, error, and banned accounts', () => {
  assert.equal(isInviteAccountSelectable(account({ enabled: false })), false)
  assert.equal(isInviteAccountSelectable(account({ status: 'error' })), false)
  assert.equal(isInviteAccountSelectable(account({ status: ' UNAUTHORIZED ' })), false)
  assert.equal(isInviteAccountSelectable(account({ status: 'banned' })), false)
  assert.equal(isInviteAccountSelectable(account({ health_tier: ' BANNED ' })), false)
})

test('invite selectors keep locked and temporarily limited accounts', () => {
  assert.equal(isInviteAccountSelectable(account({ locked: true })), true)
  assert.equal(isInviteAccountSelectable(account({ status: 'cooldown' })), true)
  assert.equal(isInviteAccountSelectable(account({ status: 'rate_limited' })), true)
  assert.equal(isInviteAccountSelectable(account({ status: 'quota_paused' })), true)
})

test('sender candidates also require a referral-capable Codex OAuth account', () => {
  assert.equal(isCodexInviteSenderCandidate(account()), true)
  assert.equal(isCodexInviteSenderCandidate(account({ openai_responses_api: true })), false)
  assert.equal(isCodexInviteSenderCandidate(account({ at_only: true })), false)
  assert.equal(isCodexInviteSenderCandidate(account({ status: 'error' })), false)
})

test('recipient candidates apply account filtering before email deduplication', () => {
  const rows = [
    account({ id: 1, email: 'sender@example.com' }),
    account({ id: 2, email: 'duplicate@example.com', enabled: false }),
    account({ id: 3, email: 'DUPLICATE@example.com' }),
    account({ id: 4, email: 'error@example.com', status: 'error' }),
    account({ id: 5, email: 'banned@example.com', status: 'unauthorized' }),
    account({ id: 6, email: 'locked@example.com', locked: true }),
    account({ id: 7, email: '  ' }),
  ]

  assert.deepEqual(
    inviteRecipientCandidates(rows, ' SENDER@example.com ').map((row) => row.id),
    [3, 6],
  )
})
