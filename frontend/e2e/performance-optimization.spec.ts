import { readFileSync } from 'node:fs'
import { expect, test, type Page, type Route } from '@playwright/test'
import * as XLSX from 'xlsx'

const admin = { id: 1, username: 'admin', email: 'admin@example.test', role: 'admin', status: 'active', balance: 0, concurrency: 10, allowed_groups: null }
const row = (id: number, platform = 'openai', type = 'oauth') => ({
  id, name: `Perf account ${id}`, platform, type, status: 'active', schedulable: true,
  concurrency: 5, priority: 1, rate_multiplier: 1, credentials: {}, extra: {}, group_ids: [],
  created_at: '2026-10-30T00:00:00Z', updated_at: '2026-10-30T00:00:00Z',
  temp_unschedulable_until: null as string | null
})
const metric = { requests: 9, tokens: 123, cost: 0.4, standard_cost: 0.5, user_cost: 0.6 }

function json(route: Route, data: unknown, headers: Record<string, string> = {}) {
  return route.fulfill({ status: 200, headers, contentType: 'application/json', body: JSON.stringify({ code: 0, message: 'ok', data }) })
}

async function prepare(page: Page, initialRows = [row(1)]) {
  const state = {
    rows: initialRows, total: initialRows.length,
    today: [] as { ids: number[]; etag?: string; status: number }[],
    list304: 0, paths: [] as string[], modules: [] as string[], errors: [] as string[],
    revision: 'day-1', exportFailure: false, exportPending: false,
    releaseExport: null as (() => void) | null
  }
  page.on('pageerror', error => state.errors.push(error.message))
  page.on('request', request => { if (!request.url().includes('/api/')) state.modules.push(request.url()) })
  await page.addInitScript(({ admin }) => {
    localStorage.clear()
    localStorage.setItem('auth_token', 'local-performance-test')
    localStorage.setItem('auth_user', JSON.stringify(admin))
    localStorage.setItem('sub2api_locale', 'en')
    localStorage.setItem('admin_guide_1_admin_v4_interactive', 'true')
    localStorage.setItem('account-auto-refresh', JSON.stringify({ enabled: true, interval_seconds: 5 }))
    window.__APP_CONFIG__ = { site_name: 'Local Performance Test', version: 'test', backend_mode_enabled: false, custom_menu_items: [] } as typeof window.__APP_CONFIG__
  }, { admin })
  // Block any non-local resource. Every API request below is fulfilled locally.
  await page.route('**/*', route => {
    const url = new URL(route.request().url())
    return url.hostname === '127.0.0.1' ? route.continue() : route.abort()
  })
  await page.route('**/api/v1/**', async route => {
    const request = route.request()
    const url = new URL(request.url())
    const path = url.pathname
    state.paths.push(path)
    if (path.endsWith('/auth/me')) return json(route, { ...admin, run_mode: 'standard' })
    if (path.endsWith('/setup/status')) return json(route, { needs_setup: false })
    if (path.endsWith('/settings/public')) return json(route, { site_name: 'Local Performance Test', custom_menu_items: [] })
    if (path.endsWith('/admin/accounts/today-stats/batch')) {
      const ids = request.postDataJSON().account_ids as number[]
      const etag = request.headers()['if-none-match']
      const validator = `"${state.revision}:${ids.join(',')}"`
      const status = etag === validator ? 304 : 200
      state.today.push({ ids, etag, status })
      if (status === 304) return route.fulfill({ status, headers: { ETag: validator } })
      return json(route, { stats: Object.fromEntries(ids.map(id => [id, metric])) }, { ETag: validator })
    }
    if (path.endsWith('/admin/accounts/usage/batch')) return json(route, { usage: {}, errors: {} })
    if (path.endsWith('/admin/accounts/upstream-billing-probe-settings')) return json(route, { enabled: false })
    if (path.endsWith('/admin/accounts/upstream-billing-rates')) return json(route, { items: [] })
    if (path.endsWith('/admin/accounts')) {
      if (request.headers()['if-none-match'] === '"list"') {
        state.list304++
        return route.fulfill({ status: 304, headers: { ETag: '"list"' } })
      }
      return json(route, { items: state.rows, total: state.total, page: Number(url.searchParams.get('page') || 1), page_size: 20, pages: Math.ceil(state.total / 20) }, { ETag: '"list"' })
    }
    if (/\/admin\/accounts\/\d+$/.test(path)) return json(route, state.rows.find(account => account.id === Number(path.split('/').pop())) || row(1))
    if (path.endsWith('/stats') && path.includes('/accounts/')) return json(route, {
      account_id: 1, history: [], models: [], endpoints: [],
      summary: { total_cost: 12.34, total_user_cost: 15, total_standard_cost: 18, total_requests: 9, total_tokens: 123, avg_daily_cost: 1, avg_daily_user_cost: 1.5, avg_daily_requests: 1, avg_daily_tokens: 10, avg_duration_ms: 100, actual_days_used: 1, days: 30, today: metric }
    })
    if (path.endsWith('/models') && path.includes('/accounts/')) return json(route, [{ id: 'gpt-5', display_name: 'Local test model' }])
    if (path.endsWith('/temp-unschedulable')) return json(route, { active: false })
    if (path.endsWith('/admin/usage')) {
      if (url.searchParams.get('page_size') === '100') {
        if (state.exportFailure) return route.fulfill({ status: 500, contentType: 'application/json', body: JSON.stringify({ code: 500, message: 'mock export failure' }) })
        if (state.exportPending) await new Promise<void>(resolve => { state.releaseExport = resolve })
      }
      return json(route, { items: [{
        id: 1, created_at: '2026-10-30T12:00:00Z', user: { id: 2, email: 'export@example.test' }, api_key: { name: 'Local key' }, account: { name: 'Local account' }, model: 'local-model', input_tokens: 123, output_tokens: 45, cache_read_tokens: 0, cache_creation_tokens: 0, total_cost: 1.25, actual_cost: 1.5, duration_ms: 100, request_id: 'local-request', stream: false
      }], total: 1, page: 1, page_size: 20, pages: 1 })
    }
    if (path.endsWith('/admin/usage/stats')) return json(route, { total_requests: 1, total_input_tokens: 123, total_output_tokens: 45, total_tokens: 168, total_cost: 1.25, actual_cost: 1.5, avg_duration_ms: 100 })
    if (/\/all$|\/models$|\/tls-fingerprint-profiles$|\/error-passthrough|\/announcements$|\/subscriptions\/active$/.test(path)) return json(route, [])
    return json(route, {})
  })
  return state
}

