import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'
import OpenAIQuotaResetCell from '../OpenAIQuotaResetCell.vue'
import ConfirmDialog from '@/components/common/ConfirmDialog.vue'
import type { Account } from '@/types'

const api = vi.hoisted(() => ({
  setOpenAIAutoResetCreditEnabled: vi.fn(),
  setOpenAICreditsEnabled: vi.fn(),
  refreshOpenAIQuota: vi.fn(),
  resetOpenAIQuota: vi.fn()
}))
vi.mock('@/api/admin/accounts', () => api)
vi.mock('vue-i18n', async () => ({
  ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'),
  useI18n: () => ({ t: (key: string, params?: Record<string, unknown>) =>
    key === 'admin.accounts.autoResetCredit.enableConfirmMessage'
      ? `${key}: ${params?.threshold5h}% / ${params?.threshold7d}%` : key
  })
}))

function makeAccount(overrides: Partial<Account> = {}): Account {
  return {
    id: 1,
    name: 'GPT OAuth',
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
    ...overrides
  }
}

function accountWithSettings(enabled = false, overrides: Partial<Account> = {}): Account {
  return makeAccount({
    extra: {
      auto_reset_credit_enabled: enabled,
      auto_reset_credit_opt_in_version: 1,
      auto_reset_credit_5h_threshold: 0.92,
      auto_reset_credit_7d_threshold: 0.84,
      codex_reset_credit_snapshot: {
        available_count: 1,
        credits: [{ expires_at: '2099-01-01T00:00:00Z' }]
      }
    },
    ...overrides
  })
}

function deferred<T>() {
  let resolve!: (value: T) => void
  let reject!: (reason: unknown) => void
  const promise = new Promise<T>((done, fail) => { resolve = done; reject = fail })
  return { promise, resolve, reject }
}

const wrappers: VueWrapper[] = []
function mountCell(account: Account = makeAccount()) {
  const wrapper = mount(OpenAIQuotaResetCell, {
    props: { account },
    global: { stubs: { ConfirmDialog: true, OpenAIReferralCell: true } }
  })
  wrappers.push(wrapper)
  return wrapper
}

const toggle = (wrapper: VueWrapper) => wrapper.get('[data-testid="auto-reset-credit-enabled-toggle"]')
const resetButton = (wrapper: VueWrapper) => wrapper.get('[data-testid="openai-quota-reset"]')
const dialog = (wrapper: VueWrapper, title: string) => {
  const result = wrapper.findAllComponents(ConfirmDialog).find(item => item.props('title') === title)
  expect(result, `confirmation dialog ${title}`).toBeDefined()
  return result!
}
const enableDialog = (wrapper: VueWrapper) => dialog(wrapper, 'admin.accounts.autoResetCredit.enableConfirmTitle')
const resetDialog = (wrapper: VueWrapper) => dialog(wrapper, 'admin.accounts.openaiQuotaReset.confirmTitle')

async function confirmEnable(wrapper: VueWrapper) {
  await toggle(wrapper).trigger('click')
  expect(enableDialog(wrapper).props('show')).toBe(true)
  enableDialog(wrapper).vm.$emit('confirm')
  await flushPromises()
}

