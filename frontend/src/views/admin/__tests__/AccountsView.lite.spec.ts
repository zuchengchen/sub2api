import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { DOMWrapper, flushPromises, mount } from '@vue/test-utils'
import { defineComponent } from 'vue'

import AccountsView from '../AccountsView.vue'
import AccountActionMenu from '@/components/admin/account/AccountActionMenu.vue'
import OpenAIQuotaResetCell from '@/components/account/OpenAIQuotaResetCell.vue'

const {
  listAccounts,
  listWithEtag,
  getById,
  getBatchTodayStatsWithEtag,
  getUpstreamBillingProbeSettings,
  getAllProxies,
  getAllGroups,
  refreshCredentials,
  showError,
  showWarning
} = vi.hoisted(() => ({
  listAccounts: vi.fn(),
  listWithEtag: vi.fn(),
  getById: vi.fn(),
  getBatchTodayStatsWithEtag: vi.fn(),
  getUpstreamBillingProbeSettings: vi.fn(),
  getAllProxies: vi.fn(),
  getAllGroups: vi.fn(),
  refreshCredentials: vi.fn(),
  showError: vi.fn(),
  showWarning: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: {
    accounts: {
      list: listAccounts,
      getById,
      listWithEtag,
      getBatchTodayStatsWithEtag,
      getUpstreamBillingProbeSettings,
      delete: vi.fn(),
      batchClearError: vi.fn(),
      batchRefresh: vi.fn(),
      toggleSchedulable: vi.fn(),
      refreshCredentials
    },
    proxies: { getAll: getAllProxies },
    groups: { getAll: getAllGroups }
  }
}))

vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError, showWarning, showSuccess: vi.fn(), showInfo: vi.fn() })
}))

vi.mock('@/stores/auth', () => ({
  useAuthStore: () => ({ token: 'test-token', isSimpleMode: false })
}))

vi.mock('vue-i18n', async () => {
  const actual = await vi.importActual<typeof import('vue-i18n')>('vue-i18n')
  return { ...actual, useI18n: () => ({ t: (key: string) => key }) }
})

const DataTableStub = defineComponent({
  props: { data: { type: Array, default: () => [] } },
  template: `
    <div>
      <div v-for="row in data" :key="row.id" :data-account-name="row.name">
        <slot name="cell-groups" :row="row" />
        <slot name="cell-usage" :row="row" />
        <slot name="cell-actions" :row="row" />
      </div>
    </div>
  `
})

const AccountGroupsCellStub = defineComponent({
  props: { groups: { type: Array, default: () => [] } },
  template: '<span data-test="account-groups">{{ groups.map(group => group.name).join(",") }}</span>'
})

const EditAccountModalStub = defineComponent({
  props: { show: Boolean, account: { type: Object, default: null } },
  template: '<div data-test="edit-account">{{ show ? account?.name : "" }}</div>'
})

const AccountTestModalStub = defineComponent({
  props: { show: Boolean, account: { type: Object, default: null } },
  template: '<div data-test="test-account">{{ show ? account?.name : "" }}</div>'
})

const AccountStatsModalStub = defineComponent({
  props: { show: Boolean, account: { type: Object, default: null } },
  template: '<div data-test="stats-account">{{ show ? account?.name : "" }}</div>'
})

function mountView(stubActionMenu = true, renderQuota = false) {
  return mount(AccountsView, {
    attachTo: document.body,
    global: {
      stubs: {
        AppLayout: { template: '<div><slot /></div>' },
        TablePageLayout: { template: '<div><slot name="filters" /><slot name="table" /><slot name="pagination" /></div>' },
        DataTable: DataTableStub,
        AccountTableActions: { template: '<div><slot name="after" /></div>' },
        AccountTableFilters: true,
        AccountBulkActionsBar: true,
        Pagination: true,
        ConfirmDialog: true,
        AccountActionMenu: stubActionMenu,
        ImportDataModal: true,
        ReAuthAccountModal: true,
        AccountTestModal: AccountTestModalStub,
        AccountStatsModal: AccountStatsModalStub,
        SyncFromCrsModal: true,
        TempUnschedStatusModal: true,
        ErrorPassthroughRulesModal: true,
        TLSFingerprintProfilesModal: true,
        CreateAccountModal: true,
        EditAccountModal: EditAccountModalStub,
        BulkEditAccountModal: true,
        PlatformTypeBadge: true,
        AccountCapacityCell: true,
        AccountStatusIndicator: true,
        AccountTodayStatsCell: true,
        AccountGroupsCell: AccountGroupsCellStub,
        AccountUsageCell: renderQuota ? OpenAIQuotaResetCell : true,
        UpstreamBillingRateCell: true,
        HelpTooltip: true,
        Icon: true,
        Teleport: stubActionMenu
      }
    }
  })
}

