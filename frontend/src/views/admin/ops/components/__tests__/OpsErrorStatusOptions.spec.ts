import { afterEach, describe, expect, it, vi } from 'vitest'
import { enableAutoUnmount, shallowMount } from '@vue/test-utils'
import OpsErrorDetailsModal from '../OpsErrorDetailsModal.vue'
import Select, { type SelectOption } from '@/components/common/Select.vue'
import { COMMON_ERROR_STATUS_CODES } from '@/utils/errorBadges'

vi.mock('vue-i18n', async (importOriginal) => {
  const actual = await importOriginal<typeof import('vue-i18n')>()
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})
vi.mock('@/api/admin/ops', () => ({ opsAPI: { listRequestErrors: vi.fn(), listUpstreamErrors: vi.fn() } }))
enableAutoUnmount(afterEach)

describe('OpsErrorDetailsModal status Other complement', () => {
  it.each(['request', 'upstream'] as const)('matches the %s known-code set', (errorType) => {
    const wrapper = shallowMount(OpsErrorDetailsModal, {
      props: { show: false, timeRange: '1h', errorType },
      global: { stubs: { BaseDialog: { template: '<div><slot /></div>' } } },
    })
    const status = wrapper.findAllComponents(Select).find((select) =>
      select.props('options').some((option: SelectOption) => option.value === 'other'))!
    const codes = status.props('options').map((option: SelectOption) => option.value).filter((value: unknown) => typeof value === 'number')
    expect(codes).toEqual(errorType === 'request' ? COMMON_ERROR_STATUS_CODES : [400, 401, 403, 404, 409, 422, 429, 500, 502, 503, 504, 529])
  })
})
