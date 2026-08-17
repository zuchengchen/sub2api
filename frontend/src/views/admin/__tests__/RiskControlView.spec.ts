import { beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h } from 'vue'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'

import RiskControlView from '../RiskControlView.vue'
import type {
  ContentModerationConfig,
  ContentModerationLog,
  UpdateContentModerationConfig,
} from '@/api/admin/riskControl'

const { getConfig, updateConfig, getStatus, listLogs, testDeepSeekChannel, getGroups, showError, showSuccess } =
  vi.hoisted(() => ({
    getConfig: vi.fn(),
    updateConfig: vi.fn(),
    getStatus: vi.fn(),
    listLogs: vi.fn(),
    testDeepSeekChannel: vi.fn(),
    getGroups: vi.fn(),
    showError: vi.fn(),
    showSuccess: vi.fn(),
  }))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    riskControl: {
      getConfig,
      updateConfig,
      getStatus,
      listLogs,
      testDeepSeekChannel,
    },
    groups: { getAll: getGroups },
  },
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError, showSuccess }),
}))

vi.mock('@/utils/apiError', () => ({
  extractApiErrorMessage: (_error: unknown, fallback: string) => fallback,
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, string | number>) =>
        key.replace(/\{(\w+)\}/g, (_match, token) => String(params?.[token] ?? `{${token}}`)),
    }),
  }
})

const officialChannel = () => ({
  id: 'deepseek-official',
  name: 'DeepSeek Official',
  base_url: 'https://api.deepseek.com',
  model: 'deepseek-v4-flash',
  enabled: true,
  order: 0,
  timeout_ms: 3000,
  api_key_configured: true,
  api_key_masked: 'sk-...1234',
  health_status: 'reachable',
  last_health_checked_at: '2026-08-17T01:00:00Z',
  breaker_status: 'closed' as const,
  last_latency_ms: 820,
})

const backupChannel = () => ({
  id: 'deepseek-backup',
  name: 'Backup',
  base_url: 'https://backup.example.com/v1',
  model: 'deepseek-v4-flash',
  enabled: true,
  order: 1,
  timeout_ms: 2800,
  api_key_configured: true,
  api_key_masked: 'sk-...5678',
  health_status: 'unknown',
  breaker_status: 'cooldown' as const,
  cooldown_until: '2099-08-17T01:01:00Z',
  last_latency_ms: 3000,
  last_error: 'timeout',
})

const baseConfig = (): ContentModerationConfig => ({
  enabled: true,
  mode: 'pre_block',
  deepseek_enabled: true,
  yufeng_enabled: false,
  deepseek_total_timeout_ms: 10000,
  deepseek_threshold: 0.8,
  policy_version: 'deepseek-v4-flash-audit-v1',
  deepseek_channels: [officialChannel(), backupChannel()],
  all_groups: true,
  group_ids: [],
  user_email_whitelist: [],
  record_non_hits: false,
  block_status: 403,
  block_message: 'request blocked',
  email_on_hit: true,
  auto_ban_enabled: true,
  cyber_policy_exclude_from_ban_count: false,
  ban_threshold: 10,
  violation_window_hours: 720,
  hit_retention_days: 180,
  non_hit_retention_days: 3,
  model_filter: { type: 'all', models: [] },
  first_layer_stage: 'shadow',
  second_layer_enabled: true,
  second_layer_stage: 'shadow',
  layer1_keywords: ['steal credentials from target'],
  layer2_keywords: ['credentials'],
})

const runtimeStatus = (enforceReady = true) => ({
  enabled: true,
  risk_control_enabled: true,
  mode: 'pre_block' as const,
  pre_block_checked: 42,
  deepseek_failover_count: 3,
  deepseek_unavailable_count: 1,
  second_layer_cache_hits: 23,
  second_layer_cache_misses: 7,
  second_layer_cache_writes: 5,
  second_layer_cache_errors: 1,
  second_layer_enforce_ready: enforceReady,
  second_layer_enforce_reason: enforceReady ? '' : 'connectivity check required',
})

const auditLog = (): ContentModerationLog => ({
  id: 19,
  created_at: '2026-08-17T01:00:00Z',
  request_id: 'req-19',
  user_id: 7,
  user_email: 'user@example.com',
  group_name: 'Production',
  provider: 'deepseek',
  model: 'gpt-5.5',
  action: 'second_layer_shadow',
  decision_source: 'deepseek_primary',
  keyword_tier: 'layer2',
  flagged: true,
  input_excerpt: '[redacted evidence]',
  upstream_latency_ms: 1120,
  deepseek_confidence: 0.91,
  deepseek_category: 'cyber_abuse',
  deepseek_reason: '明确攻击意图',
  review_outcome: 'shadow_risk',
  reviewer_disagreement: true,
  review_attempts: [
    { channel_id: 'deepseek-official', channel_name: 'DeepSeek Official', outcome: 'timeout', latency_ms: 3000 },
    { channel_id: 'deepseek-backup', channel_name: 'Backup', outcome: 'risk', latency_ms: 1120 },
  ],
  evidence_windows: [
    {
      path: 'messages[0].content',
      context_class: 'user',
      text: 'redacted text',
      matches: [{ keyword: 'credentials', rule_id: 'layer2-1', tier: 'layer2', start: 0, end: 11 }],
    },
  ],
})

