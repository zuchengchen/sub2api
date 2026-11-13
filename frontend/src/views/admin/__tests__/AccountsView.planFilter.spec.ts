import { afterEach, beforeEach, describe, expect, it, vi } from 'vitest'
import { flushPromises, mount, type VueWrapper } from '@vue/test-utils'

import AccountsView from '../AccountsView.vue'
import AccountActionMenu from '@/components/admin/account/AccountActionMenu.vue'

const api = vi.hoisted(() => ({
  list: vi.fn(), listWithEtag: vi.fn(), getUpstreamBillingRatesWithEtag: vi.fn(),
  getBatchTodayStatsWithEtag: vi.fn(), getUpstreamBillingProbeSettings: vi.fn(),
  refreshCredentials: vi.fn(), probeUpstreamBillingBatch: vi.fn(), exportData: vi.fn(), getAll: vi.fn()
}))

vi.mock('@/api/admin', () => ({
  adminAPI: { accounts: api, proxies: { getAll: api.getAll }, groups: { getAll: api.getAll } }
}))
vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ showError: vi.fn(), showSuccess: vi.fn(), showInfo: vi.fn() })
}))
vi.mock('@/stores/auth', () => ({ useAuthStore: () => ({ token: 'test-token' }) }))
vi.mock('vue-i18n', async () => ({
  ...await vi.importActual<typeof import('vue-i18n')>('vue-i18n'),
  useI18n: () => ({ t: (key: string) => key })
}))

const account = {
  id: 42, name: 'Pro account', platform: 'openai', type: 'oauth',
  status: 'active', schedulable: true, credentials: { plan_type: 'pro' }, group_ids: [], extra: {}
}
const wrappers: VueWrapper[] = []
const planSelect = 'select[aria-label="admin.accounts.openaiSubscriptionType"]'

function mountView() {
  const wrapper = mount(AccountsView, {
    global: {
      stubs: {
        AppLayout: { template: '<div><slot /></div>' },
        TablePageLayout: { template: '<div><slot name="filters" /><slot name="table" /><slot name="pagination" /></div>' },
        Select: {
          props: ['modelValue', 'options', 'ariaLabel'],
          emits: ['update:modelValue', 'change'],
          template: `<select :aria-label="ariaLabel || options[0].label" :value="modelValue"
            @change="$emit('update:modelValue', $event.target.value); $emit('change')">
            <option v-for="option in options" :key="option.value" :value="option.value">{{ option.label }}</option>
          </select>`
        },
        SearchInput: true,
        DataTable: {
          props: ['data'],
          template: '<div><span v-for="row in data" :key="row.id" data-test="row">{{ row.name }}</span></div>'
        },
        AccountTableActions: { template: '<div><slot name="after" /></div>' },
        AccountBulkActionsBar: {
          props: ['selectedIds'], emits: ['select-page', 'select-all-results', 'edit-filtered', 'clear', 'probe-upstream-billing'],
          template: `<div>
            <span data-test="selected-count">{{ selectedIds.length }}</span>
            <button data-test="select-page" @click="$emit('select-page')">select page</button>
            <button data-test="select-all" @click="$emit('select-all-results')">select all</button>
            <button data-test="edit-filtered" @click="$emit('edit-filtered')">edit filtered</button>
            <button data-test="clear-selection" @click="$emit('clear')">clear selection</button>
            <button data-test="probe-billing" @click="$emit('probe-upstream-billing')">probe billing</button>
          </div>`
        },
        Pagination: {
          props: ['page'], emits: ['update:page'],
          template: '<button data-test="next-page" @click="$emit(\'update:page\', 2)">{{ page }}</button>'
        },
        BulkEditAccountModal: {
          props: ['target'],
          template: '<div data-test="bulk-target">{{ JSON.stringify(target) }}</div>'
        },
        ConfirmDialog: {
          props: ['show', 'title'], emits: ['confirm'],
          template: '<button v-if="show" :data-dialog="title" @click="$emit(\'confirm\')">confirm</button>'
        },
        AccountActionMenu: true, ImportDataModal: true, ReAuthAccountModal: true,
        AccountTestModal: true, AccountStatsModal: true, SyncFromCrsModal: true,
        TempUnschedStatusModal: true, ErrorPassthroughRulesModal: true,
        TLSFingerprintProfilesModal: true, CreateAccountModal: true, EditAccountModal: true,
        PlatformTypeBadge: true, AccountCapacityCell: true, AccountStatusIndicator: true,
        AccountTodayStatsCell: true, AccountGroupsCell: true, AccountUsageCell: true,
        UpstreamBillingRateCell: true, TotpStepUpDialog: true, HelpTooltip: true, Icon: true,
        Teleport: true
      }
    }
  })
  wrappers.push(wrapper)
  return wrapper
}

