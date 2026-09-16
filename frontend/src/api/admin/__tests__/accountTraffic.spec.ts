import { beforeEach, describe, expect, it, vi } from 'vitest'
import { accountTrafficAPI, defaultTrafficPolicy } from '../accountTraffic'

const get = vi.fn()
const put = vi.fn()
vi.mock('../../client', () => ({
  apiClient: {
    get: (...args: unknown[]) => get(...args),
    put: (...args: unknown[]) => put(...args),
    post: vi.fn(),
    delete: vi.fn()
  }
}))

describe('account traffic admin API', () => {
  beforeEach(() => {
    get.mockReset()
    put.mockReset()
  })

  it('GETs and PUTs the registered /traffic route, not traffic-control', async () => {
    const policy = defaultTrafficPolicy()
    get.mockResolvedValue({ data: { policy, state: null, state_available: true, hard_limit: 8 } })
    put.mockResolvedValue({ data: { policy, state_available: true, hard_limit: 8 } })

    await accountTrafficAPI.get(8)
    expect(get).toHaveBeenCalledWith('/admin/accounts/8/traffic', expect.objectContaining({}))
    expect(get.mock.calls[0][0]).not.toContain('traffic-control')

    await accountTrafficAPI.save(8, { ...policy, strict_rpm_enabled: true })
    expect(put).toHaveBeenCalledWith(
      '/admin/accounts/8/traffic',
      expect.objectContaining({ strict_rpm_enabled: true })
    )
    expect(put.mock.calls[0][0]).not.toContain('traffic-control')
  })
})