const listRow = {
  id: 42,
  name: 'compact row',
  platform: 'openai',
  type: 'oauth',
  status: 'active',
  schedulable: true,
  concurrency: 2,
  priority: 1,
  group_ids: [7],
  extra: {},
  credentials: {}
}

const fullAccount = {
  ...listRow,
  groups: [{ id: 7, name: 'codex', platform: 'openai' }],
  account_groups: [{ account_id: 42, group_id: 7 }],
  credentials: { api_key: 'redacted' },
  extra: { detail_only: true }
}

describe('admin AccountsView lite account list', () => {
  beforeEach(() => {
    localStorage.clear()
    listAccounts.mockReset().mockResolvedValue({ items: [listRow], total: 1, page: 1, page_size: 20, pages: 1 })
    listWithEtag.mockReset().mockResolvedValue({ notModified: true, etag: 'compact-etag', data: null })
    getById.mockReset().mockResolvedValue(fullAccount)
    getBatchTodayStatsWithEtag.mockReset().mockResolvedValue({ notModified: false, etag: null, data: { stats: {} } })
    getUpstreamBillingProbeSettings.mockReset().mockResolvedValue({ enabled: true })
    getAllProxies.mockReset().mockResolvedValue([])
    getAllGroups.mockReset().mockResolvedValue([{ id: 7, name: 'codex', platform: 'openai' }])
    refreshCredentials.mockReset()
    showError.mockReset()
    showWarning.mockReset()
  })

  afterEach(() => {
    vi.useRealTimers()
    vi.restoreAllMocks()
  })

  it('keeps lite=1 on the initial list request', async () => {
    const wrapper = mountView()
    await flushPromises()

    expect(listAccounts).toHaveBeenCalledWith(
      1,
      20,
      expect.objectContaining({ lite: '1' }),
      expect.objectContaining({ signal: expect.any(AbortSignal) })
    )
    wrapper.unmount()
  })

  it('maps group_ids through the group catalog for the table cell', async () => {
    const wrapper = mountView()
    await flushPromises()

    expect(wrapper.get('[data-test="account-groups"]').text()).toBe('codex')
    wrapper.unmount()
  })

  it('keeps the action menu open during internal scrolling but closes it on table scrolling', async () => {
    const wrapper = mountView(false)
    await flushPromises()

    const trigger = wrapper.findAll('button').find(button => button.text() === 'common.more')!
    await trigger.trigger('click')
    const menu = new DOMWrapper(document.body.querySelector('.action-menu-content')!)
    menu.element.dispatchEvent(new Event('scroll'))
    await flushPromises()
    expect(wrapper.findComponent(AccountActionMenu).props('show')).toBe(true)

    menu.get('button').element.dispatchEvent(new Event('scroll'))
    await flushPromises()
    expect(wrapper.findComponent(AccountActionMenu).props('show')).toBe(true)

    wrapper.getComponent(DataTableStub).element.dispatchEvent(new Event('scroll'))
    await flushPromises()
    expect(wrapper.findComponent(AccountActionMenu).props('show')).toBe(false)
    wrapper.unmount()
  })

  it('keeps lite=1 on automatic ETag refreshes', async () => {
    vi.useFakeTimers()
    vi.spyOn(document, 'hidden', 'get').mockReturnValue(false)
    localStorage.setItem('account-auto-refresh', JSON.stringify({ enabled: true, interval_seconds: 5 }))
    const wrapper = mountView()
    await flushPromises()

    await vi.advanceTimersByTimeAsync(6000)
    await flushPromises()

    expect(listWithEtag).toHaveBeenCalledWith(
      1,
      20,
      expect.objectContaining({ lite: '1' }),
      expect.objectContaining({ etag: null })
    )
    wrapper.unmount()
  })

  it('reads only the visible GPT credit snapshots every five minutes with list auto-refresh off', async () => {
    vi.useFakeTimers()
    vi.spyOn(document, 'hidden', 'get').mockReturnValue(false)
    const other = { ...listRow, id: 43, platform: 'anthropic', type: 'apikey' }
    const page = { page: 1, page_size: 20, total: 2, pages: 1 }
    listAccounts.mockResolvedValue({ ...page, items: [listRow, other] })
    const snapshots = {
      codex_reset_credit_snapshot: { available_count: 0, credits: [], fetched_at: 100 },
      codex_credits_snapshot: { credits: { has_credits: true, unlimited: false, balance: '123' }, fetched_at: 100 }
    }
    listWithEtag.mockResolvedValueOnce({
      notModified: false, etag: 'quota-1',
      data: { ...page, items: [
        { ...other, name: 'unrelated update' },
        { ...listRow, name: 'another update', extra: { ...snapshots, unrelated: true } }
      ] }
    })
    const wrapper = mountView(true, true)
    await flushPromises()
    getBatchTodayStatsWithEtag.mockClear()
    expect(wrapper.get('[data-testid="codex-credits"] span').text()).toBe('—')
    await vi.advanceTimersByTimeAsync(299_999)
    expect(listWithEtag).not.toHaveBeenCalled()
    await vi.advanceTimersByTimeAsync(1)
    expect(listWithEtag).toHaveBeenCalledTimes(1)
    expect(wrapper.getComponent(DataTableStub).props('data')).toEqual([{ ...listRow, extra: snapshots }, other])
    expect(wrapper.get('[data-testid="reset-credit-count"]').text()).toContain('count0')
    expect(wrapper.get('[data-testid="codex-credits"] span').text()).toBe('123')
    await vi.advanceTimersByTimeAsync(300_000)
    expect(listWithEtag).toHaveBeenLastCalledWith(1, 20, expect.objectContaining({ lite: '1' }), expect.objectContaining({ etag: 'quota-1' }))
    expect(getBatchTodayStatsWithEtag).not.toHaveBeenCalled()
    expect(listAccounts).toHaveBeenCalledTimes(1)
    wrapper.unmount()
  })

  it.each([
    { type: 'apikey', hiddenColumns: [] },
    { type: 'oauth', hiddenColumns: ['usage'] }
  ])('skips the five-minute poll for $type rows with hidden columns $hiddenColumns', async ({ type, hiddenColumns }) => {
    vi.useFakeTimers()
    vi.spyOn(document, 'hidden', 'get').mockReturnValue(false)
    localStorage.setItem('account-hidden-columns', JSON.stringify(hiddenColumns))
    listAccounts.mockResolvedValue({ items: [{ ...listRow, type }], total: 1, pages: 1 })
    const wrapper = mountView()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(300_000)
    expect(listWithEtag).not.toHaveBeenCalled()
    wrapper.unmount()
  })

  it('uses the full list timer once auto-refresh is enabled and resets the partial-list ETag', async () => {
    vi.useFakeTimers()
    vi.spyOn(document, 'hidden', 'get').mockReturnValue(false)
    localStorage.setItem('account-auto-refresh', JSON.stringify({ enabled: false, interval_seconds: 10 }))
    const wrapper = mountView()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(300_000)
    expect(listWithEtag).toHaveBeenCalledTimes(1)
    await wrapper.get('button[title="admin.accounts.autoRefresh"]').trigger('click')
    await wrapper.findAll('button').find(button => button.text() === 'admin.accounts.enableAutoRefresh')!.trigger('click')
    document.body.click()
    const extra = { codex_reset_credit_snapshot: { available_count: 0, fetched_at: 101 } }
    listWithEtag.mockResolvedValueOnce({ notModified: false, etag: 'full', data: {
      items: [{ ...listRow, extra }], total: 1, pages: 1
    } })
    await vi.advanceTimersByTimeAsync(11_000)
    expect(listWithEtag).toHaveBeenCalledTimes(2)
    expect(listWithEtag).toHaveBeenLastCalledWith(1, 20, expect.anything(), expect.objectContaining({ etag: null }))
    expect(wrapper.getComponent(DataTableStub).props('data')).toMatchObject([{ extra }])
    await vi.advanceTimersByTimeAsync(288_999)
    const beforeFiveMinutes = listWithEtag.mock.calls.length
    await vi.advanceTimersByTimeAsync(1)
    expect(listWithEtag).toHaveBeenCalledTimes(beforeFiveMinutes)
    wrapper.unmount()
  })

  it('keeps the credit poll idle while the tab is hidden or an account editor is open', async () => {
    vi.useFakeTimers()
    const hidden = vi.spyOn(document, 'hidden', 'get').mockReturnValue(true)
    const wrapper = mountView()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(300_000)
    hidden.mockReturnValue(false)
    await wrapper.findAll('button').find(button => button.text().includes('common.edit'))!.trigger('click')
    await flushPromises()
    await vi.advanceTimersByTimeAsync(300_000)
    expect(listWithEtag).not.toHaveBeenCalled()
    wrapper.getComponent(EditAccountModalStub).vm.$emit('close')
    await vi.advanceTimersByTimeAsync(300_000)
    expect(listWithEtag).toHaveBeenCalledTimes(1)
    wrapper.unmount()
    await vi.advanceTimersByTimeAsync(300_000)
    expect(listWithEtag).toHaveBeenCalledTimes(1)
  })

  it('discards a credit snapshot response after a filter change, even when both lists contain the same row', async () => {
    vi.useFakeTimers()
    vi.spyOn(document, 'hidden', 'get').mockReturnValue(false)
    let finish!: (value: unknown) => void
    listWithEtag.mockReturnValueOnce(new Promise(resolve => { finish = resolve }))
    const wrapper = mountView()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(300_000)
    const signal = listWithEtag.mock.calls[0][3].signal
    const filters = wrapper.getComponent({ name: 'AccountTableFilters' })
    filters.vm.$emit('update:filters', { platform: 'openai' })
    filters.vm.$emit('change')
    expect(signal.aborted).toBe(true)
    const snapshot = { available_count: 0, credits: [], fetched_at: 10 }
    listAccounts.mockResolvedValue({ items: [{ ...listRow, extra: { codex_reset_credit_snapshot: snapshot } }], total: 1, pages: 1 })
    await vi.advanceTimersByTimeAsync(300)
    finish({ notModified: false, etag: 'stale', data: { items: [listRow] } })
    await flushPromises()
    expect(wrapper.getComponent(DataTableStub).props('data')).toMatchObject([{ extra: { codex_reset_credit_snapshot: snapshot } }])
    wrapper.unmount()
  })

  it('loads the full account by id before opening edit, test, and stats actions', async () => {
    const wrapper = mountView()
    await flushPromises()

    const editButton = wrapper.findAll('button').find(button => button.text().includes('common.edit'))
    expect(editButton).toBeTruthy()
    await editButton!.trigger('click')
    await flushPromises()
    expect(getById).toHaveBeenCalledWith(42)
    expect(wrapper.get('[data-test="edit-account"]').text()).toBe('compact row')

    const menu = wrapper.findComponent(AccountActionMenu)
    menu.vm.$emit('test', listRow)
    await flushPromises()
    expect(getById).toHaveBeenCalledTimes(2)
    expect(wrapper.get('[data-test="test-account"]').text()).toBe('compact row')

    menu.vm.$emit('stats', listRow)
    await flushPromises()
    expect(getById).toHaveBeenCalledTimes(3)
    expect(wrapper.get('[data-test="stats-account"]').text()).toBe('compact row')
    wrapper.unmount()
  })

  it('shows the warning and patches the account after a partial Antigravity refresh', async () => {
    refreshCredentials.mockResolvedValue({
      account: { ...fullAccount, name: 'refreshed account' },
      message: 'Token refreshed, but project_id is temporarily unavailable',
      warning: 'missing_project_id_temporary'
    })
    const wrapper = mountView(false)
    await flushPromises()

    wrapper.findComponent(AccountActionMenu).vm.$emit('refresh-token', listRow)
    await flushPromises()

    expect(refreshCredentials).toHaveBeenCalledWith(42)
    expect(wrapper.get('[data-account-name]').attributes('data-account-name')).toBe('refreshed account')
    expect(showWarning).toHaveBeenCalledWith('Token refreshed, but project_id is temporarily unavailable')
    wrapper.unmount()
  })

  it('shows an error and keeps the modal closed when detail loading fails', async () => {
    const consoleError = vi.spyOn(console, 'error').mockImplementation(() => {})
    getById.mockRejectedValueOnce(new Error('detail failed'))
    const wrapper = mountView()
    await flushPromises()

    const editButton = wrapper.findAll('button').find(button => button.text().includes('common.edit'))
    await editButton!.trigger('click')
    await flushPromises()

    expect(showError).toHaveBeenCalledWith('detail failed')
    expect(wrapper.find('[data-test="edit-account"]').exists()).toBe(false)
    consoleError.mockRestore()
    wrapper.unmount()
  })
})
