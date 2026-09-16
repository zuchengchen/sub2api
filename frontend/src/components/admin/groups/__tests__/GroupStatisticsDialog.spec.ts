import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import GroupStatisticsDialog from '../GroupStatisticsDialog.vue'
import { getStats } from '@/api/admin/groups'

vi.mock('@/api/admin/groups', async (importOriginal) => {
  const actual = await importOriginal<typeof import('@/api/admin/groups')>()
  return {
    ...actual,
    getStats: vi.fn()
  }
})

const payload = {
  group_id: 7,
  group_name: 'ops-group',
  total_api_keys: 4,
  active_api_keys: 3,
  total_accounts: 2,
  total_requests: 11,
  total_tokens: 900,
  total_cost: 32,
  total_actual_cost: 16,
  total_account_cost: 13,
  balance_cost: 9,
  subscription_cost: 7,
  zero_charge_requests: 1,
  average_duration_ms: 1500,
  from: null,
  to: null,
  generated_at: '2026-09-16T12:00:00.000Z'
}

describe('GroupStatisticsDialog', () => {
  beforeEach(() => {
    vi.mocked(getStats).mockReset()
    vi.mocked(getStats).mockResolvedValue(payload)
  })

  it('renders a read-only popup with distinct user billing, subscription, and upstream costs', async () => {
    const wrapper = mount(GroupStatisticsDialog, {
      props: { groupId: 7 },
      attachTo: document.body,
      global: {
        stubs: {
          BaseDialog: {
            props: ['show', 'title'],
            template: '<div v-if="show"><h3>{{ title }}</h3><slot /><footer><slot name="footer" /></footer></div>'
          }
        }
      }
    })
    await flushPromises()

    expect(getStats).toHaveBeenCalledTimes(1)
    expect(getStats).toHaveBeenCalledWith(7, {}, expect.any(AbortSignal))

    const text = wrapper.text()
    expect(text).toContain('ops-group')
    expect(text).toContain('完整统计')
    expect(text).toContain('实际计费用量')
    expect(text).toContain('账面费用')
    expect(text).toContain('上游账号成本')
    expect(text).toContain('余额计费用量')
    expect(text).toContain('订阅计费用量')
    expect(text).toContain('16.00')
    expect(text).toContain('32.00')
    expect(text).toContain('13.00')
    expect(text).toContain('9.00')
    expect(text).toContain('7.00')
    expect(text).not.toContain('修改定价')
    expect(wrapper.findAll('input[type=datetime-local]')).toHaveLength(2)
    expect(wrapper.findAll('button').map((button) => button.text())).toEqual(['应用 / 刷新', '全部历史', '关闭'])

    await wrapper.findAll('button').find((button) => button.text() === '关闭')!.trigger('click')
    expect(wrapper.emitted('close')).toBeTruthy()
    wrapper.unmount()
  })
})
