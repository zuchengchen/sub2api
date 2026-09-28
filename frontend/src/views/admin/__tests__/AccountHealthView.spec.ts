import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import AccountHealthView from '../AccountHealthView.vue'

vi.mock('@/api/admin/accountHealth', () => {
  const accountHealthAPI = {
    snapshot: vi.fn().mockResolvedValue({ items: [] }),
    getSettings: vi.fn().mockResolvedValue({
      enabled: true,
      window_minutes: 10,
      min_samples: 10,
      isolate_err_rate: 0.5,
      recover_err_rate: 0.2,
      cooldown_minutes: 30,
      interval_seconds: 60
    })
  }
  // @/api/admin 用默认导出组装 adminAPI.accountHealth，视图经由它调用。
  return { accountHealthAPI, default: accountHealthAPI }
})
vi.mock('@/components/layout/AppLayout.vue', () => ({ default: { template: '<div><slot /></div>' } }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showError: vi.fn(), showSuccess: vi.fn() }) }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ isAdmin: true }) }))

describe('AccountHealthView', () => {
  it('renders independent account health snapshot controls', async () => {
    const i18n = createI18n({ legacy: false, locale: 'zh', messages: { zh: { nav: { accountHealth: '账号健康' } } } })
    const wrapper = mount(AccountHealthView, { global: { plugins: [i18n] } })
    await wrapper.vm.$nextTick()
    expect(wrapper.text()).toMatch(/accountHealth|阈值设置|隔离|settings|noData/)
  })
})
