import { describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import { createI18n } from 'vue-i18n'
import AccountHealthView from '../AccountHealthView.vue'
import { accountHealthAPI } from '@/api/admin/accountHealth'

vi.mock('@/api/admin/accountHealth', () => {
  const accountHealthAPI = {
    snapshot: vi.fn(),
    getSettings: vi.fn().mockResolvedValue({
      enabled: true,
      window_minutes: 10,
      min_samples: 10,
      isolate_err_rate: 0.5,
      recover_err_rate: 0.2,
      cooldown_minutes: 30,
      interval_seconds: 60
    }),
    isolate: vi.fn(),
    resume: vi.fn()
  }
  return { accountHealthAPI, default: accountHealthAPI }
})
vi.mock('@/components/layout/AppLayout.vue', () => ({ default: { template: '<div><slot /></div>' } }))
vi.mock('@/components/common/BaseDialog.vue', () => ({ default: { props: ['show'], template: '<div v-if="show"><slot /><slot name="footer" /></div>' } }))
vi.mock('@/stores/app', () => ({ useAppStore: () => ({ showError: vi.fn(), showSuccess: vi.fn() }) }))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ isAdmin: true }) }))

const messages = {
  zh: {
    nav: { accountHealth: '账号健康' },
    common: { refresh: '刷新', actions: '操作', cancel: '取消', save: '保存', saving: '保存中' },
    admin: {
      accountHealth: {
        title: '账号健康',
        description: '全站账号错误率评分',
        settings: '阈值设置',
        account: '账号',
        platform: '平台',
        score: '评分',
        errRate: '错误率',
        latency: '平均延迟',
        samples: '样本',
        state: '状态',
        isolate: '隔离',
        resume: '恢复',
        noData: '窗口内暂无账号活动。',
        noDataForPlatform: '该平台窗口内暂无账号活动。',
        allPlatforms: '全部平台',
        errorCount: '错误 {count}',
        stateHealthy: '健康',
        stateDegraded: '降级',
        stateIsolated: '已隔离'
      }
    }
  }
}

function snapshotRow(id: number, platform: string, score: number) {
  return {
    account_id: id,
    name: `acc-${id}`,
    platform,
    score,
    err_rate: (100 - score) / 100,
    avg_latency_ms: 100,
    total: 10,
    errors: 1,
    state: 'healthy',
    isolated: false,
    evaluated_at: '2026-10-12T00:00:00Z'
  }
}

describe('AccountHealthView', () => {
  it('renders independent account health snapshot controls', async () => {
    vi.mocked(accountHealthAPI.snapshot).mockResolvedValue({ items: [], count: 0 })
    const i18n = createI18n({ legacy: false, locale: 'zh', messages })
    const wrapper = mount(AccountHealthView, { global: { plugins: [i18n], stubs: { Icon: true, Toggle: true } } })
    await flushPromises()
    expect(wrapper.text()).toMatch(/accountHealth|阈值设置|settings|noData/)
    expect(wrapper.get('[data-testid=platform-filter-all]').exists()).toBe(true)
    expect(wrapper.get('[data-testid=platform-filter-openai]').exists()).toBe(true)
    wrapper.unmount()
  })

  it('filters by platform and sorts by score', async () => {
    vi.mocked(accountHealthAPI.snapshot).mockResolvedValue({
      items: [
        snapshotRow(1, 'openai', 90),
        snapshotRow(2, 'openai', 20),
        snapshotRow(3, 'anthropic', 10)
      ],
      count: 3
    })
    const i18n = createI18n({ legacy: false, locale: 'zh', messages })
    const wrapper = mount(AccountHealthView, { global: { plugins: [i18n], stubs: { Icon: true, Toggle: true } } })
    await flushPromises()
    const names = () => wrapper.findAll('tbody tr').map(row => row.text())
    expect(names()[0]).toContain('acc-3')
    expect(names()[1]).toContain('acc-2')
    expect(names()[2]).toContain('acc-1')
    await wrapper.get('[data-testid=platform-filter-openai]').trigger('click')
    expect(names()).toHaveLength(2)
    expect(names()[0]).toContain('acc-2')
    expect(names()[1]).toContain('acc-1')
    await wrapper.get('[data-testid=sort-score]').trigger('click')
    expect(names()[0]).toContain('acc-1')
    wrapper.unmount()
  })
})
