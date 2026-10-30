import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import AccountStatsModal from '../AccountStatsModal.vue'
import type { Account } from '@/types'
const { getStats } = vi.hoisted(() => ({ getStats: vi.fn() }))
vi.mock('@/api/admin', () => ({ adminAPI: { accounts: { getStats } } }))
vi.mock('vue-i18n', async () => ({
  ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'),
  useI18n: () => ({ t: (key: string) => key })
}))
beforeEach(() => getStats.mockReset())
describe('AccountStatsModal lazy lifecycle', () => {
  it('initializes on first show=true, clears on close, and rejects an older opening response', async () => {
    let complete!: (value: unknown) => void
    getStats.mockImplementationOnce(() => new Promise(resolve => { complete = resolve })).mockResolvedValue(null)
    const wrapper = mount(AccountStatsModal, {
      props: { show: true, account: { id: 1, name: 'first' } as Account },
      global: { stubs: { BaseDialog: { props: ['show'], template: '<div v-if="show"><slot /></div>' }, ModelDistributionChart: true, EndpointDistributionChart: true } }
    })
    expect(getStats).toHaveBeenCalledWith(1, 30)
    await wrapper.setProps({ show: false })
    expect((wrapper.vm as any).stats).toBeNull()
    await wrapper.setProps({ show: true })
    await flushPromises()
    expect(getStats).toHaveBeenCalledTimes(2)
    complete({ invalid: 'stale response should never render' })
    await flushPromises()
    expect((wrapper.vm as any).stats).toBeNull()
    expect((wrapper.vm as any).loading).toBe(false)
    wrapper.unmount()
  })
})
