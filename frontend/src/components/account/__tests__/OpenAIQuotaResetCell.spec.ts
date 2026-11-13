import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount } from '@vue/test-utils'
import OpenAIQuotaResetCell from '../OpenAIQuotaResetCell.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import type { Account } from '@/types'
import {
  refreshOpenAIQuota,
  resetOpenAIQuota,
  setOpenAICreditsEnabled,
  type OpenAIQuotaRefreshResult,
} from '@/api/admin/accounts'

vi.mock('@/api/admin/accounts', () => ({
  refreshOpenAIQuota: vi.fn(),
  resetOpenAIQuota: vi.fn(),
  setOpenAICreditsEnabled: vi.fn(),
  setOpenAIAutoResetCreditEnabled: vi.fn(),
  refreshOpenAIReferrals: vi.fn(),
  sendOpenAIReferralInvite: vi.fn(),
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return {
    ...actual,
    useI18n: () => ({
      t: (key: string, params?: Record<string, unknown>) =>
        params?.time ? `${key}:${params.time}` : params?.count ? `${key}:${params.count}` : key,
    }),
  }
})

// 缓存水合会丢弃已过期的重置卡，因此缓存类用例必须使用未来时间。
const FUTURE_EXPIRY_EARLY = '2099-07-03T04:05:06Z'
const FUTURE_EXPIRY_LATE = '2099-07-05T04:05:06Z'
const PAST_EXPIRY = '2020-07-03T04:05:06Z'

function makeAccount(overrides: Partial<Account>): Account {
  return {
    id: 1,
    name: 'acc',
    platform: 'openai',
    type: 'oauth',
    proxy_id: null,
    concurrency: 3,
    priority: 50,
    status: 'active',
    error_message: null,
    last_used_at: null,
    expires_at: null,
    auto_pause_on_expired: false,
    created_at: '2026-01-01T00:00:00Z',
    updated_at: '2026-01-01T00:00:00Z',
    schedulable: true,
    rate_limited_at: null,
    rate_limit_reset_at: null,
    overload_until: null,
    temp_unschedulable_until: null,
    temp_unschedulable_reason: null,
    session_window_start: null,
    session_window_end: null,
    session_window_status: null,
    ...overrides,
  }
}

const resetButton = (wrapper: ReturnType<typeof mount>) =>
  wrapper.get('[data-testid="openai-quota-reset"]')

beforeEach(() => {
  vi.mocked(refreshOpenAIQuota).mockReset()
  vi.mocked(resetOpenAIQuota).mockReset()
  vi.mocked(setOpenAICreditsEnabled).mockReset()
})

afterEach(() => vi.useRealTimers())

describe('OpenAIQuotaResetCell — 点数开关', () => {
  const toggle = (wrapper: ReturnType<typeof mount>) => wrapper.get('[data-testid="openai-credits-enabled-toggle"]')
  const creditsDialog = (wrapper: ReturnType<typeof mount>) => wrapper.findAllComponents(ConfirmDialog)[1]

  it('defaults off and asks for confirmation before enabling', async () => {
    const updated = makeAccount({ extra: { openai_credits_enabled: true } })
    vi.mocked(setOpenAICreditsEnabled).mockResolvedValue(updated)
    const wrapper = mount(OpenAIQuotaResetCell, { props: { account: makeAccount({}) } })

    expect(toggle(wrapper).attributes('aria-checked')).toBe('false')
    await toggle(wrapper).trigger('click')
    expect(setOpenAICreditsEnabled).not.toHaveBeenCalled()
    expect(creditsDialog(wrapper).props('show')).toBe(true)

    creditsDialog(wrapper).vm.$emit('confirm')
    await flushPromises()
    expect(setOpenAICreditsEnabled).toHaveBeenCalledWith(1, true)
    expect(toggle(wrapper).attributes('aria-checked')).toBe('true')
    expect(wrapper.emitted('account-updated')).toEqual([[updated]])
    wrapper.unmount()
  })

  it('cancelling the confirmation keeps credits disabled', async () => {
    const wrapper = mount(OpenAIQuotaResetCell, { props: { account: makeAccount({}) } })
    await toggle(wrapper).trigger('click')
    creditsDialog(wrapper).vm.$emit('cancel')
    await flushPromises()
    expect(setOpenAICreditsEnabled).not.toHaveBeenCalled()
    expect(toggle(wrapper).attributes('aria-checked')).toBe('false')
    wrapper.unmount()
  })

  it('disables without confirmation', async () => {
    vi.mocked(setOpenAICreditsEnabled).mockResolvedValue(makeAccount({ extra: { openai_credits_enabled: false } }))
    const wrapper = mount(OpenAIQuotaResetCell, { props: { account: makeAccount({ extra: { openai_credits_enabled: true } }) } })
    expect(toggle(wrapper).attributes('aria-checked')).toBe('true')
    await toggle(wrapper).trigger('click')
    await flushPromises()
    expect(setOpenAICreditsEnabled).toHaveBeenCalledWith(1, false)
    expect(creditsDialog(wrapper).props('show')).toBe(false)
    expect(toggle(wrapper).attributes('aria-checked')).toBe('false')
    wrapper.unmount()
  })

  it('shows the error and keeps the state when the request fails', async () => {
    vi.mocked(setOpenAICreditsEnabled).mockRejectedValue(new Error('toggle failed'))
    const wrapper = mount(OpenAIQuotaResetCell, { props: { account: makeAccount({}) } })
    await toggle(wrapper).trigger('click')
    creditsDialog(wrapper).vm.$emit('confirm')
    await flushPromises()
    expect(wrapper.text()).toContain('toggle failed')
    expect(toggle(wrapper).attributes('aria-checked')).toBe('false')
    wrapper.unmount()
  })
})

describe('OpenAIQuotaResetCell — Codex 点数', () => {
  const points = (wrapper: ReturnType<typeof mount>) => wrapper.get('[data-testid="codex-credits"]')
  const balance = { has_credits: true, unlimited: false, balance: '12345678901234567890.0123' }
  // 显示时四舍五入到整数，按字符串处理以保留大数精度
  const balanceDisplay = '12345678901234567890'

  it.each([
    { name: 'large decimal rounds to integer', credits: balance, expected: balanceDisplay },
    { name: 'rounds half up', credits: { has_credits: true, unlimited: false, balance: '1200.50' }, expected: '1201' },
    { name: 'carries through nines', credits: { has_credits: true, unlimited: false, balance: '999.999999' }, expected: '1000' },
    { name: 'integer stays unchanged', credits: { has_credits: true, unlimited: false, balance: '42' }, expected: '42' },
    { name: 'zero', credits: { has_credits: false, unlimited: false, balance: '0' }, expected: '0' },
    { name: 'unlimited takes precedence', credits: { has_credits: false, unlimited: true, balance: null }, expected: 'admin.accounts.openaiQuotaReset.pointsUnlimited' },
    { name: 'hidden balance', credits: { has_credits: true, unlimited: false, balance: null }, expected: 'admin.accounts.openaiQuotaReset.pointsAvailable' },
    { name: 'invalid balance', credits: { has_credits: true, unlimited: false, balance: 'NaN' }, expected: 'admin.accounts.openaiQuotaReset.pointsAvailable' },
    { name: 'absent credits', credits: undefined, expected: '—' },
    { name: 'null credits', credits: null, expected: '—' },
  ])('queries and displays $name independently of reset cards', async ({ credits, expected }) => {
    vi.mocked(refreshOpenAIQuota).mockResolvedValue({
      credits, fetched_at: 1770000000, cache_persisted: false, credits_cache_persisted: true,
    })
    const wrapper = mount(OpenAIQuotaResetCell, { props: { account: makeAccount({}) } })
    expect(points(wrapper).text()).toContain('—')
    await points(wrapper).trigger('click')
    await flushPromises()
    expect(refreshOpenAIQuota).toHaveBeenCalledWith(1)
    expect(points(wrapper).text()).toContain(expected)
    if (/^\d+$/.test(expected)) expect(points(wrapper).find('span').text()).toBe(expected)
    expect(resetButton(wrapper).attributes('disabled')).toBeDefined()
    expect(resetOpenAIQuota).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('rehydrates points with their query timestamp without an upstream request', () => {
    const wrapper = mount(OpenAIQuotaResetCell, { props: { account: makeAccount({
      extra: { codex_credits_snapshot: { credits: balance, fetched_at: 1770000000 } },
    }) } })
    expect(points(wrapper).text()).toContain(balanceDisplay)
    expect(points(wrapper).text()).not.toContain(balance.balance)
    // 悬停提示保留上游精确值
    expect(points(wrapper).attributes('title')).toContain(balance.balance)
    expect(points(wrapper).attributes('title')).toContain('admin.accounts.openaiQuotaReset.pointsUpdatedAt:')
    expect(refreshOpenAIQuota).not.toHaveBeenCalled()
    expect(resetButton(wrapper).attributes('disabled')).toBeDefined()
    wrapper.unmount()
  })

  it('keeps the last balance on a request failure, but clears it on a successful unknown response', async () => {
    const wrapper = mount(OpenAIQuotaResetCell, { props: { account: makeAccount({
      extra: { codex_credits_snapshot: { credits: balance, fetched_at: 1770000000 } },
    }) } })
    vi.mocked(refreshOpenAIQuota).mockRejectedValueOnce(new Error('network unavailable'))
    await points(wrapper).trigger('click')
    await flushPromises()
    expect(points(wrapper).text()).toContain(balanceDisplay)
    expect(wrapper.text()).toContain('network unavailable')

    vi.mocked(refreshOpenAIQuota).mockResolvedValueOnce({ fetched_at: 1770000001, cache_persisted: true, credits_cache_persisted: true })
    await points(wrapper).trigger('click')
    await flushPromises()
    expect(points(wrapper).text()).toContain('—')
    expect(wrapper.text()).not.toContain(balanceDisplay)
    wrapper.unmount()
  })

  it('reports a points-cache write failure while displaying the live balance', async () => {
    vi.mocked(refreshOpenAIQuota).mockResolvedValue({
      credits: balance, fetched_at: 1770000000, cache_persisted: true, credits_cache_persisted: false,
    })
    const wrapper = mount(OpenAIQuotaResetCell, { props: { account: makeAccount({}) } })
    await points(wrapper).trigger('click')
    await flushPromises()
    expect(points(wrapper).text()).toContain(balanceDisplay)
    expect(wrapper.text()).toContain('admin.accounts.openaiQuotaReset.pointsCachePersistFailed')
    wrapper.unmount()
  })

  it('does not apply a pending response to another account row', async () => {
    let resolve!: (value: OpenAIQuotaRefreshResult) => void
    vi.mocked(refreshOpenAIQuota).mockReturnValue(new Promise((done) => { resolve = done }))
    const wrapper = mount(OpenAIQuotaResetCell, { props: { account: makeAccount({}) } })
    await points(wrapper).trigger('click')
    expect(points(wrapper).attributes('disabled')).toBeDefined()
    await wrapper.setProps({ account: makeAccount({ id: 2 }) })
    resolve({ credits: balance, fetched_at: 1770000000, cache_persisted: true, credits_cache_persisted: true })
    await flushPromises()
    expect(points(wrapper).text()).toContain('—')
    expect(points(wrapper).attributes('disabled')).toBeUndefined()
    wrapper.unmount()
  })
})

describe('OpenAIQuotaResetCell — 重置次数', () => {
  it('未查询时禁用原因是「需先查询」', () => {
    const account = makeAccount({})
    const wrapper = mount(OpenAIQuotaResetCell, { props: { account } })

    const btn = resetButton(wrapper)
    expect(btn.attributes('disabled')).toBeDefined()
    expect(btn.attributes('title')).toBe('admin.accounts.openaiQuotaReset.resetTooltipNeedQuery')
    wrapper.unmount()
  })

  it('从账号 extra 缓存恢复重置卡次数和到期时间', () => {
    const account = makeAccount({
      extra: {
        codex_reset_credit_snapshot: {
          available_count: 2,
          credits: [
            { expires_at: FUTURE_EXPIRY_LATE },
            { expires_at: FUTURE_EXPIRY_EARLY },
          ],
        },
      },
    })
    const wrapper = mount(OpenAIQuotaResetCell, { props: { account } })

    expect(refreshOpenAIQuota).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('admin.accounts.openaiQuotaReset.count')
    expect(wrapper.text()).toContain('admin.accounts.openaiQuotaReset.expiresAt:')
    expect(wrapper.text()).toContain('+1')
    expect(resetButton(wrapper).attributes('disabled')).toBeUndefined()
    wrapper.unmount()
  })

  it('缓存中的重置卡全部过期时视为未知,不点亮重置入口', () => {
    const account = makeAccount({
      extra: {
        codex_reset_credit_snapshot: {
          available_count: 1,
          credits: [{ expires_at: PAST_EXPIRY }],
        },
      },
    })
    const wrapper = mount(OpenAIQuotaResetCell, { props: { account } })

    expect(wrapper.text()).not.toContain('admin.accounts.openaiQuotaReset.expiresAt:')
    const btn = resetButton(wrapper)
    expect(btn.attributes('disabled')).toBeDefined()
    expect(btn.attributes('title')).toBe('admin.accounts.openaiQuotaReset.resetTooltipNeedQuery')
    wrapper.unmount()
  })

  it('缓存次数向未过期的明细数量收敛', () => {
    const account = makeAccount({
      extra: {
        codex_reset_credit_snapshot: {
          available_count: 3,
          credits: [
            { expires_at: PAST_EXPIRY },
            { expires_at: FUTURE_EXPIRY_EARLY },
          ],
        },
      },
    })
    const wrapper = mount(OpenAIQuotaResetCell, { props: { account } })

    expect(wrapper.text()).toContain('admin.accounts.openaiQuotaReset.count1')
    expect(wrapper.text()).not.toContain('+1')
    expect(resetButton(wrapper).attributes('disabled')).toBeUndefined()
    wrapper.unmount()
  })

  it('查询后默认折叠为最早到期时间,点击 +N 展开完整列表', async () => {
    vi.mocked(refreshOpenAIQuota).mockResolvedValue({
      rate_limit_reset_credits: {
        available_count: 3,
        credits: [
          { expires_at: '2026-07-05T04:05:06Z' },
          { expires_at: '2026-07-03T04:05:06Z' },
          { expires_at: 'not-a-date' },
        ],
      },
      fetched_at: 1770000000,
      cache_persisted: true,
    })

    const account = makeAccount({})
    const wrapper = mount(OpenAIQuotaResetCell, { props: { account } })

    await wrapper.findAll('button')[0].trigger('click')
    await flushPromises()

    expect(refreshOpenAIQuota).toHaveBeenCalledWith(1)
    expect(wrapper.text()).toContain('admin.accounts.openaiQuotaReset.expiresAt:')
    expect(wrapper.text()).toContain('+2')
    expect(wrapper.text()).not.toContain('not-a-date')

    const toggle = wrapper.find('[data-testid="reset-credit-expiry-toggle"]')
    expect(toggle.exists()).toBe(true)
    expect(toggle.attributes('aria-expanded')).toBe('false')
    await toggle.trigger('click')

    expect(toggle.attributes('aria-expanded')).toBe('true')
    expect(wrapper.find('[data-testid="reset-credit-expiry-details"]').exists()).toBe(true)
    expect(wrapper.text()).toContain('not-a-date')
    expect(wrapper.text()).not.toContain('undefined')
    wrapper.unmount()
  })

  it('只有一张重置卡时不显示展开按钮', async () => {
    vi.mocked(refreshOpenAIQuota).mockResolvedValue({
      rate_limit_reset_credits: {
        available_count: 1,
        credits: [
          { expires_at: '2026-07-03T04:05:06Z' },
        ],
      },
      fetched_at: 1770000000,
      cache_persisted: true,
    })

    const account = makeAccount({})
    const wrapper = mount(OpenAIQuotaResetCell, { props: { account } })

    await wrapper.findAll('button')[0].trigger('click')
    await flushPromises()

    expect(wrapper.find('[data-testid="reset-credit-expiry-toggle"]').exists()).toBe(false)
    expect(wrapper.find('[data-testid="reset-credit-expiry-details"]').exists()).toBe(false)
    expect(wrapper.text()).toContain('admin.accounts.openaiQuotaReset.expiresAt:')
    wrapper.unmount()
  })

  // 快照写库被拒绝(上游未返回到期明细)不得吞掉这次成功的上游读取,
  // 否则次数永远显示不出来、重置入口被永久禁用。
  it('快照持久化失败时仍显示实时次数并给出警告', async () => {
    vi.mocked(refreshOpenAIQuota).mockResolvedValue({
      rate_limit_reset_credits: { available_count: 2 },
      fetched_at: 1770000000,
      cache_persisted: false,
    })

    const account = makeAccount({})
    const wrapper = mount(OpenAIQuotaResetCell, { props: { account } })

    await wrapper.findAll('button')[0].trigger('click')
    await flushPromises()

    expect(wrapper.text()).toContain('admin.accounts.openaiQuotaReset.count2')
    expect(wrapper.text()).toContain('admin.accounts.openaiQuotaReset.refreshCachePersistFailed')
    expect(resetButton(wrapper).attributes('disabled')).toBeUndefined()
    wrapper.unmount()
  })

  it('重置成功后直接使用响应中的最新缓存并回传恢复后的账号', async () => {
    const recoveredAccount = makeAccount({
      status: 'active',
      error_message: null,
    })
    vi.mocked(resetOpenAIQuota).mockResolvedValue({
      code: 'success',
      windows_reset: 1,
      cache_refreshed: true,
      account_state_recovered: true,
      quota: {
        credits: { has_credits: true, unlimited: false, balance: '999.25' },
        rate_limit_reset_credits: {
          available_count: 0,
          credits: [],
        },
        fetched_at: 1770000000,
      },
      account: recoveredAccount,
    })
    const account = makeAccount({
      extra: {
        codex_reset_credit_snapshot: {
          available_count: 1,
          credits: [{ expires_at: FUTURE_EXPIRY_EARLY }],
        },
      },
    })
    const wrapper = mount(OpenAIQuotaResetCell, { props: { account } })

    await resetButton(wrapper).trigger('click')
    wrapper.findComponent(ConfirmDialog).vm.$emit('confirm')
    await flushPromises()

    expect(resetOpenAIQuota).toHaveBeenCalledWith(1)
    expect(refreshOpenAIQuota).not.toHaveBeenCalled()
    expect(wrapper.get('[data-testid="codex-credits"]').text()).toContain('999')
    expect(wrapper.get('[data-testid="codex-credits"]').text()).not.toContain('999.25')
    expect(wrapper.text()).not.toContain('admin.accounts.openaiQuotaReset.expiresAt:')
    expect(wrapper.text()).toContain('admin.accounts.openaiQuotaReset.resetSuccess')
    expect(wrapper.emitted('account-updated')).toEqual([[recoveredAccount]])
    wrapper.unmount()
  })

  // 缓存回读失败不影响「账号状态已恢复」这一主目标:恢复后的账号行必须照常回传,
  // 否则列表会继续显示已经不存在的限流状态。
  it('缓存刷新失败时仍回传恢复后的账号并把次数标为未知', async () => {
    const recoveredAccount = makeAccount({
      status: 'active',
      rate_limit_reset_at: null,
    })
    vi.mocked(resetOpenAIQuota).mockResolvedValue({
      code: 'success',
      windows_reset: 1,
      cache_refreshed: false,
      account_state_recovered: true,
      warning_code: 'reset_credit_cache_refresh_failed',
      account: recoveredAccount,
    })
    const account = makeAccount({
      extra: {
        codex_reset_credit_snapshot: {
          available_count: 1,
          credits: [{ expires_at: FUTURE_EXPIRY_EARLY }],
        },
      },
    })
    const wrapper = mount(OpenAIQuotaResetCell, { props: { account } })

    await resetButton(wrapper).trigger('click')
    wrapper.findComponent(ConfirmDialog).vm.$emit('confirm')
    await flushPromises()

    expect(refreshOpenAIQuota).not.toHaveBeenCalled()
    // 次数未知(隐藏)但仍展示已持久化的到期明细,重置入口保持禁用直到重新查询。
    expect(wrapper.text()).not.toContain('admin.accounts.openaiQuotaReset.count1')
    expect(wrapper.text()).toContain('admin.accounts.openaiQuotaReset.expiresAt:')
    expect(wrapper.text()).toContain('admin.accounts.openaiQuotaReset.resetCacheRefreshFailed')
    expect(resetButton(wrapper).attributes('disabled')).toBeDefined()
    expect(wrapper.emitted('account-updated')).toEqual([[recoveredAccount]])
    wrapper.unmount()
  })

  it('账号状态恢复失败时停止后续步骤并提示手动恢复', async () => {
    vi.mocked(resetOpenAIQuota).mockResolvedValue({
      code: 'success',
      windows_reset: 1,
      cache_refreshed: false,
      account_state_recovered: false,
      warning_code: 'account_state_recovery_failed',
    })
    const account = makeAccount({
      extra: {
        codex_reset_credit_snapshot: {
          available_count: 1,
          credits: [{ expires_at: FUTURE_EXPIRY_EARLY }],
        },
      },
    })
    const wrapper = mount(OpenAIQuotaResetCell, { props: { account } })

    await resetButton(wrapper).trigger('click')
    wrapper.findComponent(ConfirmDialog).vm.$emit('confirm')
    await flushPromises()

    expect(refreshOpenAIQuota).not.toHaveBeenCalled()
    expect(wrapper.text()).toContain('admin.accounts.openaiQuotaReset.resetAccountRecoveryFailed')
    expect(resetButton(wrapper).attributes('disabled')).toBeDefined()
    expect(wrapper.emitted('account-updated')).toBeUndefined()
    wrapper.unmount()
  })
})
describe('OpenAIQuotaResetCell 自动用卡运行态', () => {
  it.each([
    ['checking', 'checking'],
    ['available', 'available'],
    ['resetting', 'resetting'],
    ['success', 'success'],
    ['no_credit', 'noCredit'],
    ['failed', 'failed'],
  ] as const)('展示 %s 状态且不需要暴露卡标识', (status, labelKey) => {
    const account = makeAccount({
      extra: {
        auto_reset_credit_enabled: true,
        auto_reset_credit_opt_in_version: 1,
        codex_auto_reset_credit_state: {
          status,
          trigger_window: '5h',
          available_count: 1,
          checked_at: '2099-07-03T04:05:06Z',
          error_code: status === 'failed' ? 'RESET_FAILED' : undefined,
        },
      },
    })
    const wrapper = mount(OpenAIQuotaResetCell, { props: { account } })
    const state = wrapper.get('[data-testid="auto-reset-credit-state"]')
    expect(state.text()).toContain(`admin.accounts.openaiQuotaReset.autoStatus.${labelKey}`)
    expect(state.text()).toContain('5h')
    expect(state.text()).not.toContain('credit_id')
    wrapper.unmount()
  })

  it('开关关闭时不显示历史运行态', () => {
    const account = makeAccount({
      extra: {
        auto_reset_credit_enabled: false,
        codex_auto_reset_credit_state: { status: 'success', available_count: 1 },
      },
    })
    const wrapper = mount(OpenAIQuotaResetCell, { props: { account } })
    expect(wrapper.find('[data-testid="auto-reset-credit-state"]').exists()).toBe(false)
    wrapper.unmount()
  })

  it('遗留 enabled=true 但没有启用版本时不显示自动用卡状态', () => {
    const account = makeAccount({ extra: {
      auto_reset_credit_enabled: true,
      codex_auto_reset_credit_state: { status: 'success', available_count: 1 },
      codex_reset_credit_snapshot: { available_count: 1, credits: [{ expires_at: FUTURE_EXPIRY_EARLY }] }
    } })
    const wrapper = mount(OpenAIQuotaResetCell, { props: { account } })
    expect(wrapper.find('[data-testid="auto-reset-credit-state"]').exists()).toBe(false)
    expect(resetButton(wrapper).attributes('disabled')).toBeUndefined()
    wrapper.unmount()
  })

  it('后台自动用卡更新同一账号后立即显示最新卡数并关闭手动确认', async () => {
    const account = makeAccount({ extra: {
      auto_reset_credit_enabled: false,
      auto_reset_credit_opt_in_version: 1,
      codex_reset_credit_snapshot: { available_count: 1, credits: [{ expires_at: FUTURE_EXPIRY_EARLY }] },
      codex_credits_snapshot: { credits: { has_credits: true, unlimited: false, balance: '25' }, fetched_at: 1 }
    } })
    const wrapper = mount(OpenAIQuotaResetCell, { props: { account } })
    await resetButton(wrapper).trigger('click')
    expect(wrapper.findAllComponents(ConfirmDialog)[0].props('show')).toBe(true)
    await wrapper.setProps({ account: { ...account, extra: {
      ...account.extra,
      codex_auto_reset_credit_state: { status: 'success', available_count: 0 },
      codex_reset_credit_snapshot: { available_count: 0, credits: [] },
      codex_credits_snapshot: { credits: { has_credits: true, unlimited: false, balance: '23' }, fetched_at: 2 }
    } } })
    expect(resetButton(wrapper).attributes('disabled')).toBeDefined()
    expect(resetButton(wrapper).attributes('title')).toBe('admin.accounts.openaiQuotaReset.resetTooltipNoCredits')
    expect(wrapper.findAllComponents(ConfirmDialog)[0].props('show')).toBe(false)
    expect(wrapper.get('[data-testid="codex-credits"]').text()).toContain('23')
    expect(refreshOpenAIQuota).not.toHaveBeenCalled()
    expect(resetOpenAIQuota).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('后台自动重置进行中时不能再手动消耗重置卡', async () => {
    const account = makeAccount({ extra: {
      auto_reset_credit_enabled: false,
      auto_reset_credit_opt_in_version: 1,
      codex_reset_credit_snapshot: { available_count: 1, credits: [{ expires_at: FUTURE_EXPIRY_EARLY }] }
    } })
    const wrapper = mount(OpenAIQuotaResetCell, { props: { account } })
    await resetButton(wrapper).trigger('click')
    await wrapper.setProps({ account: { ...account, extra: {
      ...account.extra,
      codex_auto_reset_credit_state: { status: 'resetting', checked_at: new Date().toISOString(), available_count: 1 }
    } } })
    expect(resetButton(wrapper).attributes('disabled')).toBeDefined()
    expect(resetButton(wrapper).attributes('title')).toBe('admin.accounts.openaiQuotaReset.resetTooltipAutoInProgress')
    expect(wrapper.findAllComponents(ConfirmDialog)[0].props('show')).toBe(false)
    expect(resetOpenAIQuota).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('自动开关有效开启时，手动入口提示先关闭自动用卡', () => {
    const account = makeAccount({ extra: {
      auto_reset_credit_enabled: true,
      auto_reset_credit_opt_in_version: 1,
      codex_reset_credit_snapshot: { available_count: 1, credits: [{ expires_at: FUTURE_EXPIRY_EARLY }] }
    } })
    const wrapper = mount(OpenAIQuotaResetCell, { props: { account } })
    expect(resetButton(wrapper).attributes('disabled')).toBeDefined()
    expect(resetButton(wrapper).attributes('title')).toBe('admin.accounts.openaiQuotaReset.resetTooltipAutoEnabled')
    wrapper.unmount()
  })

  it('关闭自动用卡后，在途检查保护在两分钟结束时自行解除', async () => {
    vi.useFakeTimers()
    vi.setSystemTime(new Date('2026-11-06T00:00:00Z'))
    const account = makeAccount({ extra: {
      auto_reset_credit_enabled: false,
      auto_reset_credit_opt_in_version: 1,
      codex_auto_reset_credit_state: { status: 'checking', checked_at: new Date().toISOString(), available_count: 1 },
      codex_reset_credit_snapshot: { available_count: 1, credits: [{ expires_at: FUTURE_EXPIRY_EARLY }] }
    } })
    const wrapper = mount(OpenAIQuotaResetCell, { props: { account } })
    expect(resetButton(wrapper).attributes('title')).toBe('admin.accounts.openaiQuotaReset.resetTooltipAutoInProgress')
    await vi.advanceTimersByTimeAsync(120_000)
    expect(resetButton(wrapper).attributes('disabled')).toBeUndefined()
    expect(resetButton(wrapper).attributes('title')).toBe('admin.accounts.openaiQuotaReset.resetTooltipReady')
    wrapper.unmount()
  })

  it.each([
    ['OPENAI_QUOTA_AUTO_RESET_ENABLED', 'resetTooltipAutoEnabled'],
    ['OPENAI_QUOTA_AUTO_RESET_IN_PROGRESS', 'resetTooltipAutoInProgress']
  ])('后端返回 %s 时显示可操作的本地化提示', async (reason, key) => {
    vi.mocked(resetOpenAIQuota).mockRejectedValue({ reason, message: 'backend error' })
    const account = makeAccount({ extra: {
      codex_reset_credit_snapshot: { available_count: 1, credits: [{ expires_at: FUTURE_EXPIRY_EARLY }] }
    } })
    const wrapper = mount(OpenAIQuotaResetCell, { props: { account } })
    await resetButton(wrapper).trigger('click')
    wrapper.findAllComponents(ConfirmDialog)[0].vm.$emit('confirm')
    await flushPromises()
    expect(wrapper.text()).toContain(`admin.accounts.openaiQuotaReset.${key}`)
    expect(wrapper.text()).not.toContain('backend error')
    wrapper.unmount()
  })

  it('忽略后台用卡新快照之前发起的迟到查询', async () => {
    const account = makeAccount({ extra: {
      codex_reset_credit_snapshot: { available_count: 1, credits: [{ expires_at: FUTURE_EXPIRY_EARLY }] }
    } })
    let resolve!: (value: OpenAIQuotaRefreshResult) => void
    vi.mocked(refreshOpenAIQuota).mockReturnValue(new Promise((done) => { resolve = done }))
    const wrapper = mount(OpenAIQuotaResetCell, { props: { account } })
    await wrapper.findAll('button')[0].trigger('click')
    await wrapper.setProps({ account: { ...account, extra: {
      codex_reset_credit_snapshot: { available_count: 0, credits: [] }
    } } })
    resolve({ fetched_at: 1, cache_persisted: true, rate_limit_reset_credits: {
      available_count: 1, credits: [{ expires_at: FUTURE_EXPIRY_EARLY }]
    } })
    await flushPromises()
    expect(resetButton(wrapper).attributes('disabled')).toBeDefined()
    expect(resetButton(wrapper).attributes('title')).toBe('admin.accounts.openaiQuotaReset.resetTooltipNoCredits')
    wrapper.unmount()
  })

  it('无变化的后台快照不会覆盖主动查询得到的实时卡数', async () => {
    const account = makeAccount({ extra: { codex_reset_credit_snapshot: { available_count: 0, credits: [] } } })
    vi.mocked(refreshOpenAIQuota).mockResolvedValue({ fetched_at: 1, cache_persisted: false,
      rate_limit_reset_credits: { available_count: 2 }
    })
    const wrapper = mount(OpenAIQuotaResetCell, { props: { account } })
    await wrapper.findAll('button')[0].trigger('click')
    await flushPromises()
    await wrapper.setProps({ account: { ...account, extra: { codex_reset_credit_snapshot: { available_count: 0, credits: [] } } } })
    expect(wrapper.text()).toContain('admin.accounts.openaiQuotaReset.count2')
    expect(resetButton(wrapper).attributes('disabled')).toBeUndefined()
    wrapper.unmount()
  })
})