async function closeDialog(page: Page) {
  await page.getByRole('dialog').getByRole('button', { name: 'Close modal', exact: true }).click()
  await expect(page.getByRole('dialog')).toHaveCount(0)
  await expect(page.locator('body')).not.toHaveCSS('overflow', 'hidden')
}
async function action(page: Page, label: string) {
  await page.getByRole('button', { name: 'More', exact: true }).first().click()
  await page.locator('.action-menu-content').getByRole('button', { name: label, exact: true }).click()
}
async function tool(page: Page, label: string) {
  await page.getByTitle('More Actions', { exact: true }).click()
  await page.getByRole('button', { name: label, exact: true }).click()
}
async function refresh(page: Page) {
  // The existing compact refresh button has no text; locate its neighbouring
  // Create Account action rather than reaching into Vue internals.
  await page.getByRole('button', { name: 'Create Account', exact: true }).locator('..').locator('button').first().click()
}

const modalCases = [
  { name: 'create', module: 'CreateAccountModal', open: (page: Page) => page.getByRole('button', { name: 'Create Account', exact: true }).click(), path: '/tls-fingerprint-profiles' },
  { name: 'edit', module: 'EditAccountModal', open: (page: Page) => page.getByRole('button', { name: 'Edit', exact: true }).first().click(), path: '/tls-fingerprint-profiles' },
  { name: 'stats', module: 'AccountStatsModal', open: (page: Page) => action(page, 'View Stats'), path: '/1/stats' },
  { name: 'test', module: 'AccountTestModal', open: (page: Page) => action(page, 'Test Connection'), path: '/1/models' },
  { name: 'reauth', module: 'ReAuthAccountModal', open: (page: Page) => action(page, 'Re-Authorize') },
  { name: 'sync', module: 'SyncFromCrsModal', open: (page: Page) => tool(page, 'Sync from CRS') },
  { name: 'import', module: 'ImportDataModal', open: (page: Page) => tool(page, 'Import') },
  { name: 'bulk edit', module: 'BulkEditAccountModal', open: (page: Page) => page.getByRole('button', { name: 'Update Accounts', exact: true }).click() },
  { name: 'error rules', module: 'ErrorPassthroughRulesModal', open: (page: Page) => tool(page, 'Error Passthrough Rules'), path: '/error-passthrough' },
  { name: 'temp status', module: 'TempUnschedStatusModal', open: (page: Page) => page.getByTitle('View temp unschedulable details', { exact: true }).click(), path: '/1/temp-unschedulable' },
  { name: 'TLS profiles', module: 'TLSFingerprintProfilesModal', open: (page: Page) => tool(page, 'TLS Fingerprint Profiles'), path: '/tls-fingerprint-profiles' }
]