describe('OpenAIQuotaResetCell automatic reset-credit switch', () => {
  beforeEach(() => {
    Object.values(api).forEach(mock => mock.mockReset())
  })

  afterEach(() => {
    wrappers.splice(0).forEach(wrapper => wrapper.unmount())
  })

  it.each([
    ['missing settings', undefined],
    ['disabled', { auto_reset_credit_enabled: false, auto_reset_credit_opt_in_version: 1 }],
    ['legacy enable without opt-in', { auto_reset_credit_enabled: true }],
    ['legacy version', { auto_reset_credit_enabled: true, auto_reset_credit_opt_in_version: 0 }],
    ['unknown version', { auto_reset_credit_enabled: true, auto_reset_credit_opt_in_version: 2 }],
    ['string enabled', { auto_reset_credit_enabled: 'true', auto_reset_credit_opt_in_version: 1 }],
    ['string version', { auto_reset_credit_enabled: true, auto_reset_credit_opt_in_version: '1' }]
  ])('defaults off with %s', (_label, extra) => {
    const wrapper = mountCell(makeAccount({ extra: extra as Account['extra'] }))
    expect(toggle(wrapper).attributes('role')).toBe('switch')
    expect(toggle(wrapper).attributes('aria-checked')).toBe('false')
    expect(toggle(wrapper).attributes('disabled')).toBeUndefined()
    expect(api.setOpenAIAutoResetCreditEnabled).not.toHaveBeenCalled()
    expect(api.refreshOpenAIQuota).not.toHaveBeenCalled()
  })

  it('reflects an explicit supported opt-in and blocks manual reset', () => {
    const wrapper = mountCell(accountWithSettings(true))
    expect(toggle(wrapper).attributes('aria-checked')).toBe('true')
    expect(resetButton(wrapper).attributes('disabled')).toBeDefined()
    expect(resetButton(wrapper).attributes('title')).toBe('admin.accounts.openaiQuotaReset.resetTooltipAutoEnabled')
  })

  it.each([
    { platform: 'openai', type: 'apikey' },
    { platform: 'anthropic', type: 'oauth' }
  ] as const)('does not expose the switch for $platform / $type', overrides => {
    const wrapper = mountCell(makeAccount(overrides))
    expect(wrapper.find('[data-testid="auto-reset-credit-enabled-toggle"]').exists()).toBe(false)
  })

  it('asks for confirmation and leaves the switch off after cancellation', async () => {
    const wrapper = mountCell()
    await toggle(wrapper).trigger('click')
    expect(enableDialog(wrapper).props('show')).toBe(true)
    expect(toggle(wrapper).attributes('aria-checked')).toBe('false')
    expect(api.setOpenAIAutoResetCreditEnabled).not.toHaveBeenCalled()
    enableDialog(wrapper).vm.$emit('cancel')
    await flushPromises()
    expect(enableDialog(wrapper).props('show')).toBe(false)
    expect(toggle(wrapper).attributes('aria-checked')).toBe('false')
    expect(api.setOpenAIAutoResetCreditEnabled).not.toHaveBeenCalled()
    expect(wrapper.emitted('account-updated')).toBeUndefined()
  })

  it('sends only the enabled flag and emits the server account without consuming a card', async () => {
    const account = accountWithSettings()
    const original = JSON.stringify(account)
    const updated = { ...accountWithSettings(true), updated_at: '2026-11-13T08:00:00Z' }
    api.setOpenAIAutoResetCreditEnabled.mockResolvedValue(updated)
    const wrapper = mountCell(account)
    await confirmEnable(wrapper)

    expect(api.setOpenAIAutoResetCreditEnabled.mock.calls).toEqual([[1, true]])
    expect(toggle(wrapper).attributes('aria-checked')).toBe('true')
    expect(wrapper.emitted('account-updated')).toEqual([[updated]])
    expect(JSON.stringify(account)).toBe(original)
    expect(api.refreshOpenAIQuota).not.toHaveBeenCalled()
    expect(api.resetOpenAIQuota).not.toHaveBeenCalled()
    expect(api.setOpenAICreditsEnabled).not.toHaveBeenCalled()
  })

  it('can configure automatic resets while the card count is unknown', async () => {
    api.setOpenAIAutoResetCreditEnabled.mockResolvedValue(accountWithSettings(true))
    const wrapper = mountCell()
    expect(resetButton(wrapper).attributes('disabled')).toBeDefined()
    expect(toggle(wrapper).attributes('disabled')).toBeUndefined()
    await confirmEnable(wrapper)
    expect(api.setOpenAIAutoResetCreditEnabled).toHaveBeenCalledWith(1, true)
  })

  it.each([
    { account: accountWithSettings(), expected: '92% / 84%' },
    { account: makeAccount(), expected: '100% / 100%' }
  ])('confirms the current threshold settings: $expected', async ({ account, expected }) => {
    const wrapper = mountCell(account)
    await toggle(wrapper).trigger('click')
    expect(enableDialog(wrapper).props('message')).toBe(`admin.accounts.autoResetCredit.enableConfirmMessage: ${expected}`)
    expect(api.setOpenAIAutoResetCreditEnabled).not.toHaveBeenCalled()
  })

  it('disables directly without confirmation and makes manual reset available after success', async () => {
    const updated = accountWithSettings(false)
    api.setOpenAIAutoResetCreditEnabled.mockResolvedValue(updated)
    const wrapper = mountCell(accountWithSettings(true))
    await toggle(wrapper).trigger('click')
    expect(enableDialog(wrapper).props('show')).toBe(false)
    await flushPromises()
    expect(api.setOpenAIAutoResetCreditEnabled.mock.calls).toEqual([[1, false]])
    expect(toggle(wrapper).attributes('aria-checked')).toBe('false')
    expect(resetButton(wrapper).attributes('disabled')).toBeUndefined()
    expect(wrapper.emitted('account-updated')).toEqual([[updated]])
  })

  it('blocks duplicate toggle requests and manual resets while saving', async () => {
    const pending = deferred<Account>()
    api.setOpenAIAutoResetCreditEnabled.mockReturnValue(pending.promise)
    const wrapper = mountCell(accountWithSettings())
    await confirmEnable(wrapper)
    expect(toggle(wrapper).attributes('disabled')).toBeDefined()
    expect(resetButton(wrapper).attributes('disabled')).toBeDefined()
    await toggle(wrapper).trigger('click')
    enableDialog(wrapper).vm.$emit('confirm')
    resetDialog(wrapper).vm.$emit('confirm')
    await flushPromises()
    expect(api.setOpenAIAutoResetCreditEnabled).toHaveBeenCalledTimes(1)
    expect(api.resetOpenAIQuota).not.toHaveBeenCalled()

    pending.resolve(accountWithSettings(true))
    await flushPromises()
    expect(toggle(wrapper).attributes('disabled')).toBeUndefined()
    expect(resetButton(wrapper).attributes('disabled')).toBeDefined()
  })

  it('prevents enabling automation while a manual reset is in flight', async () => {
    const pending = deferred<unknown>()
    api.resetOpenAIQuota.mockReturnValue(pending.promise)
    const wrapper = mountCell(accountWithSettings())
    await resetButton(wrapper).trigger('click')
    resetDialog(wrapper).vm.$emit('confirm')
    await flushPromises()
    expect(api.resetOpenAIQuota).toHaveBeenCalledWith(1)
    expect(toggle(wrapper).attributes('disabled')).toBeDefined()
    await toggle(wrapper).trigger('click')
    expect(enableDialog(wrapper).props('show')).toBe(false)
    expect(api.setOpenAIAutoResetCreditEnabled).not.toHaveBeenCalled()

    pending.resolve({ code: 'success', windows_reset: 1, cache_refreshed: false })
    await flushPromises()
    expect(toggle(wrapper).attributes('disabled')).toBeUndefined()
  })

  it.each([true, false])('keeps the previous enabled=%s state when saving fails', async enabled => {
    api.setOpenAIAutoResetCreditEnabled.mockRejectedValue(new Error('Setting could not be saved'))
    const wrapper = mountCell(accountWithSettings(enabled))
    if (enabled) {
      await toggle(wrapper).trigger('click')
      await flushPromises()
    } else {
      await confirmEnable(wrapper)
    }
    expect(toggle(wrapper).attributes('aria-checked')).toBe(String(enabled))
    expect(toggle(wrapper).attributes('disabled')).toBeUndefined()
    expect(wrapper.text()).toContain('Setting could not be saved')
    expect(wrapper.emitted('account-updated')).toBeUndefined()
    expect(resetButton(wrapper).attributes('disabled')).toBe(enabled ? '' : undefined)
  })

  it('uses the server opt-in result instead of assuming a requested enable succeeded', async () => {
    const updated = accountWithSettings(true)
    delete updated.extra!.auto_reset_credit_opt_in_version
    api.setOpenAIAutoResetCreditEnabled.mockResolvedValue(updated)
    const wrapper = mountCell(accountWithSettings())
    await confirmEnable(wrapper)
    expect(toggle(wrapper).attributes('aria-checked')).toBe('false')
    expect(wrapper.emitted('account-updated')).toEqual([[updated]])
  })

  it('adopts parent settings on refresh even when they match the original props', async () => {
    api.setOpenAIAutoResetCreditEnabled.mockResolvedValue(accountWithSettings(true))
    const wrapper = mountCell(accountWithSettings(false))
    await confirmEnable(wrapper)
    expect(toggle(wrapper).attributes('aria-checked')).toBe('true')
    await wrapper.setProps({ account: accountWithSettings(false) })
    expect(toggle(wrapper).attributes('aria-checked')).toBe('false')
    expect(resetButton(wrapper).attributes('disabled')).toBeUndefined()
  })

  it.each(['success', 'failure'])('ignores late %s after a parent changes automatic-reset settings', async outcome => {
    const pending = deferred<Account>()
    api.setOpenAIAutoResetCreditEnabled.mockReturnValue(pending.promise)
    const account = accountWithSettings(false)
    const wrapper = mountCell(account)
    await confirmEnable(wrapper)
    await wrapper.setProps({ account: { ...account, extra: {
      ...account.extra, auto_reset_credit_5h_threshold: 0.99
    } } })
    expect(toggle(wrapper).attributes('disabled')).toBeUndefined()
    if (outcome === 'success') pending.resolve(accountWithSettings(true))
    else pending.reject(new Error('Old setting failed'))
    await flushPromises()
    expect(toggle(wrapper).attributes('aria-checked')).toBe('false')
    expect(wrapper.emitted('account-updated')).toBeUndefined()
    expect(wrapper.text()).not.toContain('Old setting failed')
  })

  it('closes the confirmation when the component is reused for another account', async () => {
    const wrapper = mountCell()
    await toggle(wrapper).trigger('click')
    expect(enableDialog(wrapper).props('show')).toBe(true)
    await wrapper.setProps({ account: makeAccount({ id: 2 }) })
    expect(enableDialog(wrapper).props('show')).toBe(false)
    expect(api.setOpenAIAutoResetCreditEnabled).not.toHaveBeenCalled()
  })

  it.each(['success', 'failure'])('ignores a recycled row’s late %s without releasing the new row’s busy state', async outcome => {
    const first = deferred<Account>()
    const second = deferred<Account>()
    api.setOpenAIAutoResetCreditEnabled.mockReturnValueOnce(first.promise).mockReturnValueOnce(second.promise)
    const wrapper = mountCell(accountWithSettings(false))
    await confirmEnable(wrapper)
    await wrapper.setProps({ account: accountWithSettings(false, { id: 2 }) })
    await confirmEnable(wrapper)
    expect(api.setOpenAIAutoResetCreditEnabled.mock.calls).toEqual([[1, true], [2, true]])

    if (outcome === 'success') first.resolve(accountWithSettings(true))
    else first.reject(new Error('Previous row failed'))
    await flushPromises()
    expect(toggle(wrapper).attributes('aria-checked')).toBe('false')
    expect(toggle(wrapper).attributes('disabled')).toBeDefined()
    expect(wrapper.text()).not.toContain('Previous row failed')
    expect(wrapper.emitted('account-updated')).toBeUndefined()

    const updated = accountWithSettings(true, { id: 2 })
    second.resolve(updated)
    await flushPromises()
    expect(toggle(wrapper).attributes('aria-checked')).toBe('true')
    expect(toggle(wrapper).attributes('disabled')).toBeUndefined()
    expect(wrapper.emitted('account-updated')).toEqual([[updated]])
  })

  it('does not revive an old request when the recycled row returns to the same account', async () => {
    const first = deferred<Account>()
    const current = deferred<Account>()
    api.setOpenAIAutoResetCreditEnabled.mockReturnValueOnce(first.promise).mockReturnValueOnce(current.promise)
    const wrapper = mountCell(accountWithSettings(false))
    await confirmEnable(wrapper)
    await wrapper.setProps({ account: accountWithSettings(false, { id: 2 }) })
    await wrapper.setProps({ account: accountWithSettings(false) })
    await confirmEnable(wrapper)

    first.resolve(accountWithSettings(true))
    await flushPromises()
    expect(toggle(wrapper).attributes('aria-checked')).toBe('false')
    expect(toggle(wrapper).attributes('disabled')).toBeDefined()
    expect(wrapper.emitted('account-updated')).toBeUndefined()

    const updated = accountWithSettings(true)
    current.resolve(updated)
    await flushPromises()
    expect(toggle(wrapper).attributes('aria-checked')).toBe('true')
    expect(toggle(wrapper).attributes('disabled')).toBeUndefined()
    expect(wrapper.emitted('account-updated')).toEqual([[updated]])
  })
})