const AppLayoutStub = { template: '<div><slot /></div>' }
const BaseDialogStub = defineComponent({
  props: { show: { type: Boolean, default: false } },
  template: '<div v-if="show"><slot /><slot name="footer" /></div>',
})
const ToggleStub = defineComponent({
  inheritAttrs: false,
  props: { modelValue: { type: Boolean, required: true } },
  emits: ['update:modelValue'],
  setup(props, { attrs, emit }) {
    return () =>
      h('button', {
        ...attrs,
        type: 'button',
        role: 'switch',
        'aria-checked': String(props.modelValue),
        onClick: () => emit('update:modelValue', !props.modelValue),
      })
  },
})

function mountView(): VueWrapper {
  return mount(RiskControlView, {
    global: {
      stubs: {
        AppLayout: AppLayoutStub,
        BaseDialog: BaseDialogStub,
        Icon: true,
        Toggle: ToggleStub,
        Pagination: true,
      },
    },
  })
}

function configFromUpdate(payload: UpdateContentModerationConfig): ContentModerationConfig {
  const channels = payload.deepseek_channels?.map((channel) => ({
    ...channel,
    api_key_configured: Boolean(channel.api_key) || channel.id !== 'new-without-key',
    api_key_masked: channel.api_key ? 'sk-...new' : 'sk-...saved',
    health_status: 'unknown',
    breaker_status: 'unknown' as const,
  }))
  return {
    ...baseConfig(),
    ...payload,
    deepseek_channels: channels ?? baseConfig().deepseek_channels,
    model_filter: payload.model_filter ?? baseConfig().model_filter,
  }
}

