const CONTINUOUS_RETRY_ERROR_CODE_PATTERN = /^[a-z0-9_.-]+$/

export function buildContinuousRetryEnabledPatch(enabled: boolean) {
  return enabled
    ? { continuous_retry_enabled: true }
    : { continuous_retry_enabled: false, continuous_retry_catch_all: false }
}

export function buildContinuousRetryCatchAllPatch(enabled: boolean) {
  return enabled
    ? { continuous_retry_enabled: true, continuous_retry_catch_all: true }
    : { continuous_retry_catch_all: false }
}

export function parseContinuousRetryStatusCodes(raw: string): number[] {
  const values = raw
    .split(',')
    .map((value) => value.trim())
    .filter((value) => /^\d{3}$/.test(value))
    .map((value) => Number(value))
    .filter((value) => value >= 100 && value <= 599)

  return Array.from(new Set(values)).sort((a, b) => a - b)
}

export function parseContinuousRetryErrorCodes(raw: string): string[] {
  const values = raw
    .split(',')
    .map((value) => value.trim().toLowerCase())
    .filter((value) => value.length > 0 && value.length <= 128)
    .filter((value) => CONTINUOUS_RETRY_ERROR_CODE_PATTERN.test(value))

  return Array.from(new Set(values)).sort()
}
