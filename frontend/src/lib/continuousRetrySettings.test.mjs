import assert from 'node:assert/strict'
import test from 'node:test'

import {
  buildContinuousRetryCatchAllPatch,
  buildContinuousRetryEnabledPatch,
  parseContinuousRetryErrorCodes,
  parseContinuousRetryStatusCodes,
} from './continuousRetrySettings.ts'

test('continuous retry catch-all is one click and cannot remain active behind the master switch', () => {
  assert.deepEqual(buildContinuousRetryCatchAllPatch(true), {
    continuous_retry_enabled: true,
    continuous_retry_catch_all: true,
  })
  assert.deepEqual(buildContinuousRetryCatchAllPatch(false), {
    continuous_retry_catch_all: false,
  })
  assert.deepEqual(buildContinuousRetryEnabledPatch(false), {
    continuous_retry_enabled: false,
    continuous_retry_catch_all: false,
  })
})

test('continuous retry status-code drafts preserve multiple valid selections', () => {
  assert.deepEqual(
    parseContinuousRetryStatusCodes('503, 403,404,503,099,600,404x'),
    [403, 404, 503],
  )
})

test('continuous retry error-code drafts normalize exact machine tokens', () => {
  assert.deepEqual(
    parseContinuousRetryErrorCodes(' Rate_Limited,server.error,rate_limited,bad code!,context-error '),
    ['context-error', 'rate_limited', 'server.error'],
  )
})