describe('admin RiskControlView', () => {
  beforeEach(() => {
    getConfig.mockReset()
    updateConfig.mockReset()
    getStatus.mockReset()
    listLogs.mockReset()
    testDeepSeekChannel.mockReset()
    getGroups.mockReset()
    showError.mockReset()
    showSuccess.mockReset()

    getConfig.mockResolvedValue(baseConfig())
    updateConfig.mockImplementation(async (payload: UpdateContentModerationConfig) => configFromUpdate(payload))
    getStatus.mockResolvedValue(runtimeStatus())
    listLogs.mockResolvedValue({ items: [], total: 0, page: 1, page_size: 20, pages: 1 })
    testDeepSeekChannel.mockResolvedValue({
      channel_id: 'deepseek-official',
      reachable: true,
      health_valid: true,
      latency_ms: 18,
      http_status: 404,
      checked_at: '2026-08-17T01:02:00Z',
    })
    getGroups.mockResolvedValue([])
  })

  it('renders DeepSeek as the default risk reviewer', async () => {
    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.get('[data-test="deepseek-enabled"]').attributes('aria-checked')).toBe('true')
    expect(wrapper.get('[data-test="yufeng-enabled"]').attributes('aria-checked')).toBe('false')
    expect(wrapper.get('[data-test="deepseek-threshold"]').element).toHaveProperty('value', '80%')
    expect(wrapper.text()).toContain('deepseek-v4-flash-audit-v1')
    expect(wrapper.text()).toContain('admin.riskControl.nonThinking')
    expect(wrapper.find('input[type="file"]').exists()).toBe(false)

    wrapper.unmount()
  })

  it('shows production Layer 2 dedup cache counters in the overview', async () => {
    const wrapper = mountView()
    await flushPromises()

    const cacheCard = wrapper.get('[data-test="risk-overview-second-layer-cache"]')
    expect(cacheCard.text()).toContain('admin.riskControl.overview.secondLayerCache')
    expect(cacheCard.attributes('data-cache-hits')).toBe('23')
    expect(cacheCard.attributes('data-cache-misses')).toBe('7')
    expect(cacheCard.attributes('data-cache-writes')).toBe('5')
    expect(cacheCard.attributes('data-cache-errors')).toBe('1')
    expect(cacheCard.find('.text-red-700').exists()).toBe(true)

    wrapper.unmount()
  })

  it('saves reviewer switches, ordered channels, masked-key replacement, and independent stages', async () => {
    const wrapper = mountView()
    await flushPromises()

    await wrapper.get('[data-test="yufeng-enabled"]').trigger('click')
    await wrapper.get('[data-test="cyber-policy-exclude-ban"]').trigger('click')
    await wrapper.get('[data-test="layer1-stage-enforce"]').trigger('click')
    const moveDown = wrapper.get(
      '[data-test="deepseek-channel-0"] button[aria-label="admin.riskControl.moveChannelDown"]'
    )
    await moveDown.trigger('click')
    await wrapper.get('[data-test="deepseek-channel-key-0"]').setValue('replacement-key')
    await wrapper.get('[data-test="save-risk-control"]').trigger('click')
    await flushPromises()

    expect(updateConfig).toHaveBeenCalledWith(
      expect.objectContaining({
        deepseek_enabled: true,
        yufeng_enabled: true,
        cyber_policy_exclude_from_ban_count: true,
        deepseek_threshold: 0.8,
        deepseek_total_timeout_ms: 10000,
        first_layer_stage: 'enforce',
        second_layer_stage: 'shadow',
        deepseek_channels: [
          expect.objectContaining({ id: 'deepseek-backup', order: 0, api_key: 'replacement-key' }),
          expect.objectContaining({ id: 'deepseek-official', order: 1, api_key: undefined }),
        ],
      })
    )

    wrapper.unmount()
  })

  it('keeps Layer 2 Enforce disabled until the backend health gate is valid', async () => {
    getStatus.mockResolvedValue(runtimeStatus(false))
    const blockedWrapper = mountView()
    await flushPromises()

    expect(blockedWrapper.get('[data-test="layer2-stage-enforce"]').attributes('disabled')).toBeDefined()
    expect(blockedWrapper.get('[data-test="enforce-health-gate"]').text()).toContain('connectivity check required')
    blockedWrapper.unmount()

    getStatus.mockResolvedValue(runtimeStatus(true))
    const readyWrapper = mountView()
    await flushPromises()
    const enforceButton = readyWrapper.get('[data-test="layer2-stage-enforce"]')
    expect(enforceButton.attributes('disabled')).toBeUndefined()
    await enforceButton.trigger('click')
    await readyWrapper.get('[data-test="save-risk-control"]').trigger('click')
    await flushPromises()
    expect(updateConfig).toHaveBeenCalledWith(expect.objectContaining({ second_layer_stage: 'enforce' }))
    readyWrapper.unmount()
  })

  it('runs the saved connectivity test for a channel', async () => {
    const wrapper = mountView()
    await flushPromises()

    await wrapper.get('[data-test="test-deepseek-channel-0"]').trigger('click')
    await flushPromises()

    expect(testDeepSeekChannel).toHaveBeenCalledWith('deepseek-official')
    expect(wrapper.get('[data-test="deepseek-channel-test-result-0"]').text()).toContain(
      'admin.riskControl.channelTestReachable'
    )
    expect(showSuccess).toHaveBeenCalledWith('admin.riskControl.channelTestComplete')
    wrapper.unmount()
  })

  it('shows model fields, failover attempts, and reviewer disagreement in audit records', async () => {
    listLogs.mockResolvedValue({ items: [auditLog()], total: 1, page: 1, page_size: 20, pages: 1 })
    const wrapper = mountView()
    await flushPromises()

    expect(listLogs).toHaveBeenCalledWith(expect.objectContaining({ result: 'risky_shadow' }))
    expect(wrapper.text()).toContain('cyber_abuse')
    expect(wrapper.text()).toContain('91%')
    expect(wrapper.text()).toContain('admin.riskControl.reviewerDisagreement')
    expect(wrapper.text()).toContain('DeepSeek Official')
    expect(wrapper.text()).toContain('admin.riskControl.attemptCount')

    const detailButton = wrapper.findAll('button').find((button) => button.text().includes('[redacted evidence]'))
    expect(detailButton).toBeDefined()
    await detailButton!.trigger('click')
    expect(wrapper.get('[data-test="review-attempts"]').text()).toContain('Backup')
    expect(wrapper.get('[data-test="evidence-window"]').text()).toContain('credentials')
    wrapper.unmount()
  })

  it('falls back to the historical score when DeepSeek confidence is null', async () => {
    listLogs.mockResolvedValue({
      items: [{ ...auditLog(), deepseek_confidence: null, highest_score: 0.73 }],
      total: 1,
      page: 1,
      page_size: 20,
      pages: 1,
    })
    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.get('[data-test="review-confidence"]').text()).toBe('73%')
    wrapper.unmount()
  })

  it('saves canonical Layer 1 and Layer 2 keyword lists without legacy aliases', async () => {
    const wrapper = mountView()
    await flushPromises()

    await wrapper.get('[data-test="layer1-keywords"]').setValue('explicit phrase\nexplicit phrase\nsecond phrase')
    await wrapper.get('[data-test="layer2-keywords"]').setValue('candidate\ncontext term')
    await wrapper.get('[data-test="save-risk-control"]').trigger('click')
    await flushPromises()

    const payload = updateConfig.mock.calls.at(-1)?.[0]
    expect(payload).toEqual(
      expect.objectContaining({
        layer1_keywords: ['explicit phrase', 'second phrase'],
        layer2_keywords: ['candidate', 'context term'],
      })
    )
    expect(payload).not.toHaveProperty('hard_block_patterns')
    expect(payload).not.toHaveProperty('candidate_keywords')
    wrapper.unmount()
  })
})
