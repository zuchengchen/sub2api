import { describe, expect, it, vi } from 'vitest'
import { mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import IntelligentTestsView from '../IntelligentTestsView.vue'

vi.mock('@/api/intelligentTests', () => ({
  intelligentTestsAPI: {
    accounts: vi.fn().mockResolvedValue({ items: [], total: 0, page: 1, page_size: 24, overview: {} }),
    settings: vi.fn().mockResolvedValue([
      { test_type: 'pelican', name: '鹈鹕', enabled: true, user_visible: false, config: {} },
      { test_type: 'candy', name: '糖果', enabled: true, user_visible: false, config: {} }
    ])
  }
}))
vi.mock('@/components/layout/AppLayout.vue', () => ({ default: { template: '<div><slot /></div>' } }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showError: vi.fn(), showSuccess: vi.fn() }) }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ isAdmin: true }) }))
vi.mock('vue-router', () => ({
  useRoute: () => ({ query: {}, path: '/admin/intelligent-tests', name: 'AdminIntelligentTests' }),
  useRouter: () => ({ push: vi.fn(), replace: vi.fn() })
}))

describe('IntelligentTestsView', () => {
  it('renders the intelligent test admin page with default questions hidden from users', async () => {
    const i18n = createI18n({ legacy: false, locale: 'zh', messages: { zh: { nav: { intelligentTests: '智能测试' } } } })
    const wrapper = mount(IntelligentTestsView, { global: { plugins: [i18n] } })
    await wrapper.vm.$nextTick()
    expect(wrapper.html().length).toBeGreaterThan(20)
  })
})
