export type ErrorStatusRow = {
  code: string
  count: number
  percent: number
}

export function buildErrorStatusBreakdown(
  counts: Record<string, number> | undefined,
  errorTotal?: number,
): ErrorStatusRow[] {
  const rows = Object.entries(counts ?? {})
    .map(([code, count]) => ({
      code,
      count: Number(count) || 0,
    }))
    .filter((row) => row.count > 0)
    .sort((a, b) => b.count - a.count || Number(a.code) - Number(b.code))
  const total =
    errorTotal && errorTotal > 0
      ? errorTotal
      : rows.reduce((sum, row) => sum + row.count, 0)
  return rows.map((row) => ({
    ...row,
    percent: total > 0 ? (row.count / total) * 100 : 0,
  }))
}

export function formatErrorStatusPercent(percent: number): string {
  if (!Number.isFinite(percent) || percent <= 0) {
    return "0%"
  }
  if (percent >= 99.95 || percent < 0.05) {
    return `${percent.toFixed(0)}%`
  }
  return `${percent.toFixed(1)}%`
}
