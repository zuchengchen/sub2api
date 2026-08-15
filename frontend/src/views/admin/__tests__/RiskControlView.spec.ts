import { beforeEach, describe, expect, it, vi } from 'vitest'
import { defineComponent, h } from 'vue'
import { flushPromises, mount } from '@vue/test-utils'
import type { DOMWrapper, VueWrapper } from '@vue/test-utils'

import RiskControlView from '../RiskControlView.vue'
import type { ContentModerationConfig, ContentModerationLog, UpdateContentModerationConfig } from '@/api/admin/riskControl'

const {
  getConfig,
  updateConfig,
  getStatus,
  listLogs,
  previewArchive,
  downloadArchive,
  deleteArchive,
  getGroups,
  getProxies,
  showError,
  showSuccess,
} = vi.hoisted(() => ({
  getConfig: vi.fn(),
  updateConfig: vi.fn(),
  getStatus: vi.fn(),
  listLogs: vi.fn(),
  previewArchive: vi.fn(),
  downloadArchive: vi.fn(),
  deleteArchive: vi.fn(),
  getGroups: vi.fn(),
  getProxies: vi.fn(),
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
      previewArchive,
      downloadArchive,
      deleteArchive,
      testAPIKeys: vi.fn(),
      deleteFlaggedHash: vi.fn(),
      clearFlaggedHashes: vi.fn(),
      unbanUser: vi.fn(),
    },
    groups: {
      getAll: getGroups,
    },
    proxies: {
      getAll: getProxies,
    },
  },
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({
    showError,
    showSuccess,
  }),
}))

vi.mock('@/utils/apiError', () => ({
  extractApiErrorMessage: (_err: unknown, fallback: string) => fallback,
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, string | number>) => {
        if (key === 'admin.riskControl.preBlockAPIKeyLoadSummary') {
          return `同步并发 ${params?.active} / 可用 Key ${params?.available}，累计 ${params?.total} 次`
        }
        return key.replace(/\{(\w+)\}/g, (_, token) => String(params?.[token] ?? `{${token}}`))
      },
    }),
  }
})

const baseConfig = (): ContentModerationConfig => ({
  enabled: true,
  mode: 'pre_block',
  base_url: 'https://api.openai.com',
  model: 'omni-moderation-latest',
  proxy_id: null,
  api_key_configured: false,
  api_key_masked: '',
  api_key_count: 0,
  api_key_masks: [],
  api_key_statuses: [],
  timeout_ms: 3000,
  sample_rate: 100,
  all_groups: true,
  group_ids: [],
  user_email_whitelist: [],
  record_non_hits: false,
  block_status: 403,
  block_message: '内容审计命中风险规则，请调整输入后重试',
  email_on_hit: true,
  auto_ban_enabled: true,
  ban_threshold: 10,
  violation_window_hours: 720,
  retry_count: 2,
  hit_retention_days: 180,
  non_hit_retention_days: 3,
  pre_hash_check_enabled: false,
  blocked_keywords: [],
  keyword_blocking_mode: 'keyword_and_api',
  thresholds: {
    harassment: 0.98,
    sexual: 0.65,
  },
  model_filter: {
    type: 'all',
    models: [],
  },
  cache_version: 'v1',
  cache_max_entries: 100000,
  cache_max_bytes: 67108864,
  fragment_block_ttl_seconds: 600,
  fragment_allow_ttl_seconds: 3600,
  fragment_ttl_policy_version: 'ttl-v1',
  second_layer_enabled: false,
  second_layer_stage: 'enforce',
  second_layer_endpoints: [],
  second_layer_scanners: [],
  hard_block_patterns: [],
  candidate_keywords: [],
  keyword_allowlist: [],
  keyword_policy_version: 'keyword-v2',
  context_policy_version: 'context-v1',
  evidence_policy_version: 'evidence-v1',
  candidate_asset: 'legacy-prompt-audit-v1',
  candidate_enabled: false,
  candidate_layer1_count: 972,
  candidate_layer2_count: 246,
  candidate_source_commit: '99c8e4bf7564823bafbab369acab6539e734c1bb',
  candidate_endpoints: [],
  cyber_policy_exclude_from_ban_count: false,
})

