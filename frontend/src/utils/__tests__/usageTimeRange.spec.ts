import { describe, expect, it } from 'vitest'

import { buildUsageRangeQuery, createLast24HoursRange } from '../usageTimeRange'

describe('usageTimeRange', () => {
  it('builds an exact rolling 24-hour window', () => {
    const now = new Date(2026, 7, 15, 15, 30, 45, 123)
    const range = createLast24HoursRange(now)

    expect(range.start).toBe('2026-08-14')
    expect(range.end).toBe('2026-08-15')
    expect(Date.parse(range.end_time) - Date.parse(range.start_time)).toBe(24 * 60 * 60 * 1000)
  })

  it('uses exact timestamps only for a rolling range', () => {
    expect(buildUsageRangeQuery('2026-08-14', '2026-08-15', {
      start_time: '2026-08-14T07:30:00.000Z',
      end_time: '2026-08-15T07:30:00.000Z',
    })).toEqual({
      start_date: undefined,
      end_date: undefined,
      start_time: '2026-08-14T07:30:00.000Z',
      end_time: '2026-08-15T07:30:00.000Z',
    })

    expect(buildUsageRangeQuery('2026-08-01', '2026-08-15', null)).toEqual({
      start_date: '2026-08-01',
      end_date: '2026-08-15',
      start_time: undefined,
      end_time: undefined,
    })
  })
})
