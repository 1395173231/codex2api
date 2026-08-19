const CONTINUOUS_RETRY_ERROR_CODE_PATTERN = /^[a-z0-9_.-]+$/

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
