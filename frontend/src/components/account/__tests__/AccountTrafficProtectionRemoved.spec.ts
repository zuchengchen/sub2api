import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createI18n } from 'vue-i18n'
import type { Account } from '@/types'
import zh from '@/i18n/locales/zh'

const mocks = vi.hoisted(() => ({ update: vi.fn(), retired: vi.fn(), error: vi.fn(), http: vi.fn() }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showError: mocks.error, showSuccess: vi.fn(), showInfo: vi.fn() }) }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ isSimpleMode: false, isAdmin: true }) }))
vi.mock('@/api/client', () => ({ default: { get: mocks.http, post: mocks.http, put: mocks.http } }))
vi.mock('@/api/admin', () => ({ adminAPI: {
  accounts: { getById: vi.fn(), update: mocks.update, previewAntiDegrade: mocks.retired, applyAntiDegrade: mocks.retired, revertAntiDegrade: mocks.retired, setProtection: mocks.retired, checkMixedChannelRisk: vi.fn().mockResolvedValue({ has_risk: false }) },
  settings: { getWebSearchEmulationConfig: vi.fn().mockResolvedValue({ enabled: false, providers: [] }), getSettings: vi.fn().mockResolvedValue({}) },
  tlsFingerprintProfiles: { list: vi.fn().mockResolvedValue([]) }
} }))

import EditAccountModal from '../EditAccountModal.vue'
const BaseDialog = defineComponent({ props: ['show'], template: '<div v-if="show"><slot /><slot name="footer" /></div>' })
const Select = defineComponent({ props: ['modelValue', 'options'], emits: ['update:modelValue'], template: '<select :value="modelValue" @change="$emit(\'update:modelValue\', $event.target.value)"><option v-for="item in options" :value="item.value">{{ item.label }}</option></select>' })
const account = (old: unknown): Account => ({
  id: 8, name: 'retired-policy', platform: 'openai', type: 'oauth', concurrency: 50,
  extra: { account_traffic_control: old, anti_degradation: old, anti_degrade: old, protection_scope: 'codex_v3',
    codex_fingerprint_mode: 'full', codex_fingerprint_seed: '11111111-1111-4111-8111-111111111111',
    codex_image_generation_bridge: true, codex_image_generation_explicit_tool_policy: 'allow',
    enable_tls_fingerprint: true, tls_fingerprint_builtin: 'nodejs24', auto_pause_5h_threshold: 0.95 },
  proxy_id: null, group_ids: [], credentials: {}, priority: 1, rate_multiplier: 1, status: 'active', expires_at: null, auto_pause_on_expired: false
} as Account)

describe('AccountTrafficProtectionRemoved', () => {
  beforeEach(() => {
    vi.clearAllMocks()
    mocks.http.mockResolvedValue({ data: {} })
    mocks.update.mockImplementation(async (_id, payload) => ({ ...account(undefined), ...payload }))
  })
  it.each([undefined, null, false, true, 'malformed', { strict_rpm_enabled: true, rpm: -1, enabled: true, mode: 'mode1' }])('saves an account with retired policy %j and preserves its independent settings', async old => {
    const value = account(old)
    const wrapper = mount(EditAccountModal, { props: { show: true, account: value, proxies: [], groups: [] }, global: { plugins: [createI18n({ legacy: false, locale: 'zh', messages: { zh } })], stubs: { BaseDialog, Select } } })
    await flushPromises()
    expect(wrapper.get('#edit-account-form').exists()).toBe(true)
    expect(wrapper.find('[data-testid="account-traffic-section"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="account-protection-section"]').exists()).toBe(false)
    expect(wrapper.text()).not.toContain('账号保护')
    expect(wrapper.get('[data-testid="edit-codex-fingerprint-mode-select"]').element).toHaveProperty('value', 'full')
    await wrapper.get('#edit-account-form').trigger('submit')
    await flushPromises()
    expect(mocks.update).toHaveBeenCalledTimes(1)
    const payload = mocks.update.mock.calls[0][1]
    expect(payload.concurrency).toBe(50)
    expect(payload.extra).toMatchObject({ codex_fingerprint_mode: 'full', codex_fingerprint_seed: value.extra!.codex_fingerprint_seed, codex_image_generation_bridge: true, enable_tls_fingerprint: true, tls_fingerprint_builtin: 'nodejs24', auto_pause_5h_threshold: 0.95 })
    expect(['strip', 'remove', 'drop']).not.toContain(payload.extra.codex_image_generation_explicit_tool_policy)
    expect(payload.extra.account_traffic_control).toEqual(old)
    expect(payload.extra.anti_degrade).toEqual(old)
    expect(mocks.retired).not.toHaveBeenCalled()
    for (const [url] of mocks.http.mock.calls) expect(String(url)).not.toMatch(/\/(?:traffic|protection|anti-degrade)(?:\/|$)/)
    expect(mocks.error).not.toHaveBeenCalled()
    expect(wrapper.emitted('updated')).toHaveLength(1)
    expect(wrapper.emitted('close')).toHaveLength(1)
    wrapper.unmount()
  })
})
