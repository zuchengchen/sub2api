import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, flushPromises, shallowMount } from '@vue/test-utils'
import OpsErrorDetailsModal from '../OpsErrorDetailsModal.vue'
import OpsErrorLogTable from '../OpsErrorLogTable.vue'

const mocks = vi.hoisted(() => ({ request: vi.fn(), upstream: vi.fn() }))
vi.mock('@/api/admin/ops', () => ({ opsAPI: { listRequestErrors: mocks.request, listUpstreamErrors: mocks.upstream } }))
vi.mock('vue-i18n', async (importOriginal) => {
  const actual = await importOriginal<typeof import('vue-i18n')>()
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})
enableAutoUnmount(afterEach)
beforeEach(() => {
  mocks.request.mockReset().mockResolvedValue({ items: [], total: 0 })
  mocks.upstream.mockReset().mockResolvedValue({ items: [], total: 0 })
})

describe('OpsErrorDetailsModal status scope', () => {
  it.each(['request', 'upstream'] as const)('keeps %s filtering and display aligned', async (errorType) => {
    const wrapper = shallowMount(OpsErrorDetailsModal, {
      props: { show: false, timeRange: '1h', errorType },
      global: { stubs: { BaseDialog: { template: '<div><slot /></div>' } } },
    })
    await wrapper.setProps({ show: true }); await flushPromises()
    const expected = errorType === 'request' ? 'request' : undefined
    const called = errorType === 'request' ? mocks.request : mocks.upstream
    expect(called).toHaveBeenCalled()
    for (const [params] of called.mock.calls) {
      expect(params.status_scope).toBe(expected)
    }
    // Read the bound value from the component even before this optional prop exists.
    const table = wrapper.findComponent(OpsErrorLogTable)
    const bound = table.props('useFinalRequestStatus') ?? table.attributes('use-final-request-status')
    expect(String(bound)).toBe(String(errorType === 'request'))
  })
})
