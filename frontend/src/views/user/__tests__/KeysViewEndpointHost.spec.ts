/**
 * The endpoint chip on /keys used to render the single global `api_base_url`,
 * so a deployment reachable under several domains advertised the same host on
 * all of them. These tests cover the `api_base_url_follow_host` opt-in and the
 * unchanged behaviour when it stays off.
 */
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createPinia, setActivePinia } from 'pinia'

const {
  listKeys,
  getPublicSettings,
  getDashboardApiKeysUsage,
  getAvailableGroups,
  getUserGroupRates,
} = vi.hoisted(() => ({
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
  useClipboard: () => ({ copyToClipboard: vi.fn().mockResolvedValue(true) }),
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

import KeysView from '../KeysView.vue'

const EndpointPopoverStub = {
  name: 'EndpointPopover',
  props: ['apiBaseUrl', 'customEndpoints'],
  template: '<div data-test="endpoint-chip">{{ apiBaseUrl }}</div>',
}

const PassthroughStub = { template: '<div><slot /></div>' }
// The endpoint chip lives in TablePageLayout's #filters slot.
const TablePageLayoutStub = {
  template: '<div><slot name="filters" /><slot name="actions" /><slot /></div>',
}

async function mountKeys() {
  const wrapper = mount(KeysView, {
    global: {
      stubs: {
        AppLayout: PassthroughStub,
        TablePageLayout: TablePageLayoutStub,
        DataTable: true,
        Pagination: true,
        BaseDialog: true,
        ConfirmDialog: true,
        EmptyState: true,
        Select: true,
        SearchInput: true,
        Icon: true,
        UseKeyModal: true,
        GroupBadge: true,
        GroupOptionItem: true,
        Teleport: true,
        EndpointPopover: EndpointPopoverStub,
      },
    },
  })
  await flushPromises()
  return wrapper
}

function setLocation(origin: string) {
  Object.defineProperty(window, 'location', {
    configurable: true,
    value: new URL(origin) as unknown as Location,
  })
}

describe('KeysView 端点跟随访问域名', () => {
  beforeEach(() => {
    setActivePinia(createPinia())
    localStorage.clear()
    vi.clearAllMocks()

    listKeys.mockResolvedValue({ items: [], total: 0, page: 1, page_size: 20, pages: 0 })
    getDashboardApiKeysUsage.mockResolvedValue({ stats: {} })
    getAvailableGroups.mockResolvedValue([])
    getUserGroupRates.mockResolvedValue({})
    setLocation('https://key66.cc.cd')
  })

  it('开启开关后端点显示当前访问域名并保留路径', async () => {
    getPublicSettings.mockResolvedValue({
      api_base_url: 'https://key66.vip/v1',
      api_base_url_follow_host: true,
      custom_endpoints: [],
    })

    const wrapper = await mountKeys()
    const chip = wrapper.get('[data-test="endpoint-chip"]')

    expect(chip.text()).toBe('https://key66.cc.cd/v1')
    expect(chip.text()).not.toContain('key66.vip')
  })

  it('未开启开关时仍显示配置的固定端点', async () => {
    getPublicSettings.mockResolvedValue({
      api_base_url: 'https://key66.vip/v1',
      api_base_url_follow_host: false,
      custom_endpoints: [],
    })

    const wrapper = await mountKeys()

    expect(wrapper.get('[data-test="endpoint-chip"]').text()).toBe('https://key66.vip/v1')
  })

  it('未配置端点且未开启开关时不展示端点（保持原行为）', async () => {
    getPublicSettings.mockResolvedValue({ api_base_url: '', custom_endpoints: [] })

    const wrapper = await mountKeys()

    expect(wrapper.find('[data-test="endpoint-chip"]').exists()).toBe(false)
  })

  it('未配置端点但开启开关时展示当前访问域名', async () => {
    getPublicSettings.mockResolvedValue({
      api_base_url: '',
      api_base_url_follow_host: true,
      custom_endpoints: [],
    })

    const wrapper = await mountKeys()

    expect(wrapper.get('[data-test="endpoint-chip"]').text()).toBe('https://key66.cc.cd')
  })
})
