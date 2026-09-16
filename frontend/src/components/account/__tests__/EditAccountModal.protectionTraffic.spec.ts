import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import EditAccountModal from '../EditAccountModal.vue'

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError: vi.fn(), showSuccess: vi.fn(), showInfo: vi.fn() })
}))
vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({ isSimpleMode: false, isAdmin: true })
}))
vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      getById: vi.fn(),
      update: vi.fn()
    },
    settings: {
      getWebSearchEmulationConfig: vi.fn().mockResolvedValue({ enabled: false, providers: [] }),
      getSettings: vi.fn().mockResolvedValue({})
    },
    tlsFingerprintProfiles: { list: vi.fn().mockResolvedValue([]) }
  }
}))
vi.mock('@/api/admin/accountTraffic', () => ({
  accountTrafficAPI: {
    get: vi.fn().mockResolvedValue({
      policy: {
        strict_rpm_enabled: false,
        rpm: 60,
        burst: 5,
        adaptive_enabled: false,
        adaptive_mode: 'observe',
        min_concurrency: 1,
        failure_threshold: 3,
        failure_window_seconds: 60,
        recovery_seconds: 60
      },
      state: null,
      state_available: false,
      hard_limit: 3
    })
  },
  defaultTrafficPolicy: () => ({
    strict_rpm_enabled: false,
    rpm: 60,
    burst: 5,
    adaptive_enabled: false,
    adaptive_mode: 'observe',
    min_concurrency: 1,
    failure_threshold: 3,
    failure_window_seconds: 60,
    recovery_seconds: 60
  }),
  normalizeTrafficDraft: (value: unknown) => value,
  trafficPolicyError: () => ''
}))
vi.mock('@/api/admin/accounts', async () => {
  const actual = await vi.importActual<typeof import('@/api/admin/accounts')>('@/api/admin/accounts')
  return { ...actual, setProtection: vi.fn() }
})

const i18n = createI18n({
  legacy: false,
  locale: 'zh',
  messages: { zh: { common: { name: '名称' }, admin: { accounts: { editAccount: '编辑账号', notes: '备注', notesPlaceholder: '', notesHint: '' } } } }
})

describe('EditAccountModal protection and traffic partitions', () => {
  it('renders independent protection and traffic sections', () => {
    const wrapper = mount(EditAccountModal, {
      props: {
        show: true,
        account: {
          id: 8,
          name: 'oai',
          platform: 'openai',
          type: 'oauth',
          concurrency: 3,
          extra: { auto_pause_5h_threshold: 0.95 },
          proxy_id: null,
          group_ids: []
        } as never,
        proxies: [],
        groups: []
      },
      global: { plugins: [i18n], stubs: { BaseDialog: { template: '<div><slot /><slot name="footer" /></div>' } } }
    })
    expect(wrapper.find('[data-testid="account-protection-section"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="account-traffic-section"]').exists()).toBe(true)
    expect(wrapper.find('[data-testid="account-traffic-controls"]').exists()).toBe(true)
  })
})
