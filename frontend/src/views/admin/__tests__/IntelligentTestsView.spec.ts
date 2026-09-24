import { defineComponent } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import { beforeEach, describe, expect, it, vi } from 'vitest'
import { createI18n } from 'vue-i18n'
import { createMemoryHistory, createRouter } from 'vue-router'

vi.mock('@/api/intelligentTests', () => ({
  intelligentTestsAPI: {
    accounts: vi.fn().mockResolvedValue({ items: [], total: 0, page: 1, page_size: 24, overview: {} }),
    records: vi.fn().mockResolvedValue({ items: [], total: 0, page: 1, page_size: 24 }),
    settings: vi.fn().mockResolvedValue([
      {
        test_type: 'pelican',
        name: '鹈鹕',
        enabled: true,
        user_visible: false,
        config: { prompt: 'Draw a pelican', model: '', evaluator: 'svg_structure', timeout_seconds: 180 }
      },
      {
        test_type: 'candy',
        name: '糖果',
        enabled: true,
        user_visible: false,
        config: { prompt: '12 minus 5?', model: '', evaluator: 'exact_answer', expected_answer: '7', timeout_seconds: 120 }
      }
    ])
  },
  newTestRequestKey: vi.fn(() => 'test-key')
}))
vi.mock('@/api/admin/groups', () => ({
  getAllIncludingInactive: vi.fn().mockResolvedValue([
    { id: 9, name: 'GPT-PRO' },
    { id: 10, name: 'GPT-PRO-企业' }
  ])
}))
vi.mock('@/components/layout/AppLayout.vue', () => ({ default: { template: '<div><slot /></div>' } }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showError: vi.fn(), showSuccess: vi.fn() }) }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ isAdmin: true }) }))

import IntelligentTestsView from '../IntelligentTestsView.vue'
import { intelligentTestsAPI } from '@/api/intelligentTests'

const AppLayoutHost = defineComponent({ template: '<router-view />' })

function buildRouter() {
  return createRouter({
    history: createMemoryHistory(),
    routes: [
      { path: '/admin/accounts', name: 'AdminAccounts', component: { template: '<div>accounts</div>' } },
      { path: '/admin/accounts/tests', name: 'AdminIntelligentTests', component: IntelligentTestsView, props: { mode: 'tests' } },
      { path: '/admin/accounts/test-history', name: 'AdminIntelligentTestHistory', component: IntelligentTestsView, props: { mode: 'history' } },
      { path: '/admin/accounts/test-settings', name: 'AdminIntelligentTestSettings', component: IntelligentTestsView, props: { mode: 'settings' } }
    ]
  })
}

describe('IntelligentTestsView router modes', () => {
  const i18n = createI18n({ legacy: false, locale: 'zh', messages: { zh: { nav: { intelligentTests: '智能测试' } } } })

  beforeEach(() => {
    vi.clearAllMocks()
  })

  it('opens settings from router props so pelican/candy visibility is reachable', async () => {
    const router = buildRouter()
    await router.push('/admin/accounts/test-settings')
    const wrapper = mount(AppLayoutHost, { global: { plugins: [i18n, router] } })
    await flushPromises()
    expect(intelligentTestsAPI.settings).toHaveBeenCalled()
    expect(wrapper.text()).toContain('测试设置')
    expect(wrapper.text()).toContain('鹈鹕')
    expect(wrapper.text()).toContain('糖果')
    expect(wrapper.text()).toContain('用户可查看')
    const visibility = wrapper.findAll('input[type=checkbox]').filter(input => {
      const label = input.element.closest('label')
      return label?.textContent?.includes('用户可查看')
    })
    expect(visibility).toHaveLength(2)
    expect(visibility.every(input => !(input.element as HTMLInputElement).checked)).toBe(true)
    wrapper.unmount()
  })

  it('opens history from the tests page using the registered history route', async () => {
    const router = buildRouter()
    vi.mocked(intelligentTestsAPI.accounts).mockResolvedValue({
      items: [{
        account_id: 8,
        name: 'oai',
        platform: 'openai',
        account_type: 'oauth',
        account_status: 'active',
        group_ids: [],
        anti_degradation: true,
        tests: [{ test_type: 'pelican', latest: null, history_count: 1, consecutive_anomalies: 0, risk: '' }]
      }],
      total: 1,
      page: 1,
      page_size: 24,
      overview: { total_accounts: 1, tested_today: 0, success_accounts: 0, abnormal_accounts: 0, suspected_degradation: 0 }
    })
    await router.push('/admin/accounts/tests')
    const wrapper = mount(AppLayoutHost, { global: { plugins: [i18n, router] } })
    await flushPromises()
    expect(intelligentTestsAPI.accounts).toHaveBeenCalledWith(
      expect.objectContaining({ group_id: '9', account_status: 'schedulable', test_type: 'pelican', page: 1 }),
      expect.anything()
    )
    const historyButton = wrapper.findAll('button').find(button => button.text().includes('历史'))
    expect(historyButton).toBeTruthy()
    await historyButton!.trigger('click')
    await flushPromises()
    expect(router.currentRoute.value.path).toBe('/admin/accounts/test-history')
    expect(router.currentRoute.value.query).toMatchObject({ account_id: '8', test_type: 'pelican' })
    expect(intelligentTestsAPI.records).toHaveBeenCalled()
    expect(intelligentTestsAPI.records.mock.calls[0][0]).toMatchObject({ account_id: '8', test_type: 'pelican' })
    wrapper.unmount()
  })
})
