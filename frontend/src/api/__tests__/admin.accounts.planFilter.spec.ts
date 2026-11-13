import { beforeEach, describe, expect, it, vi } from 'vitest'

const { get } = vi.hoisted(() => ({ get: vi.fn() }))
vi.mock('@/api/client', () => ({ apiClient: { get } }))

import { exportData, getUpstreamBillingRatesWithEtag, list, listWithEtag } from '@/api/admin/accounts'

describe('admin account subscription filter API', () => {
  beforeEach(() => {
    get.mockReset().mockResolvedValue({ data: { items: [] }, headers: {}, status: 200 })
  })

  it.each([
    { request: list, endpoint: '/admin/accounts' },
    { request: listWithEtag, endpoint: '/admin/accounts' },
    { request: getUpstreamBillingRatesWithEtag, endpoint: '/admin/accounts/upstream-billing-rates' }
  ])('passes the plan filter to $endpoint', async ({ request, endpoint }) => {
    await request(2, 50, { plan_type: 'prolite' })
    expect(get).toHaveBeenCalledWith(endpoint, expect.objectContaining({
      params: expect.objectContaining({ page: 2, page_size: 50, plan_type: 'prolite' })
    }))
  })

  it('includes plan_type when exporting filtered accounts', async () => {
    await exportData({ filters: { plan_type: 'promax', group: '7' }, includeProxies: false })
    expect(get).toHaveBeenCalledWith('/admin/accounts/data', {
      params: { plan_type: 'promax', group: '7', include_proxies: 'false' }
    })
  })

  it('keeps explicit export selection independent of the active plan filter', async () => {
    await exportData({ ids: [7, 9], filters: { plan_type: 'plus' } })
    expect(get).toHaveBeenCalledWith('/admin/accounts/data', { params: { ids: '7,9' } })
  })
})
