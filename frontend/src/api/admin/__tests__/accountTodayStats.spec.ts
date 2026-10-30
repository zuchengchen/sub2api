import { beforeEach, describe, expect, it, vi } from 'vitest'
import { getBatchTodayStats, getBatchTodayStatsWithEtag } from '../accounts'
const { post } = vi.hoisted(() => ({ post: vi.fn() }))
vi.mock('../../client', () => ({ apiClient: { post } }))
beforeEach(() => post.mockReset())

describe('batch today stats HTTP validators', () => {
  it('keeps the original data-only API working', async () => {
    post.mockResolvedValue({ data: { stats: {} } })
    expect(await getBatchTodayStats([1])).toEqual({ stats: {} })
  })
  it('sends the validator and signal, accepts 304, and retains the response ETag', async () => {
    const signal = new AbortController().signal
    post.mockResolvedValue({ status: 304, headers: { etag: 'snapshot' }, data: '' })
    expect(await getBatchTodayStatsWithEtag([1, 2], { etag: 'snapshot', signal })).toEqual({
      notModified: true, etag: 'snapshot', data: null
    })
    const [, body, config] = post.mock.calls[0]
    expect(body).toEqual({ account_ids: [1, 2] })
    expect(config.headers).toEqual({ 'If-None-Match': 'snapshot' })
    expect(config.signal).toBe(signal)
    expect(config.validateStatus(304)).toBe(true)
    expect(config.validateStatus(200)).toBe(true)
    expect(config.validateStatus(500)).toBe(false)
  })
  it('returns fresh data without adding a validator when none is available', async () => {
    post.mockResolvedValue({ status: 200, headers: {}, data: { stats: {} } })
    expect(await getBatchTodayStatsWithEtag([2])).toEqual({ notModified: false, etag: null, data: { stats: {} } })
    expect(post.mock.calls[0][2].headers).toEqual({})
  })
})