describe('account subscription plan filtering', () => {
  beforeEach(() => {
    vi.useFakeTimers()
    vi.clearAllMocks()
    localStorage.clear()
    vi.spyOn(document, 'hidden', 'get').mockReturnValue(false)
    api.list.mockResolvedValue({ items: [account], total: 1, page: 1, page_size: 20, pages: 1 })
    api.listWithEtag.mockResolvedValue({ notModified: true, etag: 'plan-etag', data: null })
    api.getUpstreamBillingRatesWithEtag.mockResolvedValue({ notModified: true, etag: 'rate-etag', data: null })
    api.getBatchTodayStatsWithEtag.mockResolvedValue({ notModified: false, etag: null, data: { stats: {} } })
    api.getUpstreamBillingProbeSettings.mockResolvedValue({ enabled: true })
    api.getAll.mockResolvedValue([])
  })

  afterEach(() => {
    wrappers.splice(0).forEach(wrapper => wrapper.unmount())
    vi.useRealTimers()
    vi.restoreAllMocks()
    vi.unstubAllGlobals()
  })

  it('offers the requested tiers and resets pagination and selection when a plan changes', async () => {
    api.list.mockResolvedValue({ items: [account], total: 21, page: 1, page_size: 20, pages: 2 })
    const wrapper = mountView()
    await flushPromises()
    expect(wrapper.get(planSelect).findAll('option').map(option => option.text())).toEqual([
      'admin.accounts.allSubscriptionTypes', 'Plus', 'Pro 100', 'Pro 200', 'Pro 500', 'Free'
    ])
    await wrapper.get('[data-test="next-page"]').trigger('click')
    await flushPromises()
    await wrapper.get('[data-test="select-page"]').trigger('click')
    expect(wrapper.get('[data-test="selected-count"]').text()).toBe('1')

    await wrapper.get(planSelect).setValue('prolite')
    await vi.advanceTimersByTimeAsync(350)
    await flushPromises()

    expect(api.list).toHaveBeenLastCalledWith(1, 20,
      expect.objectContaining({ plan_type: 'prolite', lite: '1' }), expect.any(Object))
    expect(wrapper.get('[data-test="selected-count"]').text()).toBe('0')
    expect(JSON.parse(localStorage.getItem('account-filter-values')!)).toMatchObject({ plan_type: 'prolite' })

    await wrapper.get(planSelect).setValue('')
    await vi.advanceTimersByTimeAsync(350)
    expect(api.list).toHaveBeenLastCalledWith(1, 20,
      expect.objectContaining({ plan_type: '' }), expect.any(Object))
  })

  it.each([
    { platform: '', saved: 'Pro_200', expected: 'pro' },
    { platform: 'openai', saved: 'pro500', expected: 'promax' },
    { platform: 'openai', saved: 'unrecognized', expected: '' },
    { platform: 'grok', saved: 'pro', expected: '' }
  ])('restores only compatible plan filters: $platform / $saved', async ({ platform, saved, expected }) => {
    localStorage.setItem('account-filter-values', JSON.stringify({ platform, plan_type: saved }))
    const wrapper = mountView()
    await flushPromises()
    expect(api.list).toHaveBeenCalledWith(1, 20,
      expect.objectContaining({ platform, plan_type: expected }), expect.any(Object))
    expect(wrapper.find(planSelect).exists()).toBe(platform !== 'grok')
  })

  it('clears the OpenAI plan when changing platforms and does not restore the hidden filter', async () => {
    localStorage.setItem('account-filter-values', JSON.stringify({ plan_type: 'pro' }))
    const wrapper = mountView()
    await flushPromises()
    const platformSelect = wrapper.get('select[aria-label="admin.accounts.allPlatforms"]')
    await platformSelect.setValue('grok')
    await vi.advanceTimersByTimeAsync(350)
    expect(wrapper.find(planSelect).exists()).toBe(false)
    expect(api.list).toHaveBeenLastCalledWith(1, 20,
      expect.objectContaining({ platform: 'grok', plan_type: '' }), expect.any(Object))
    await platformSelect.setValue('')
    expect((wrapper.get(planSelect).element as HTMLSelectElement).value).toBe('')
  })

  it('keeps the selected plan in select-all and bulk-edit scopes', async () => {
    localStorage.setItem('account-filter-values', JSON.stringify({ plan_type: 'pro' }))
    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('[data-test="select-all"]').trigger('click')
    await flushPromises()
    expect(api.list).toHaveBeenLastCalledWith(1, 1000,
      expect.objectContaining({ plan_type: 'pro', lite: '1' }))
    expect(wrapper.get('[data-test="selected-count"]').text()).toBe('1')
    await wrapper.get('[data-test="edit-filtered"]').trigger('click')
    await flushPromises()
    expect(api.list).toHaveBeenLastCalledWith(1, 100, expect.objectContaining({ plan_type: 'pro' }))
    expect(JSON.parse(wrapper.get('[data-test="bulk-target"]').text())).toMatchObject({
      mode: 'filtered', filters: { plan_type: 'pro' }, previewCount: 1
    })
  })

  it('keeps plan filters on ETag and billing refreshes and clears the ETag after a change', async () => {
    localStorage.setItem('account-filter-values', JSON.stringify({ plan_type: 'pro' }))
    localStorage.setItem('account-auto-refresh', JSON.stringify({ enabled: true, interval_seconds: 5 }))
    const wrapper = mountView()
    await flushPromises()
    await vi.advanceTimersByTimeAsync(6000)
    expect(api.listWithEtag).toHaveBeenCalledWith(1, 20,
      expect.objectContaining({ plan_type: 'pro' }), { etag: null })

    api.list.mockResolvedValue({
      items: [{ ...account, credentials: { plan_type: 'plus' } }], total: 1, pages: 1
    })
    await wrapper.get(planSelect).setValue('plus')
    await vi.advanceTimersByTimeAsync(6000)
    expect(api.listWithEtag).toHaveBeenLastCalledWith(1, 20,
      expect.objectContaining({ plan_type: 'plus' }), { etag: null })
    api.probeUpstreamBillingBatch.mockResolvedValue([{ account_id: 42, snapshot: { status: 'unsupported' } }])
    await wrapper.get('[data-test="select-page"]').trigger('click')
    await wrapper.get('[data-test="probe-billing"]').trigger('click')
    await flushPromises()
    expect(api.getUpstreamBillingRatesWithEtag).toHaveBeenCalledWith(1, 20,
      expect.objectContaining({ plan_type: 'plus' }), expect.objectContaining({ signal: expect.any(AbortSignal) }))
  })

  it.each(['CHATGPT_PRO', 'Pro 200', 'pro'])('keeps equivalent refreshed plan %s but removes changed plans', async plan => {
    localStorage.setItem('account-filter-values', JSON.stringify({ plan_type: 'pro' }))
    const wrapper = mountView()
    await flushPromises()
    const menu = wrapper.findComponent(AccountActionMenu)
    api.refreshCredentials.mockResolvedValue({ account: { ...account, credentials: { plan_type: plan } } })
    menu.vm.$emit('refresh-token', account)
    await flushPromises()
    expect(wrapper.findAll('[data-test="row"]')).toHaveLength(1)

    api.refreshCredentials.mockResolvedValue({ account: { ...account, credentials: { plan_type: 'free' } } })
    menu.vm.$emit('refresh-token', account)
    await flushPromises()
    expect(wrapper.findAll('[data-test="row"]')).toHaveLength(0)
  })

  it('exports only the active plan when no account selection overrides the filters', async () => {
    localStorage.setItem('account-filter-values', JSON.stringify({ plan_type: 'promax' }))
    api.exportData.mockResolvedValue({ accounts: [] })
    vi.stubGlobal('URL', class extends URL {
      static createObjectURL = vi.fn(() => 'blob:account-export')
      static revokeObjectURL = vi.fn()
    })
    vi.spyOn(HTMLAnchorElement.prototype, 'click').mockImplementation(() => {})
    const wrapper = mountView()
    await flushPromises()
    await wrapper.get('button[title="admin.accounts.moreActions"]').trigger('click')
    await wrapper.findAll('button').find(button => button.text() === 'admin.accounts.dataExport')!.trigger('click')
    await wrapper.get('[data-dialog="admin.accounts.dataExport"]').trigger('click')
    await flushPromises()
    expect(api.exportData).toHaveBeenCalledWith({
      includeProxies: true, filters: expect.objectContaining({ plan_type: 'promax' })
    })
  })
})