const runtimeStatus = () => ({
  enabled: true,
  risk_control_enabled: true,
  mode: 'pre_block',
  pre_block_active: 0,
  pre_block_checked: 0,
  pre_block_allowed: 0,
  pre_block_blocked: 0,
  pre_block_errors: 0,
  pre_block_avg_latency_ms: 0,
  pre_block_api_key_active: 0,
  pre_block_api_key_available_count: 0,
  pre_block_api_key_total_calls: 0,
  pre_block_api_key_loads: [],
  api_key_statuses: [],
  flagged_hash_count: 0,
  last_cleanup_deleted_hit: 0,
  last_cleanup_deleted_non_hit: 0,
  pending_body_bytes: 0,
  pending_body_max_seen: 0,
  pending_body_budget_bytes: 1073741824,
  pending_body_rejections: 0,
  observed_request_body_max: 0,
  request_body_histogram: [],
  fragment_cache_hits: 0,
  fragment_cache_misses: 0,
  fragment_cache_expired: 0,
  fragment_cache_replays: 0,
  fragment_cache_errors: 0,
  fragment_cache_writes: 0,
  fragment_cache_write_errors: 0,
  second_layer_metrics: [],
  archive_runtime: {
    degraded: false,
    retry_queue_depth: 0,
    emergency_queue_depth: 0,
    archive_retry_attempts: 0,
    archive_retry_errors: 0,
    content_lost: 0,
    disk_free_bytes: 10737418240,
    disposition_queue_depth: 0,
    disposition_retry_attempts: 0,
    disposition_retry_errors: 0,
    lost_summary_queue_depth: 0,
  },
})

const archivedLog = (): ContentModerationLog => ({
  id: 41,
  request_id: 'req-41',
  user_id: 9,
  user_email: 'user@example.com',
  api_key_id: 7,
  api_key_name: 'test-key',
  group_id: 3,
  group_name: 'GPT Production',
  endpoint: '/v1/responses',
  provider: 'openai',
  model: 'gpt-5.5',
  mode: 'pre_block',
  action: 'keyword_block',
  cache_hit: false,
  decision_source: 'keyword_high_confidence',
  evidence_truncated: false,
  flagged: true,
  highest_category: 'content_policy',
  highest_score: 1,
  matched_keyword: 'blocked term',
  category_scores: { content_policy: 1 },
  threshold_snapshot: {},
  input_excerpt: '[redacted summary]',
  upstream_latency_ms: null,
  error: '',
  violation_count: 1,
  auto_banned: false,
  email_sent: false,
  user_status: 'active',
  queue_delay_ms: null,
  protocol: 'openai_responses',
  transport: 'http',
  request_stage: 'http',
  request_target: '/v1/responses?stream=true',
  input_hash: 'a'.repeat(64),
  archive_id: 'archive-41',
  archive_version: 1,
  archive_key_id: 'key-current',
  archive_bytes: 2048,
  archive_status: 'available',
  archive_incomplete: false,
  archive_content_lost: false,
  disposition_status: 'not_required',
  disposition_target: '',
  disposition_transitioned: false,
  created_at: '2026-08-13T00:00:00Z',
})

const AppLayoutStub = { template: '<div><slot /></div>' }
const BaseDialogStub = defineComponent({
  props: {
    show: {
      type: Boolean,
      default: false,
    },
  },
  template: '<div v-if="show"><slot /><slot name="footer" /></div>',
})
const ModelWhitelistSelectorStub = defineComponent({
  props: {
    modelValue: {
      type: Array,
      default: () => [],
    },
  },
  emits: ['update:modelValue'],
  setup(props, { emit }) {
    const onInput = (event: Event) => {
      const value = (event.target as HTMLInputElement).value
      emit(
        'update:modelValue',
        value
          .split(/[,\n]/)
          .map((item) => item.trim())
          .filter(Boolean)
      )
    }
    return () =>
      h('input', {
        'data-test': 'model-filter-input',
        value: (props.modelValue as string[]).join('\n'),
        onInput,
      })
  },
})