for (const modal of modalCases) {
  test(`${modal.name}: lazy first open, cleanup and reopening`, async ({ page }) => {
    const account = modal.name === 'reauth' ? row(1, 'anthropic', 'setup-token') : row(1)
    if (modal.name === 'temp status') account.temp_unschedulable_until = '2099-01-01T00:00:00Z'
    const state = await prepare(page, [account])
    await page.goto('/admin/accounts')
    await expect(page.getByText('Perf account 1', { exact: true })).toBeVisible()
    expect(state.modules.some(url => url.includes(`${modal.module}.vue`))).toBe(false)
    expect(state.modules.some(url => /xlsx|AccountStatsModal\.vue|chart\.js/.test(url))).toBe(false)
    for (let opening = 0; opening < 2; opening++) {
      const previousCalls = modal.path ? state.paths.filter(path => path.includes(modal.path!)).length : 0
      await modal.open(page)
      await expect(page.getByRole('dialog')).toBeVisible()
      if (modal.path) await expect.poll(() => state.paths.filter(path => path.includes(modal.path!)).length).toBeGreaterThan(previousCalls)
      if (modal.name === 'create') {
        const name = page.locator('[data-tour="account-form-name"]')
        await expect(name).toHaveValue('')
        await name.fill('discard on close')
      }
      if (modal.name === 'edit') await expect(page.getByRole('dialog').locator('input[type="text"]').first()).toHaveValue('Perf account 1')
      if (modal.name === 'stats') await expect(page.getByRole('dialog')).toContainText('$12.34')
      if (modal.name === 'test') await expect(page.getByRole('dialog')).toContainText('Local test model')
      if (modal.name === 'reauth') await expect(page.getByRole('dialog').locator('input[value="setup-token"]')).toBeChecked()
      if (modal.name === 'import') {
        await expect(page.getByRole('dialog')).not.toContainText('local-import.json')
        await page.getByRole('dialog').locator('input[type="file"]').setInputFiles({ name: 'local-import.json', mimeType: 'application/json', buffer: Buffer.from('{}') })
        await expect(page.getByRole('dialog')).toContainText('local-import.json')
      }
      await closeDialog(page)
    }
    expect(state.modules.some(url => url.includes(`${modal.module}.vue`))).toBe(true)
    expect(state.errors).toEqual([])
  })
}

