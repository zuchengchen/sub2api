import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createI18n } from 'vue-i18n'
import type { Account } from '@/types'
import { defaultTrafficPolicy } from '@/api/admin/accountTraffic'

const mocks = vi.hoisted(() => ({
  update: vi.fn(),
  preview: vi.fn(),
  apply: vi.fn(),
  revert: vi.fn(),
  trafficGet: vi.fn(),
  trafficSave: vi.fn()
}))

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
      update: mocks.update,
      previewAntiDegrade: mocks.preview,
      applyAntiDegrade: mocks.apply,
      revertAntiDegrade: mocks.revert,
      checkMixedChannelRisk: vi.fn().mockResolvedValue({ has_risk: false })
    },
    settings: {
      getWebSearchEmulationConfig: vi.fn().mockResolvedValue({ enabled: false, providers: [] }),
      getSettings: vi.fn().mockResolvedValue({})
    },
    tlsFingerprintProfiles: { list: vi.fn().mockResolvedValue([]) }
  }
}))
vi.mock('@/api/admin/accountTraffic', async () => ({
  ...await vi.importActual('@/api/admin/accountTraffic'),
  accountTrafficAPI: { get: mocks.trafficGet, save: mocks.trafficSave }
}))

import EditAccountModal from '../EditAccountModal.vue'
import AccountTrafficControls from '../AccountTrafficControls.vue'

const i18n = createI18n({
  legacy: false,
  locale: 'zh',
  messages: { zh: { common: { name: '名称' }, admin: { accounts: { editAccount: '编辑账号', notes: '备注', notesPlaceholder: '', notesHint: '', accountUpdated: '已更新' } } } }
})

const BaseDialogStub = defineComponent({
  props: ['show', 'title'],
  template: '<div v-if="show" :data-title="title"><slot /><slot name="footer" /></div>'
})

const account = (extra: Record<string, unknown> = {}, overrides: Partial<Account> = {}) => ({
  id: 8,
  name: 'oai',
  platform: 'openai',
  type: 'oauth',
  concurrency: 8,
  extra: { auto_pause_5h_threshold: 0.95, ...extra },
  proxy_id: null,
  group_ids: [],
  credentials: {},
  priority: 1,
  rate_multiplier: 1,
  status: 'active',
  expires_at: null,
  auto_pause_on_expired: false,
  ...overrides
} as Account)

function mountModal(value = account()) {
  return mount(EditAccountModal, {
    props: { show: true, account: value, proxies: [], groups: [] },
    global: { plugins: [i18n], stubs: { BaseDialog: BaseDialogStub } }
  })
}

describe('EditAccountModal protection and traffic partitions', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    mocks.update.mockImplementation(async (_id, payload) => account(payload.extra, payload))
    mocks.trafficGet.mockResolvedValue({
      policy: defaultTrafficPolicy(),
      state: null,
      state_available: true,
      hard_limit: 8
    })
    mocks.trafficSave.mockImplementation(async (_id, policy) => ({
      policy,
      state_available: true,
      hard_limit: 8
    }))
    mocks.preview.mockResolvedValue({ account_id: 8, eligible: true, enabled: false, changes: [{ key: 'mode', from: '', to: 'mode1' }] })
    mocks.apply.mockResolvedValue(account({ anti_degrade: { enabled: true, mode: 'mode1' } }, { anti_degradation: true, protection_scope: 'codex_v3' }))
    mocks.revert.mockResolvedValue(account())
  })

  it('persists embedded traffic on the account save path instead of the independent save button', async () => {
    const wrapper = mountModal()
    await flushPromises()
    expect(wrapper.find('[data-testid="account-traffic-controls"]').exists()).toBe(true)
    expect(wrapper.findAll('button').some(button => button.text() === '保存流量控制')).toBe(false)

    await wrapper.get('[data-testid=strict-rpm-toggle]').setValue(true)
    await wrapper.get('#edit-account-form').trigger('submit')
    await flushPromises()

    expect(mocks.update).toHaveBeenCalledTimes(1)
    expect(mocks.update.mock.calls[0][1].extra.account_traffic_control).toMatchObject({
      strict_rpm_enabled: true
    })
    expect(mocks.update.mock.calls[0][1].extra.auto_pause_5h_threshold).toBe(0.95)
    expect(mocks.trafficSave).toHaveBeenCalledWith(8, expect.objectContaining({ strict_rpm_enabled: true }))
    wrapper.unmount()
  })

  it('previews and applies mode1 from account edit instead of toggling default legacy', async () => {
    const wrapper = mountModal()
    await flushPromises()
    expect(wrapper.get('[data-testid=anti-degrade-mode-legacy]').exists()).toBe(true)
    expect(wrapper.get('[data-testid=anti-degrade-mode-1]').exists()).toBe(true)

    await wrapper.get('[data-testid=anti-degrade-mode-1]').trigger('click')
    await flushPromises()
    expect(mocks.preview).toHaveBeenCalledWith(8, 'mode1')
    expect(mocks.apply).not.toHaveBeenCalled()

    await wrapper.get('[data-testid=anti-degrade-confirm]').trigger('click')
    await flushPromises()
    expect(mocks.apply).toHaveBeenCalledWith(8, 'mode1')
    expect(wrapper.emitted('updated')?.[0]?.[0]).toMatchObject({
      anti_degradation: true,
      protection_scope: 'codex_v3'
    })
    wrapper.unmount()
  })

  it('keeps the traffic draft when validating against the edited concurrency ceiling', async () => {
    const wrapper = mountModal()
    await flushPromises()
    const concurrency = wrapper.findAll('input[type=number]').find(input => (input.element as HTMLInputElement).value === '8')!
    await concurrency.setValue(16)
    await wrapper.get('[data-testid=adaptive-toggle]').setValue(true)
    const minConcurrency = wrapper.findComponent(AccountTrafficControls).findAll('input[type=number]')[0]
    await minConcurrency.setValue(12)
    await wrapper.get('#edit-account-form').trigger('submit')
    await flushPromises()
    expect(mocks.update.mock.calls[0][1]).toMatchObject({
      concurrency: 16,
      extra: { account_traffic_control: { adaptive_enabled: true, min_concurrency: 12 } }
    })
    expect(mocks.trafficSave).toHaveBeenCalledWith(8, expect.objectContaining({
      adaptive_enabled: true,
      min_concurrency: 12
    }))
    wrapper.unmount()
  })
})
