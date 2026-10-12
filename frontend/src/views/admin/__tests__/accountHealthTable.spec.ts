import { describe, expect, it } from 'vitest'
import type { AccountHealthSnapshot } from '@/types'
import { filterHealthItems, nextHealthSort, sortHealthItems, visibleHealthItems } from '../accountHealthTable'

function row(partial: Partial<AccountHealthSnapshot> & { account_id: number }): AccountHealthSnapshot {
  return {
    name: `a${partial.account_id}`,
    platform: 'openai',
    score: 80,
    err_rate: 0.1,
    total: 10,
    errors: 1,
    state: 'healthy',
    isolated: false,
    evaluated_at: '2026-10-12T00:00:00Z',
    ...partial
  }
}

describe('account health table helpers', () => {
  it('filters by platform and keeps all rows for the empty selection', () => {
    const items = [row({ account_id: 1, platform: 'openai' }), row({ account_id: 2, platform: 'anthropic' })]
    expect(filterHealthItems(items, '').map(item => item.account_id)).toEqual([1, 2])
    expect(filterHealthItems(items, 'anthropic').map(item => item.account_id)).toEqual([2])
  })

  it('sorts score ascending so the worst accounts are first', () => {
    const items = [row({ account_id: 1, score: 90 }), row({ account_id: 2, score: 40 }), row({ account_id: 3, score: 70 })]
    expect(sortHealthItems(items, 'score', 'asc').map(item => item.account_id)).toEqual([2, 3, 1])
  })

  it('toggles the same column and starts error-rate at descending', () => {
    expect(nextHealthSort('score', 'asc', 'score')).toEqual({ key: 'score', dir: 'desc' })
    expect(nextHealthSort('score', 'asc', 'err_rate')).toEqual({ key: 'err_rate', dir: 'desc' })
  })

  it('filters then sorts', () => {
    const items = [
      row({ account_id: 1, platform: 'openai', score: 90 }),
      row({ account_id: 2, platform: 'openai', score: 20 }),
      row({ account_id: 3, platform: 'grok', score: 10 })
    ]
    expect(visibleHealthItems(items, 'openai', 'score', 'asc').map(item => item.account_id)).toEqual([2, 1])
  })
})