test('today stats: pure OAuth, mixed rows, columns, 304, pagination, filter, manual refresh and next day', async ({ page }) => {
  const state = await prepare(page)
  await page.clock.install({ time: new Date('2026-10-30T12:00:00Z') })
  await page.goto('/admin/accounts')
  await expect(page.getByText('Perf account 1', { exact: true })).toBeVisible()
  await page.clock.runFor(7000)
  await expect.poll(() => state.paths.filter(path => path.endsWith('/admin/accounts')).length).toBe(2)
  await page.clock.runFor(6000)
  await expect.poll(() => state.list304).toBe(1)
  expect(state.today).toEqual([])
  state.rows = [row(1), row(2, 'anthropic', 'apikey'), row(3, 'anthropic', 'bedrock'), row(4, 'openai', 'apikey')]
  state.total = 21
  await refresh(page)
  await expect.poll(() => state.today.length).toBe(1)
  expect(state.today[0].ids).toEqual([2, 3])
  await refresh(page)
  await expect.poll(() => state.today.length).toBe(2)
  expect(state.today[1]).toMatchObject({ ids: [2, 3], status: 304 })
  await tool(page, 'Today Stats')
  await expect.poll(() => state.today.length).toBe(3)
  expect(state.today[2]).toMatchObject({ ids: [1, 2, 3, 4], status: 200 })
  await page.getByTitle('More Actions', { exact: true }).click()
  state.rows = [row(5, 'anthropic', 'apikey')]
  await page.getByRole('button', { name: 'Next', exact: true }).filter({ visible: true }).click()
  await expect.poll(() => state.today.length).toBe(4)
  expect(state.today[3]).toMatchObject({ ids: [5], status: 200 })
  expect(state.today[3].etag).toBeUndefined()
  await page.getByPlaceholder('Search accounts...', { exact: true }).fill('Perf account 5')
  await page.clock.runFor(400)
  await expect.poll(() => state.today.length).toBe(5)
  expect(state.today[4].etag).toBeUndefined()
  await page.clock.setSystemTime(new Date('2026-10-31T00:00:01Z'))
  state.revision = 'day-2'
  await refresh(page)
  await expect.poll(() => state.today.length).toBe(6)
  expect(state.today[5].etag).toBeUndefined()
  // Both columns hidden removes every consumer, including subsequent refreshes.
  await tool(page, 'Today Stats')
  await page.getByRole('button', { name: 'Usage Windows', exact: true }).click()
  await page.getByTitle('More Actions', { exact: true }).click()
  const before = state.today.length
  await refresh(page)
  expect(state.today).toHaveLength(before)
  expect(state.errors).toEqual([])
})

test('Excel export loads XLSX on demand and downloads correct contents after failure/retry and cancellation', async ({ page }) => {
  const state = await prepare(page)
  await page.goto('/admin/usage')
  await expect(page.getByRole('button', { name: 'Export Excel', exact: true })).toBeVisible()
  expect(state.modules.some(url => /xlsx/.test(url))).toBe(false)
  state.exportFailure = true
  await page.getByRole('button', { name: 'Export Excel', exact: true }).click()
  await expect(page.getByText('Export Failed', { exact: true })).toBeVisible()
  state.exportFailure = false
  const download = page.waitForEvent('download')
  await page.getByRole('button', { name: 'Export Excel', exact: true }).click()
  const file = await download
  expect(file.suggestedFilename()).toMatch(/^usage_.*\.xlsx$/)
  const workbook = XLSX.read(readFileSync((await file.path())!), { type: 'buffer' })
  const records = XLSX.utils.sheet_to_json<unknown[]>(workbook.Sheets.Usage, { header: 1 })
  expect(records).toHaveLength(2)
  expect(records[1]).toEqual(expect.arrayContaining(['export@example.test', 'local-model', 123, 45, 'local-request']))
  expect(state.modules.some(url => /xlsx/.test(url))).toBe(true)
  let cancelledDownloads = 0
  page.on('download', () => cancelledDownloads++)
  state.exportPending = true
  await page.getByRole('button', { name: 'Export Excel', exact: true }).click()
  await expect.poll(() => state.releaseExport !== null).toBe(true)
  await page.getByRole('button', { name: /Cancel/, exact: false }).filter({ visible: true }).click()
  state.releaseExport!()
  await expect(page.getByRole('button', { name: 'Export Excel', exact: true })).toBeEnabled()
  expect(cancelledDownloads).toBe(0)
  expect(state.errors).toEqual([])
})
