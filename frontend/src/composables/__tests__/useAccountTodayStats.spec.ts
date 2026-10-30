import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { effectScope, ref } from 'vue'
import { useAccountTodayStats } from '../useAccountTodayStats'
import { accountUsageShowsTodayStats } from '@/utils/accountTodayStats'
import type { Account } from '@/types'

const { request } = vi.hoisted(() => ({ request: vi.fn() }))
vi.mock('@/api/admin', () => ({ adminAPI: { accounts: { getBatchTodayStatsWithEtag: request } } }))
const metric = { requests: 9, tokens: 100, cost: 0.4, standard_cost: 0.5, user_cost: 0.6 }
const scopes: ReturnType<typeof effectScope>[] = []
function setup(initial = [2, 1]) {
  const ids = ref(initial)
  const context = ref({ page: 1, search: '' })
  const scope = effectScope()
  scopes.push(scope)
  const state = scope.run(() => useAccountTodayStats({ accountIDs: () => ids.value, context: () => context.value }))!
  return { ...state, ids, context, scope }
}

beforeEach(() => {
  vi.useFakeTimers()
  vi.setSystemTime(new Date('2026-10-30T12:00:00Z'))
  request.mockReset().mockResolvedValue({ notModified: false, etag: 'day-1', data: { stats: { '1': metric } } })
})
afterEach(() => {
  scopes.splice(0).forEach(scope => scope.stop())
  vi.useRealTimers()
  vi.restoreAllMocks()
})

describe('account today stats snapshot', () => {
  it('does not request or leave a loading state without visible consumers', async () => {
    const state = setup([])
    await state.refresh()
    expect(request).not.toHaveBeenCalled()
    expect(state.loading.value).toBe(false)
    expect(state.stats.value).toEqual({})
  })

  it('normalizes IDs, fills missing rows, and preserves the object on repeated 304 refreshes', async () => {
    const state = setup([2, 1, 2])
    await state.refresh()
    expect(request).toHaveBeenCalledWith([1, 2], expect.objectContaining({ etag: null }))
    expect(state.stats.value['2']).toEqual({ requests: 0, tokens: 0, cost: 0, standard_cost: 0, user_cost: 0 })
    const snapshot = state.stats.value
    request.mockResolvedValue({ notModified: true, etag: 'day-1', data: null })
    await state.refresh()
    await state.refresh()
    expect(request).toHaveBeenLastCalledWith([1, 2], expect.objectContaining({ etag: 'day-1' }))
    expect(state.stats.value).toBe(snapshot)
    expect(state.stats.value['1']).toEqual(metric)
    expect(request).toHaveBeenCalledTimes(3) // Manual refresh always revalidates too.
  })

  it('replaces a changed server snapshot and clears a missing ETag', async () => {
    const state = setup()
    await state.refresh()
    request.mockResolvedValue({ notModified: false, etag: null, data: { stats: { '1': { ...metric, requests: 10 } } } })
    await state.refresh()
    expect(state.stats.value['1'].requests).toBe(10)
    await state.refresh()
    expect(request).toHaveBeenLastCalledWith([1, 2], expect.objectContaining({ etag: null }))
  })

  it.each(['page', 'search'] as const)('isolates the validator when query %s changes with identical IDs', async (field) => {
    const state = setup()
    await state.refresh()
    if (field === 'page') state.context.value.page = 2
    else state.context.value.search = 'new filter'
    expect(state.stats.value).toEqual({})
    await state.refresh()
    expect(request).toHaveBeenLastCalledWith([1, 2], expect.objectContaining({ etag: null }))
  })

  it('discards the prior day validator and stats at midnight', async () => {
    const state = setup()
    await state.refresh()
    vi.setSystemTime(new Date('2026-10-31T00:00:01Z'))
    request.mockResolvedValue({ notModified: false, etag: 'day-2', data: { stats: {} } })
    await state.refresh()
    expect(request).toHaveBeenLastCalledWith([1, 2], expect.objectContaining({ etag: null }))
    expect(state.stats.value['1'].requests).toBe(0)
  })

  it('ignores old-page completions while the next page is still loading', async () => {
    let complete!: (data: unknown) => void
    request.mockImplementationOnce(() => new Promise(resolve => { complete = resolve }))
    const state = setup()
    const pending = state.refresh()
    const signal = request.mock.calls[0][1].signal as AbortSignal
    state.context.value.page = 2
    expect(signal.aborted).toBe(true)
    complete({ notModified: false, etag: 'stale', data: { stats: { '1': metric } } })
    await pending
    expect(state.stats.value).toEqual({})
    state.ids.value = [3]
    await state.refresh()
    expect(request).toHaveBeenLastCalledWith([3], expect.objectContaining({ etag: null }))
  })

  it('invalidates in-flight requests when columns remove all consumers and on disposal', async () => {
    let complete!: (data: unknown) => void
    request.mockImplementation(() => new Promise(resolve => { complete = resolve }))
    const state = setup()
    const pending = state.refresh()
    state.ids.value = []
    await state.refresh()
    complete({ notModified: false, etag: 'stale', data: { stats: { '1': metric } } })
    await pending
    expect(state.stats.value).toEqual({})
    state.ids.value = [1]
    const next = state.refresh()
    const signal = request.mock.lastCall![1].signal as AbortSignal
    state.scope.stop()
    expect(signal.aborted).toBe(true)
    complete({ notModified: true, etag: 'stale', data: null })
    await next
    expect(state.stats.value).toEqual({})
  })

  it('retains same-context data on failure and recovers on revalidation', async () => {
    vi.spyOn(console, 'error').mockImplementation(() => {})
    const state = setup()
    await state.refresh()
    const snapshot = state.stats.value
    request.mockRejectedValueOnce(new Error('offline'))
    await state.refresh()
    expect(state.error.value).toBe('Failed')
    expect(state.loading.value).toBe(false)
    expect(state.stats.value).toBe(snapshot)
    request.mockResolvedValue({ notModified: true, etag: 'day-1', data: null })
    await state.refresh()
    expect(state.error.value).toBeNull()
    expect(state.stats.value).toBe(snapshot)
  })
})

describe('usage cell today-stat consumers', () => {
  it.each([
    ['anthropic', 'oauth', false], ['anthropic', 'setup-token', false],
    ['openai', 'oauth', false], ['openai', 'apikey', false],
    ['grok', 'oauth', false], ['kimi', 'apikey', false],
    ['zhipu', 'apikey', false], ['deepseek', 'apikey', false],
    ['anthropic', 'apikey', true], ['anthropic', 'bedrock', true],
    ['gemini', 'apikey', true], ['grok', 'apikey', true]
  ])('%s %s consumes today stats: %s', (platform, type, expected) => {
    expect(accountUsageShowsTodayStats({ platform, type } as Pick<Account, 'platform' | 'type'>)).toBe(expected)
  })
})
