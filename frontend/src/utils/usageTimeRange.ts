const DAY_MS = 24 * 60 * 60 * 1000

export const LAST_24_HOURS_PRESET = 'last24Hours'

export interface ExactUsageTimeRange {
  start_time: string
  end_time: string
}

export interface UsageRangeQuery extends Partial<ExactUsageTimeRange> {
  start_date?: string
  end_date?: string
}

export interface Last24HoursRange extends ExactUsageTimeRange {
  start: string
  end: string
}

function formatLocalDate(date: Date): string {
  const year = date.getFullYear()
  const month = String(date.getMonth() + 1).padStart(2, '0')
  const day = String(date.getDate()).padStart(2, '0')
  return `${year}-${month}-${day}`
}

export function createLast24HoursRange(now = new Date()): Last24HoursRange {
  const start = new Date(now.getTime() - DAY_MS)
  return {
    start: formatLocalDate(start),
    end: formatLocalDate(now),
    start_time: start.toISOString(),
    end_time: now.toISOString(),
  }
}

export function buildUsageRangeQuery(
  startDate: string,
  endDate: string,
  exactRange: ExactUsageTimeRange | null,
): UsageRangeQuery {
  if (exactRange) {
    return {
      start_date: undefined,
      end_date: undefined,
      ...exactRange,
    }
  }
  return {
    start_date: startDate,
    end_date: endDate,
    start_time: undefined,
    end_time: undefined,
  }
}
