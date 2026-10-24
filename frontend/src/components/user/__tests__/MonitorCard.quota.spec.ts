import { mount } from '@vue/test-utils'
import { describe, expect, it, vi } from 'vitest'

import type { UserMonitorView } from '@/api/channelMonitor'
import MonitorCard from '../monitor/MonitorCard.vue'

const { isQuotaVisible } = vi.hoisted(() => ({
  isQuotaVisible: vi.fn(() => false),
}))

vi.mock('@/utils/featureFlags', () => ({
  isChannelMonitorQuotaVisible: () => isQuotaVisible(),
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ cachedPublicSettings: { server_utc_offset: '+08:00' } }),
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({ t: (key: string, params?: Record<string, unknown>) => {
      if (key === 'common.peakRateTooltip') return `peak:${params?.window ?? ''}`
      return key
    }, te: () => true }),
  }
})

function makeItem(overrides: Partial<UserMonitorView> = {}): UserMonitorView {
  return {
    id: 1,
    name: 'claude-main',
    provider: 'kimi',
    group_name: '',
    primary_model: 'quota',
    primary_status: 'operational',
    primary_latency_ms: null,
    primary_ping_latency_ms: null,
    availability_7d: 100,
    extra_models: [],
    timeline: [],
    ...overrides,
  }
}

function mountCard(item: UserMonitorView) {
  return mount(MonitorCard, {
    props: {
      item,
      window: '7d',
      availabilityValue: 100,
      countdownSeconds: 0,
    },
    global: {
      stubs: {
        MonitorMetricPair: true,
        MonitorAvailabilityRow: true,
        MonitorTimeline: true,
      },
    },
  })
}

describe('MonitorCard quota snapshot visibility', () => {
  it('hides the quota block when the system switch is off even if data exists', () => {
    isQuotaVisible.mockReturnValue(false)
    const wrapper = mountCard(
      makeItem({
        latest_quota: { source: 'cn_quota', success: true, fetched_at: '2026-08-18T00:00:00Z' },
      }),
    )
    expect(wrapper.find('[data-testid="monitor-quota-view"]').exists()).toBe(false)
  })

  it('renders the quota block when the switch is on and a snapshot exists', () => {
    isQuotaVisible.mockReturnValue(true)
    const wrapper = mountCard(
      makeItem({
        latest_quota: {
          source: 'cn_quota',
          success: true,
          plan_level: 'kimi-plus',
          tiers: [{ window: 'daily', label: 'requests', used_percent: 60 }],
          fetched_at: '2026-08-18T00:00:00Z',
        },
      }),
    )
    expect(wrapper.find('[data-testid="monitor-quota-view"]').exists()).toBe(true)
    expect(wrapper.text()).toContain('kimi-plus')
  })

  it('never renders the quota block without a snapshot', () => {
    isQuotaVisible.mockReturnValue(true)
    const wrapper = mountCard(makeItem())
    expect(wrapper.find('[data-testid="monitor-quota-view"]').exists()).toBe(false)
  })

  // 卡片第二行不再展示模型名，占位符 "quota" 不得作为假模型名漏出。
  it('does not leak the quota placeholder model on the card', () => {
    const wrapper = mountCard(makeItem())
    expect(wrapper.text()).not.toContain('quota')
    expect(wrapper.text()).not.toContain('monitorCommon.checkMode.quota')
  })

  it('keeps the primary model off the card header', () => {
    const wrapper = mountCard(makeItem({ primary_model: 'claude-sonnet-4-5' }))
    expect(wrapper.text()).not.toContain('claude-sonnet-4-5')
  })

  it('shows only the group name and a highlighted rate on the second line', () => {
    const wrapper = mountCard(
      makeItem({
        name: 'DeepSeek',
        provider: 'openai',
        group_name: 'DeepSeek',
        group_id: 84,
        rate_multiplier: 1,
        primary_model: 'deepseek-chat',
      }),
    )
    expect(wrapper.text()).toContain('DeepSeek')
    expect(wrapper.text()).not.toContain('deepseek-chat')
    expect(wrapper.text()).not.toContain('monitorCommon.providers.openai')
    const rate = wrapper.get('[data-testid="monitor-card-rate"]')
    expect(rate.text()).toBe('×1.00')
    expect(rate.classes()).toContain('text-lg')
    expect(rate.classes()).toContain('font-extrabold')
  })

  it('highlights a user-specific rate and strikes through the group default', () => {
    const wrapper = mount(MonitorCard, {
      props: {
        item: makeItem({
          name: 'DeepSeek',
          provider: 'openai',
          group_name: 'DeepSeek',
          group_id: 84,
          rate_multiplier: 1,
          primary_model: 'deepseek-chat',
        }),
        window: '7d',
        availabilityValue: 100,
        countdownSeconds: 0,
        userGroupRates: { 84: 0.8 },
      },
      global: {
        stubs: {
          MonitorMetricPair: true,
          MonitorAvailabilityRow: true,
          MonitorTimeline: true,
        },
      },
    })
    expect(wrapper.get('[data-testid="monitor-card-rate"]').text()).toBe('×0.80')
    expect(wrapper.text()).toContain('×1.00')
    expect(wrapper.text()).not.toContain('deepseek-chat')
  })

  it('keeps peak-rate details next to the highlighted multiplier', () => {
    const wrapper = mountCard(
      makeItem({
        name: 'Kiro2Claude',
        group_name: 'Kiro2Claude',
        rate_multiplier: 0.5,
        peak_rate_enabled: true,
        peak_start: '09:00',
        peak_end: '18:00',
        peak_rate_multiplier: 0.8,
      }),
    )
    expect(wrapper.get('[data-testid="monitor-card-rate"]').text()).toBe('×0.50')
    expect(wrapper.text()).toContain('09:00-18:00 ×0.8 (UTC+08:00)')
  })
})
