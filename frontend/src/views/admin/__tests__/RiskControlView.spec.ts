import { beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h } from 'vue'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'

import RiskControlView from '../RiskControlView.vue'
import type {
  ContentModerationConfig,
  ContentModerationLog,
  UpdateContentModerationConfig,
} from '@/api/admin/riskControl'

const {
  getConfig,
  updateConfig,
  getStatus,
  listLogs,
  getLog,
  testAPIAvailability,
  testDeepSeekChannel,
  getGroups,
  showError,
  showSuccess,
} =
  vi.hoisted(() => ({
    getConfig: vi.fn(),
    updateConfig: vi.fn(),
    getStatus: vi.fn(),
    listLogs: vi.fn(),
    getLog: vi.fn(),
    testAPIAvailability: vi.fn(),
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
      getLog,
      testAPIAvailability,
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
  remote_reviewers_enabled: true,
  remote_consensus_required: 2,
  remote_unavailable_policy: 'fail_closed',
  deepseek_total_timeout_ms: 10000,
  deepseek_threshold: 0.8,
  policy_version: 'deepseek-v4-flash-audit-v3',
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
  startup_api_usability_tested: true,
  startup_api_usability_checked_at: '2026-08-17T00:59:00Z',
  startup_api_usability_configured: 2,
  startup_api_usability_succeeded: 2,
  remote_heartbeats: [
    {
      channel_id: 'deepseek-official',
      provider: 'deepseek',
      status: 'reachable',
      checked_at: '2026-08-17T01:00:30Z',
      latency_ms: 15,
      http_status: 404,
    },
    {
      channel_id: 'deepseek-backup',
      provider: 'deepseek',
      status: 'unreachable',
      checked_at: '2026-08-17T01:00:30Z',
      latency_ms: 2800,
      error: 'timeout',
    },
  ],
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
      text: '前文🔐 credentials 后文',
      matches: [{ keyword: 'credentials', rule_id: 'layer2-1', tier: 'layer2', start: 4, end: 15 }],
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
    getLog.mockReset()
    testAPIAvailability.mockReset()
    testDeepSeekChannel.mockReset()
    getGroups.mockReset()
    showError.mockReset()
    showSuccess.mockReset()

    getConfig.mockResolvedValue(baseConfig())
    updateConfig.mockImplementation(async (payload: UpdateContentModerationConfig) => configFromUpdate(payload))
    getStatus.mockResolvedValue(runtimeStatus())
    listLogs.mockResolvedValue({ items: [], total: 0, page: 1, page_size: 20, pages: 1 })
    getLog.mockResolvedValue(auditLog())
    testAPIAvailability.mockResolvedValue({
      channel_id: 'deepseek-official',
      provider: 'deepseek',
      model: 'deepseek-v4-flash',
      test_type: 'api_usability',
      reachable: true,
      health_valid: true,
      latency_ms: 218,
      http_status: 200,
      verdict: 'safe',
      category: 'safe',
      checked_at: '2026-08-17T01:02:00Z',
    })
    getGroups.mockResolvedValue([])
  })

  it('renders DeepSeek as the default risk reviewer', async () => {
    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.get('[data-test="deepseek-enabled"]').attributes('aria-checked')).toBe('true')
    expect(wrapper.find('[data-test="yufeng-enabled"]').exists()).toBe(false)
    expect(wrapper.get('[data-test="deepseek-threshold"]').element).toHaveProperty('value', '80%')
    expect(wrapper.text()).toContain('deepseek-v4-flash-audit-v3')
    expect(wrapper.text()).toContain('admin.riskControl.nonThinking')
    expect(wrapper.find('input[type="file"]').exists()).toBe(false)

    wrapper.unmount()
  })

  it('shows the managed gateway providers and offers their models in the risk scope', async () => {
    const wrapper = mountView()
    await flushPromises()

    const catalog = wrapper.get('[data-test="managed-provider-catalog"]')
    expect(catalog.text()).toContain('mimo-v2.5')
    expect(catalog.text()).toContain('glm-4.7-flashx')
    expect(catalog.text()).toContain('qwen3.7-flash')

    await wrapper.get('[data-test="model-filter-type"]').setValue('include')
    await wrapper.get('[data-test="managed-model-filter-mimo-v2-5"]').trigger('click')

    expect(wrapper.get('[data-test="model-filter-models"]').element).toHaveProperty('value', 'mimo-v2.5')
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
        remote_reviewers_enabled: true,
        remote_consensus_required: 1,
        remote_unavailable_policy: 'fail_closed',
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

  it('persists both review outage policies instead of forcing fail-closed', async () => {
    const wrapper = mountView()
    await flushPromises()

    await wrapper.get('[data-test="remote-unavailable-risk-tiered"]').trigger('click')
    await wrapper.get('[data-test="save-risk-control"]').trigger('click')
    await flushPromises()
    expect(updateConfig).toHaveBeenLastCalledWith(
      expect.objectContaining({ remote_unavailable_policy: 'risk_tiered' })
    )

    await wrapper.get('[data-test="remote-unavailable-fail-closed"]').trigger('click')
    await wrapper.get('[data-test="save-risk-control"]').trigger('click')
    await flushPromises()
    expect(updateConfig).toHaveBeenLastCalledWith(
      expect.objectContaining({ remote_unavailable_policy: 'fail_closed' })
    )

    wrapper.unmount()
  })

  it('keeps the review pool ready but marks strategy restriction confirmation unavailable with one provider', async () => {
    const statusWithoutReadiness = {
      ...runtimeStatus(),
      second_layer_enforce_ready: undefined,
      second_layer_enforce_reason: undefined,
    }
    getStatus.mockResolvedValue(statusWithoutReadiness)

    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.get('[data-test="enforce-health-gate"]').text()).toContain('admin.riskControl.enforceGateReady')
    expect(wrapper.get('[data-test="restriction-consensus-gate"]').text()).toContain(
      'admin.riskControl.restrictionConsensusUnavailable'
    )
    wrapper.unmount()
  })

  it('lets the server perform the authoritative Enforce connectivity check', async () => {
    getStatus.mockResolvedValue(runtimeStatus(false))
    const blockedWrapper = mountView()
    await flushPromises()

    const enforceButton = blockedWrapper.get('[data-test="layer2-stage-enforce"]')
    expect(enforceButton.attributes('disabled')).toBeUndefined()
    expect(blockedWrapper.get('[data-test="enforce-health-gate"]').text()).toContain('connectivity check required')
    await enforceButton.trigger('click')
    await blockedWrapper.get('[data-test="save-risk-control"]').trigger('click')
    await flushPromises()
    expect(updateConfig).toHaveBeenCalledWith(expect.objectContaining({ second_layer_stage: 'enforce' }))
    blockedWrapper.unmount()
  })

  it('runs a real API availability test for a saved channel', async () => {
    testAPIAvailability.mockResolvedValueOnce({
      channel_id: 'deepseek-official',
      provider: 'deepseek',
      model: 'deepseek-v4-flash',
      test_type: 'api_usability',
      reachable: true,
      health_valid: true,
      latency_ms: 218,
      http_status: 200,
      verdict: 'restricted',
      category: 'restricted_security_content',
    })
    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.get('[data-test="test-api-availability-0"]').text()).toContain(
      'admin.riskControl.testAPIAvailability'
    )
    await wrapper.get('[data-test="test-api-availability-0"]').trigger('click')
    await flushPromises()

    expect(testAPIAvailability).toHaveBeenCalledWith('deepseek-official')
    expect(testDeepSeekChannel).not.toHaveBeenCalled()
    expect(wrapper.get('[data-test="deepseek-channel-test-result-0"]').text()).toContain(
      'admin.riskControl.apiTestReachable'
    )
    expect(showSuccess).toHaveBeenCalledWith('admin.riskControl.channelTestComplete')
    wrapper.unmount()
  })

  it('displays the backend heartbeat result without a manual ping action', async () => {
    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.get('[data-test="deepseek-channel-heartbeat-status-0"]').text()).toContain(
      'admin.riskControl.heartbeatReachable'
    )
    const heartbeat = wrapper.get('[data-test="deepseek-channel-heartbeat-0"]')
    expect(heartbeat.text()).toContain('15 ms')
    expect(heartbeat.text()).toContain('HTTP 404')
    expect(wrapper.find('[data-test^="ping-"]').exists()).toBe(false)

    wrapper.unmount()
  })

  it('shows model fields, failover attempts, and reviewer disagreement in audit records', async () => {
    listLogs.mockResolvedValue({ items: [auditLog()], total: 1, page: 1, page_size: 20, pages: 1 })
    const wrapper = mountView()
    await flushPromises()

    expect(listLogs).toHaveBeenCalledWith(expect.objectContaining({ result: 'risky_shadow' }))
    expect(wrapper.text()).toContain('cyber_abuse')
    expect(wrapper.text()).toContain('91%')
    expect(wrapper.text()).toContain('admin.riskControl.result.disagreementUndetermined')
    expect(wrapper.text()).toContain('DeepSeek Official')
    expect(wrapper.text()).toContain('admin.riskControl.attemptCount')

    const detailButton = wrapper.findAll('button').find((button) => button.text().includes('[redacted evidence]'))
    expect(detailButton).toBeDefined()
    await detailButton!.trigger('click')
    expect(wrapper.get('[data-test="review-attempts"]').text()).toContain('Backup')
    expect(wrapper.get('[data-test="evidence-window"]').text()).toContain('credentials')
    expect(wrapper.get('[data-test="evidence-match"]').text()).toBe('credentials')
    wrapper.unmount()
  })

  it('distinguishes confirmed restrictions from undetermined review outcomes', async () => {
    const restrictedAttempts = [
      {
        provider: 'deepseek',
        channel_id: 'deepseek-official',
        outcome: 'success',
        verdict: 'restricted',
        chunk_index: 0,
        chunk_count: 1,
      },
      {
        provider: 'alibaba_qwen',
        channel_id: 'qwen-official',
        outcome: 'success',
        verdict: 'restricted',
        chunk_index: 0,
        chunk_count: 1,
      },
    ]
    const records = [
      {
        ...auditLog(),
        id: 101,
        action: 'restricted_block',
        review_outcome: 'policy_restricted',
        reviewer_disagreement: false,
        evidence_truncated: false,
        consensus_status: 'confirmed_restricted',
        remote_votes: 2,
        review_attempts: restrictedAttempts,
        input_excerpt: 'confirmed restriction',
      },
      {
        ...auditLog(),
        id: 102,
        action: 'restricted_block',
        review_outcome: 'disagreement_restricted',
        reviewer_disagreement: true,
        evidence_truncated: false,
        consensus_status: 'disagreement_restricted',
        remote_votes: 2,
        input_excerpt: 'review disagreement',
      },
      {
        ...auditLog(),
        id: 103,
        action: 'review_unavailable',
        review_outcome: 'unavailable',
        reviewer_disagreement: false,
        evidence_truncated: false,
        consensus_status: 'consensus_unavailable',
        remote_votes: 1,
        input_excerpt: 'review unavailable',
      },
      {
        ...auditLog(),
        id: 104,
        action: 'restricted_block',
        review_outcome: 'policy_restricted',
        reviewer_disagreement: false,
        evidence_truncated: true,
        consensus_status: 'confirmed_restricted',
        remote_votes: 2,
        review_attempts: restrictedAttempts,
        input_excerpt: 'truncated evidence',
      },
      {
        ...auditLog(),
        id: 105,
        action: 'restricted_block',
        review_outcome: 'policy_restricted',
        reviewer_disagreement: false,
        evidence_truncated: false,
        consensus_status: 'single_restricted',
        remote_votes: 1,
        review_attempts: [restrictedAttempts[0]],
        input_excerpt: 'single vote',
      },
      {
        ...auditLog(),
        id: 106,
        action: 'restricted_block',
        review_outcome: 'policy_restricted',
        reviewer_disagreement: false,
        evidence_truncated: false,
        consensus_status: undefined,
        remote_votes: undefined,
        review_attempts: [],
        input_excerpt: 'legacy restriction',
      },
      {
        ...auditLog(),
        id: 107,
        action: 'evidence_capacity_exceeded',
        review_outcome: 'evidence_capacity_exceeded',
        reviewer_disagreement: false,
        evidence_truncated: true,
        consensus_status: 'consensus_unavailable',
        remote_votes: 1,
        input_excerpt: 'capacity exceeded',
      },
    ] satisfies ContentModerationLog[]
    listLogs.mockResolvedValue({ items: records, total: records.length, page: 1, page_size: 20, pages: 1 })

    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.findAll('[data-test="audit-decision-label"]').map((label) => label.text())).toEqual([
      'admin.riskControl.result.restrictedConfirmed',
      'admin.riskControl.result.disagreementUndetermined',
      'admin.riskControl.result.unavailableUndetermined',
      'admin.riskControl.result.evidenceTruncatedUndetermined',
      'admin.riskControl.result.singleVoteUndetermined',
      'admin.riskControl.result.restrictedUnconfirmed',
      'admin.riskControl.result.evidenceCapacityUndetermined',
    ])
    expect(wrapper.findAll('[data-test="audit-decision-meta"]').map((meta) => meta.text())).toEqual([
      'admin.riskControl.confirmedVoteCount',
      'admin.riskControl.undeterminedNonViolation',
      'admin.riskControl.undeterminedNonViolation',
      'admin.riskControl.undeterminedNonViolation',
      'admin.riskControl.singleVoteDetail',
      'admin.riskControl.consensusUnknownDetail',
      'admin.riskControl.undeterminedNonViolation',
    ])

    const detailButton = wrapper.findAll('button').find((button) => button.text().includes('confirmed restriction'))
    expect(detailButton).toBeDefined()
    await detailButton!.trigger('click')
    expect(wrapper.get('[data-test="audit-log-detail"]').text()).toContain('admin.riskControl.reviewConsensus')
    expect(wrapper.get('[data-test="audit-log-detail"]').text()).toContain('admin.riskControl.confirmedVoteCount')
    wrapper.unmount()
  })

  it('derives disagreement only when independent safe and restricted votes refer to the same chunk', async () => {
    listLogs.mockResolvedValue({
      items: [
        {
          ...auditLog(),
          id: 108,
          action: 'restricted_block',
          review_outcome: 'policy_restricted',
          reviewer_disagreement: false,
          evidence_truncated: false,
          review_attempts: [
            { provider: 'deepseek', outcome: 'success', verdict: 'safe', chunk_index: 0, chunk_count: 2 },
            { provider: 'alibaba_qwen', outcome: 'success', verdict: 'restricted', chunk_index: 1, chunk_count: 2 },
          ],
        },
        {
          ...auditLog(),
          id: 109,
          action: 'restricted_block',
          review_outcome: 'policy_restricted',
          reviewer_disagreement: false,
          evidence_truncated: false,
          review_attempts: [
            { provider: 'deepseek', outcome: 'success', verdict: 'safe', chunk_index: 0, chunk_count: 1 },
            { provider: 'alibaba_qwen', outcome: 'success', verdict: 'restricted', chunk_index: 0, chunk_count: 1 },
          ],
        },
      ],
      total: 2,
      page: 1,
      page_size: 20,
      pages: 1,
    })

    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.findAll('[data-test="audit-decision-label"]').map((label) => label.text())).toEqual([
      'admin.riskControl.result.singleVoteUndetermined',
      'admin.riskControl.result.disagreementUndetermined',
    ])
    wrapper.unmount()
  })

  it('links a cache replay to the original evidence and keeps keyword highlighting', async () => {
    const replay = {
      ...auditLog(),
      id: 20,
      request_id: 'req-20',
      action: 'cache_block',
      cache_hit: true,
      decision_source: 'cache_replay',
      source_log_id: 19,
      input_excerpt: '[cached evidence]',
      evidence_windows: [
        {
          path: 'messages[0].content',
          context_class: 'user',
          text: '缓存 credentials 命中',
          matches: [{ keyword: 'credentials', rule_id: 'layer2-1', tier: 'layer2', start: 3, end: 14 }],
        },
      ],
    } satisfies ContentModerationLog
    listLogs.mockResolvedValue({ items: [replay], total: 1, page: 1, page_size: 20, pages: 1 })

    const wrapper = mountView()
    await flushPromises()
    const detailButton = wrapper.findAll('button').find((button) => button.text().includes('[cached evidence]'))
    expect(detailButton).toBeDefined()
    await detailButton!.trigger('click')

    expect(wrapper.get('[data-test="replay-source"]').text()).toContain('#19')
    expect(wrapper.get('[data-test="evidence-match"]').text()).toBe('credentials')

    await wrapper.get('[data-test="open-replay-source"]').trigger('click')
    await flushPromises()

    expect(getLog).toHaveBeenCalledWith(19)
    expect(wrapper.get('[data-test="replay-source"]').text()).toContain('#19')
    expect(wrapper.get('[data-test="evidence-match"]').text()).toBe('credentials')
    expect(wrapper.get('[data-test="evidence-text"]').text()).toContain('前文🔐 credentials 后文')
    wrapper.unmount()
  })

  it('loads policy-restricted non-violation records as a separate view', async () => {
    const wrapper = mountView()
    await flushPromises()

    const restrictedTab = wrapper.get('[data-test="record-tab-restricted"]')
    expect(restrictedTab.text()).toContain('admin.riskControl.recordTabs.restricted')
    await restrictedTab.trigger('click')
    await flushPromises()

    expect(listLogs).toHaveBeenLastCalledWith(expect.objectContaining({ result: 'restricted' }))
    wrapper.unmount()
  })

  it('loads violation blocks without exposing the combined content-blocked view', async () => {
    const wrapper = mountView()
    await flushPromises()

    const violationTab = wrapper.get('[data-test="record-tab-violation_blocked"]')
    expect(violationTab.text()).toContain('admin.riskControl.recordTabs.violationBlocked')
    expect(wrapper.find('[data-test="record-tab-content_blocked"]').exists()).toBe(false)
    await violationTab.trigger('click')
    await flushPromises()

    expect(listLogs).toHaveBeenLastCalledWith(expect.objectContaining({ result: 'violation_blocked' }))
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
