import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { nextTick } from 'vue'

import { keysAPI } from '@/api'
import KeysView from '../KeysView.vue'

const { listKeys, getPublicSettings, getDashboardApiKeysUsage, getAvailableGroups, getUserGroupRates } = vi.hoisted(() => ({
  listKeys: vi.fn(),
  getPublicSettings: vi.fn(),
  getDashboardApiKeysUsage: vi.fn(),
  getAvailableGroups: vi.fn(),
  getUserGroupRates: vi.fn(),
}))

vi.mock('@/api', () => ({
  keysAPI: {
    list: listKeys,
    create: vi.fn(),
    update: vi.fn(),
    delete: vi.fn(),
    toggleStatus: vi.fn(),
  },
  authAPI: { getPublicSettings },
  usageAPI: { getDashboardApiKeysUsage },
  userGroupsAPI: { getAvailable: getAvailableGroups, getUserGroupRates },
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError: vi.fn(), showSuccess: vi.fn() }),
}))

vi.mock('@/stores/onboarding', () => ({
  useOnboardingStore: () => ({ isCurrentStep: () => false, nextStep: vi.fn() }),
}))

vi.mock('@/composables/useClipboard', () => ({
  useClipboard: () => ({ copyToClipboard: vi.fn() }),
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string) => key,
    }),
  }
})

describe('KeysView concurrency field', () => {
  beforeEach(() => {
    listKeys.mockResolvedValue({ items: [], total: 0, page: 1, page_size: 20, pages: 0 })
    getPublicSettings.mockResolvedValue({})
    getDashboardApiKeysUsage.mockResolvedValue({ stats: {} })
    getAvailableGroups.mockResolvedValue([])
    getUserGroupRates.mockResolvedValue({})
  })

  it('exposes an editable concurrency input defaulting to unlimited', async () => {
    const wrapper = mount(KeysView, {
      global: {
        stubs: {
          AppLayout: { template: '<div><slot /></div>' },
          TablePageLayout: {
            template: '<div><slot name="filters" /><slot name="actions" /><slot name="table" /><slot name="pagination" /></div>',
          },
          DataTable: { template: '<div />' },
          Pagination: true,
          BaseDialog: {
            props: ['show'],
            template: '<div v-if="show" role="dialog"><slot /><slot name="footer" /></div>',
          },
          ConfirmDialog: true,
          EmptyState: true,
          Select: true,
          SearchInput: true,
          Icon: true,
          UseKeyModal: true,
          BulkEditKeysModal: true,
          EndpointPopover: true,
          GroupBadge: true,
          GroupOptionItem: true,
          Teleport: true,
        },
      },
    })
    await flushPromises()
    await nextTick()
    await wrapper.get('[data-tour="keys-create-btn"]').trigger('click')
    await nextTick()
    const input = wrapper.get('[data-testid="key-concurrency"]')
    expect((input.element as HTMLInputElement).value).toBe('0')
    expect(wrapper.text()).toContain('keys.concurrencyHint')
    await input.setValue(3)
    expect((input.element as HTMLInputElement).value).toBe('3')
    expect(vi.mocked(keysAPI.create)).not.toHaveBeenCalled()
  })
})