function findButtonByText(wrapper: VueWrapper, text: string): DOMWrapper<HTMLButtonElement> {
  const button = wrapper.findAll<HTMLButtonElement>('button').find((item) => item.text().includes(text))
  if (!button) {
    throw new Error(`button not found: ${text}`)
  }
  return button
}

describe('admin RiskControlView', () => {
  beforeEach(() => {
    getConfig.mockReset()
    updateConfig.mockReset()
    getStatus.mockReset()
    listLogs.mockReset()
    previewArchive.mockReset()
    downloadArchive.mockReset()
    deleteArchive.mockReset()
    getGroups.mockReset()
    showError.mockReset()
    showSuccess.mockReset()

    getConfig.mockResolvedValue(baseConfig())
    getStatus.mockResolvedValue(runtimeStatus())
    listLogs.mockResolvedValue({ items: [], total: 0, page: 1, page_size: 20, pages: 1 })
    getGroups.mockResolvedValue([])
    getProxies.mockResolvedValue([])
    updateConfig.mockImplementation(async (payload: UpdateContentModerationConfig) => ({
      ...baseConfig(),
      ...payload,
      model_filter: payload.model_filter ?? baseConfig().model_filter,
      api_key_configured: false,
      api_key_masked: '',
      api_key_count: 0,
      api_key_masks: [],
      api_key_statuses: [],
    }))
    previewArchive.mockResolvedValue({
      content: '{"archive_id":"archive-41"}',
      returned_bytes: 27,
      total_bytes: 2097152,
      truncated: true,
    })
    downloadArchive.mockResolvedValue(new Blob(['{"archive_id":"archive-41"}'], { type: 'application/json' }))
    deleteArchive.mockResolvedValue({ deleted: true })
  })

  it('requests cyber policy records by default and switches between the three audit views', async () => {
    listLogs.mockResolvedValue({
      items: [{ ...archivedLog(), action: 'hash_block', flagged: false }],
      total: 1,
      page: 1,
      page_size: 20,
      pages: 1,
    })
    const wrapper = mount(RiskControlView, {
      global: {
        stubs: {
          AppLayout: AppLayoutStub,
          BaseDialog: BaseDialogStub,
          Icon: true,
          Select: true,
          Toggle: true,
          Pagination: true,
          ModelWhitelistSelector: ModelWhitelistSelectorStub,
          ProxySelector: true,
        },
      },
    })

    await flushPromises()

    expect(listLogs).toHaveBeenCalledWith(expect.objectContaining({ result: 'cyber_policy' }))

    const blockedTab = wrapper.get('[data-test="record-tab-content_blocked"]')
    await blockedTab.trigger('click')
    await flushPromises()
    expect(listLogs).toHaveBeenLastCalledWith(expect.objectContaining({ result: 'content_blocked' }))
    expect(blockedTab.attributes('aria-selected')).toBe('true')

    const shadowTab = wrapper.get('[data-test="record-tab-risky_shadow"]')
    await shadowTab.trigger('click')
    await flushPromises()
    expect(listLogs).toHaveBeenLastCalledWith(expect.objectContaining({ result: 'risky_shadow' }))
    expect(shadowTab.attributes('aria-selected')).toBe('true')
    expect(wrapper.get('[data-test="audit-result"]').text()).toBe('admin.riskControl.action.block')
    expect(wrapper.get('[data-test="audit-result"]').classes()).toContain('bg-red-100')
    expect(wrapper.text()).not.toContain('admin.riskControl.result.pass')
  })

  it('renders cache replay rows as non-counting retries linked to the original decision', async () => {
    const source = { ...archivedLog(), request_id: 'req-original' }
    const replay: ContentModerationLog = {
      ...archivedLog(),
      id: 42,
      request_id: 'req-replay',
      action: 'cache_block',
      cache_hit: true,
      decision_source: 'cache_replay',
      source_log_id: 41,
      violation_count: 0,
      email_sent: false,
      archive_id: undefined,
      archive_status: 'none',
      input_excerpt: '[replay summary]',
    }
    listLogs.mockResolvedValue({ items: [replay, source], total: 2, page: 1, page_size: 20, pages: 1 })
    const wrapper = mount(RiskControlView, {
      global: {
        stubs: {
          AppLayout: AppLayoutStub,
          BaseDialog: BaseDialogStub,
          Icon: true,
          Select: true,
          Toggle: true,
          Pagination: true,
          ModelWhitelistSelector: ModelWhitelistSelectorStub,
          ProxySelector: true,
        },
      },
    })

    await flushPromises()

    const replayResult = wrapper.findAll('[data-test="audit-result"]')[0]
    expect(replayResult.text()).toBe('admin.riskControl.action.cacheReplay')
    expect(replayResult.classes()).toContain('bg-sky-50')
    expect(wrapper.text()).toContain('admin.riskControl.replayNotCounted')
    expect(wrapper.text()).toContain('admin.riskControl.replayNoSideEffects')

    await findButtonByText(wrapper, 'admin.riskControl.replaySource').trigger('click')
    expect(wrapper.text()).toContain('req-original')
  })

  it('renders risky shadow decisions as observations instead of passes', async () => {
    listLogs.mockResolvedValue({
      items: [{
        ...archivedLog(),
        action: 'second_layer_shadow',
        decision_source: 'model_shadow',
        flagged: false,
        highest_category: 'jailbreak',
        violation_count: 0,
      }],
      total: 1,
      page: 1,
      page_size: 20,
      pages: 1,
    })
    const wrapper = mount(RiskControlView, {
      global: {
        stubs: {
          AppLayout: AppLayoutStub,
          BaseDialog: BaseDialogStub,
          Icon: true,
          Select: true,
          Toggle: true,
          Pagination: true,
          ModelWhitelistSelector: ModelWhitelistSelectorStub,
          ProxySelector: true,
        },
      },
    })

    await flushPromises()
    await wrapper.get('[data-test="record-tab-risky_shadow"]').trigger('click')
    await flushPromises()

    const result = wrapper.get('[data-test="audit-result"]')
    expect(result.text()).toBe('admin.riskControl.action.shadowBlock')
    expect(result.classes()).toContain('bg-amber-100')
    expect(result.text()).not.toBe('admin.riskControl.result.pass')
  })

  it('saves staged YuFeng endpoint, bounded TTLs, and policy versions', async () => {
    getConfig.mockResolvedValue({ ...baseConfig(), second_layer_enabled: true })
    const wrapper = mount(RiskControlView, {
      global: {
        stubs: {
          AppLayout: AppLayoutStub,
          BaseDialog: BaseDialogStub,
          Icon: true,
          Select: true,
          Toggle: true,
          Pagination: true,
          ModelWhitelistSelector: ModelWhitelistSelectorStub,
          ProxySelector: true,
        },
      },
    })

    await flushPromises()
    await findButtonByText(wrapper, 'admin.riskControl.openSettings').trigger('click')
    await findButtonByText(wrapper, 'admin.riskControl.tabs.secondLayer').trigger('click')
    await wrapper.get('[data-test="second-layer-stage-shadow"]').trigger('click')
    await wrapper.get('[data-test="fragment-block-ttl"]').setValue('120')
    await wrapper.get('[data-test="fragment-allow-ttl"]').setValue('90000')
    await wrapper.get('[data-test="add-second-layer-endpoint"]').trigger('click')
    await wrapper.get('[data-test="second-layer-model-revision-0"]').setValue('c9766937')
    await wrapper.get('[data-test="second-layer-prompt-version-0"]').setValue('yufeng-xguard-v2')
    await findButtonByText(wrapper, 'admin.riskControl.saveConfig').trigger('click')
    await flushPromises()

    expect(updateConfig).toHaveBeenCalledWith(expect.objectContaining({
      fragment_block_ttl_seconds: 300,
      fragment_allow_ttl_seconds: 86400,
      fragment_ttl_policy_version: 'ttl-v1',
      second_layer_enabled: true,
      second_layer_stage: 'shadow',
      keyword_policy_version: 'keyword-v2',
      context_policy_version: 'context-v1',
      evidence_policy_version: 'evidence-v1',
      second_layer_endpoints: [expect.objectContaining({
        profile: 'yufeng_xguard',
        model_revision: 'c9766937',
        prompt_version: 'yufeng-xguard-v2',
      })],
    }))
  })

  it('saves the selected model filter mode and models', async () => {
    const wrapper = mount(RiskControlView, {
      global: {
        stubs: {
          AppLayout: AppLayoutStub,
          BaseDialog: BaseDialogStub,
          Icon: true,
          Select: true,
          Toggle: true,
          Pagination: true,
          ModelWhitelistSelector: ModelWhitelistSelectorStub,
          ProxySelector: true,
        },
      },
    })

    await flushPromises()

    await findButtonByText(wrapper, 'admin.riskControl.openSettings').trigger('click')
    await findButtonByText(wrapper, 'admin.riskControl.tabs.scope').trigger('click')
    await findButtonByText(wrapper, 'admin.riskControl.modelFilterInclude').trigger('click')
    await wrapper.get('[data-test="model-filter-input"]').setValue('gpt-5.5, gpt-5.4')
    await findButtonByText(wrapper, 'admin.riskControl.saveConfig').trigger('click')
    await flushPromises()

    expect(updateConfig).toHaveBeenCalledWith(expect.objectContaining({
      model_filter: {
        type: 'include',
        models: ['gpt-5.5', 'gpt-5.4'],
      },
    }))
    expect(showError).not.toHaveBeenCalled()
  })

  it('loads and saves the user email whitelist from the scope settings', async () => {
    getConfig.mockResolvedValue({
      ...baseConfig(),
      user_email_whitelist: ['existing@example.com'],
    })
    const wrapper = mount(RiskControlView, {
      global: {
        stubs: {
          AppLayout: AppLayoutStub,
          BaseDialog: BaseDialogStub,
          Icon: true,
          Select: true,
          Toggle: true,
          Pagination: true,
          ModelWhitelistSelector: ModelWhitelistSelectorStub,
          ProxySelector: true,
        },
      },
    })

    await flushPromises()
    await findButtonByText(wrapper, 'admin.riskControl.openSettings').trigger('click')
    await findButtonByText(wrapper, 'admin.riskControl.tabs.scope').trigger('click')
    const whitelist = wrapper.get('[data-test="user-email-whitelist"]')
    expect((whitelist.element as HTMLTextAreaElement).value).toBe('existing@example.com')

    await whitelist.setValue('Allowed@Example.COM\nallowed@example.com\nsecond@example.net')
    await findButtonByText(wrapper, 'admin.riskControl.saveConfig').trigger('click')
    await flushPromises()

    expect(updateConfig).toHaveBeenCalledWith(expect.objectContaining({
      user_email_whitelist: ['allowed@example.com', 'second@example.net'],
    }))
    expect(showError).not.toHaveBeenCalled()
  })

  it('submits edited risk control thresholds when saving moderation config', async () => {
    const wrapper = mount(RiskControlView, {
      global: {
        stubs: {
          AppLayout: AppLayoutStub,
          BaseDialog: BaseDialogStub,
          Icon: true,
          Select: true,
          Toggle: true,
          Pagination: true,
          ModelWhitelistSelector: ModelWhitelistSelectorStub,
          ProxySelector: true,
        },
      },
    })

    await flushPromises()

    await findButtonByText(wrapper, 'admin.riskControl.openSettings').trigger('click')
    await findButtonByText(wrapper, 'admin.riskControl.tabs.riskThresholds').trigger('click')
    await wrapper.get('[data-test="risk-threshold-sexual"]').setValue('72')
    await wrapper.get('[data-test="risk-threshold-harassment"]').setValue('99')
    await findButtonByText(wrapper, 'admin.riskControl.saveConfig').trigger('click')
    await flushPromises()

    expect(updateConfig).toHaveBeenCalledWith(expect.objectContaining({
      thresholds: expect.objectContaining({
        sexual: 0.72,
        harassment: 0.99,
      }),
    }))
    expect(showError).not.toHaveBeenCalled()
  })

  it('shows pre-block synchronous moderation metrics', async () => {
    getStatus.mockResolvedValue({
      ...runtimeStatus(),
      pre_block_active: 2,
      pre_block_checked: 128,
      pre_block_allowed: 120,
      pre_block_blocked: 8,
      pre_block_errors: 1,
      pre_block_avg_latency_ms: 86,
      pre_block_api_key_active: 2,
      pre_block_api_key_available_count: 2,
      pre_block_api_key_total_calls: 128,
      pre_block_api_key_loads: [
        {
          index: 0,
          key_hash: 'hash-one',
          masked: 'sk-...one',
          status: 'ok',
          active: 1,
          total: 72,
          success: 70,
          errors: 2,
          avg_latency_ms: 84,
          last_latency_ms: 80,
          last_http_status: 200,
        },
        {
          index: 1,
          key_hash: 'hash-two',
          masked: 'sk-...two',
          status: 'ok',
          active: 1,
          total: 56,
          success: 56,
          errors: 0,
          avg_latency_ms: 90,
          last_latency_ms: 92,
          last_http_status: 200,
        },
      ],
    })

    const wrapper = mount(RiskControlView, {
      global: {
        stubs: {
          AppLayout: AppLayoutStub,
          BaseDialog: BaseDialogStub,
          Icon: true,
          Select: true,
          Toggle: true,
          Pagination: true,
          ModelWhitelistSelector: ModelWhitelistSelectorStub,
          ProxySelector: true,
        },
      },
    })

    await flushPromises()

    expect(wrapper.text()).toContain('admin.riskControl.preBlockSyncStatus')
    expect(wrapper.text()).toContain('admin.riskControl.preBlockSyncHint')
    expect(wrapper.text()).toContain('admin.riskControl.records')
    expect(wrapper.text()).toContain('128')
    expect(wrapper.text()).toContain('120')
    expect(wrapper.text()).toContain('8')
    expect(wrapper.text()).toContain('86 ms')
    expect(wrapper.text()).toContain('admin.riskControl.preBlockAPIKeyLoad')
    expect(wrapper.text()).toContain('sk-...one')
    expect(wrapper.text()).toContain('sk-...two')
    expect(wrapper.text()).toContain('72')
    expect(wrapper.text()).toContain('56')
    expect(wrapper.text()).toContain('同步并发 2 / 可用 Key 2，累计 128 次')

    const runtimeCards = wrapper.get('[data-test="pre-block-runtime-cards"]')
    const syncCard = wrapper.get('[data-test="pre-block-sync-card"]')
    const apiKeyLoadCard = wrapper.get('[data-test="pre-block-api-key-load-card"]')

    expect(runtimeCards.classes()).toEqual(expect.arrayContaining([
      'grid',
      'grid-cols-1',
      'xl:grid-cols-[minmax(0,520px)_minmax(0,1fr)]',
    ]))
    expect(syncCard.element.parentElement).toBe(runtimeCards.element)
    expect(apiKeyLoadCard.element.parentElement).toBe(runtimeCards.element)
    expect(syncCard.classes()).toContain('card')
    expect(apiKeyLoadCard.classes()).toContain('card')
    expect(syncCard.get('h2').text()).toBe('admin.riskControl.preBlockSyncStatus')
    expect(syncCard.text()).toContain('admin.riskControl.preBlockSyncHint')
    expect(apiKeyLoadCard.get('h2').text()).toBe('admin.riskControl.preBlockAPIKeyLoad')
    expect(apiKeyLoadCard.text()).toContain('admin.riskControl.preBlockAPIKeyLoadHint')
    expect(wrapper.get('[data-test="pre-block-api-key-load-list"]').classes()).toEqual(expect.arrayContaining([
      'max-h-[280px]',
      'overflow-y-auto',
    ]))
  })

  it('keeps archive content out of list and summary requests until explicit preview', async () => {
    listLogs.mockResolvedValue({ items: [archivedLog()], total: 1, page: 1, page_size: 20, pages: 1 })
    const wrapper = mount(RiskControlView, {
      global: {
        stubs: {
          AppLayout: AppLayoutStub,
          BaseDialog: BaseDialogStub,
          Icon: true,
          Select: true,
          Toggle: true,
          Pagination: true,
          ModelWhitelistSelector: ModelWhitelistSelectorStub,
          ProxySelector: true,
        },
      },
    })
    await flushPromises()

    expect(previewArchive).not.toHaveBeenCalled()
    await findButtonByText(wrapper, '[redacted summary]').trigger('click')
    expect(wrapper.get('[data-test="archive-section"]').exists()).toBe(true)
    expect(wrapper.text()).not.toContain('archive-41')
    expect(previewArchive).not.toHaveBeenCalled()

    await wrapper.get('[data-test="preview-archive"]').trigger('click')
    await flushPromises()

    expect(previewArchive).toHaveBeenCalledOnce()
    expect(previewArchive).toHaveBeenCalledWith(41)
    expect(wrapper.get('[data-test="archive-preview-result"]').text()).toContain('archive-41')
    expect(wrapper.get('[data-test="archive-preview-truncated"]').text()).toContain('admin.riskControl.archivePreviewTruncated')
  })

  it('downloads the full archive through the dedicated blob endpoint', async () => {
    listLogs.mockResolvedValue({ items: [archivedLog()], total: 1, page: 1, page_size: 20, pages: 1 })
    const createObjectURL = vi.fn(() => 'blob:risk-archive')
    const revokeObjectURL = vi.fn()
    Object.defineProperty(window.URL, 'createObjectURL', { configurable: true, value: createObjectURL })
    Object.defineProperty(window.URL, 'revokeObjectURL', { configurable: true, value: revokeObjectURL })
    const click = vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {})
    const wrapper = mount(RiskControlView, {
      global: {
        stubs: {
          AppLayout: AppLayoutStub,
          BaseDialog: BaseDialogStub,
          Icon: true,
          Select: true,
          Toggle: true,
          Pagination: true,
          ModelWhitelistSelector: ModelWhitelistSelectorStub,
          ProxySelector: true,
        },
      },
    })
    await flushPromises()
    await findButtonByText(wrapper, '[redacted summary]').trigger('click')
    await wrapper.get('[data-test="download-archive"]').trigger('click')
    await flushPromises()

    expect(downloadArchive).toHaveBeenCalledWith(41)
    expect(createObjectURL).toHaveBeenCalledOnce()
    expect(click).toHaveBeenCalledOnce()
    expect(revokeObjectURL).toHaveBeenCalledWith('blob:risk-archive')

    click.mockRestore()
    Reflect.deleteProperty(window.URL, 'revokeObjectURL')
    Reflect.deleteProperty(window.URL, 'createObjectURL')
  })

  it('deletes only archive content and retains the audit row summary', async () => {
    listLogs.mockResolvedValue({ items: [archivedLog()], total: 1, page: 1, page_size: 20, pages: 1 })
    const confirm = vi.spyOn(window, 'confirm').mockReturnValue(true)
    const wrapper = mount(RiskControlView, {
      global: {
        stubs: {
          AppLayout: AppLayoutStub,
          BaseDialog: BaseDialogStub,
          Icon: true,
          Select: true,
          Toggle: true,
          Pagination: true,
          ModelWhitelistSelector: ModelWhitelistSelectorStub,
          ProxySelector: true,
        },
      },
    })
    await flushPromises()
    await findButtonByText(wrapper, '[redacted summary]').trigger('click')
    await wrapper.get('[data-test="delete-archive"]').trigger('click')
    await flushPromises()

    expect(deleteArchive).toHaveBeenCalledWith(41)
    expect(wrapper.text()).toContain('[redacted summary]')
    expect(wrapper.text()).toContain('admin.riskControl.archiveStatus.deleted')
    expect(wrapper.find('[data-test="preview-archive"]').exists()).toBe(false)
    expect(showSuccess).toHaveBeenCalledWith('admin.riskControl.archiveDeleted')
    confirm.mockRestore()
  })
})
