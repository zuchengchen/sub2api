import { mkdirSync } from 'node:fs'
import { resolve } from 'node:path'
import { expect, test, type Page, type Route } from '@playwright/test'

const adminUser = {
  id: 1,
  username: 'admin',
  email: 'admin@example.test',
  role: 'admin',
  balance: 0,
  concurrency: 10,
  status: 'active',
  allowed_groups: null,
  balance_notify_enabled: false,
  balance_notify_threshold: null,
  balance_notify_extra_emails: [],
  created_at: '2026-08-17T00:00:00Z',
  updated_at: '2026-08-17T00:00:00Z',
}

const riskConfig = {
  enabled: true,
  mode: 'pre_block',
  deepseek_enabled: true,
  yufeng_enabled: false,
  deepseek_total_timeout_ms: 10000,
  deepseek_threshold: 0.8,
  policy_version: 'deepseek-v4-flash-audit-v1',
  deepseek_channels: [
    {
      id: 'deepseek-official',
      name: 'DeepSeek 官方',
      base_url: 'https://api.deepseek.com',
      model: 'deepseek-v4-flash',
      enabled: true,
      order: 0,
      timeout_ms: 3000,
      api_key_configured: true,
      api_key_masked: 'sk-...a123',
      health_status: 'reachable',
      last_health_checked_at: '2026-08-17T00:00:00Z',
      breaker_status: 'closed',
      last_latency_ms: 912,
    },
    {
      id: 'deepseek-backup',
      name: '备用渠道',
      base_url: 'https://backup.example.test/v1',
      model: 'deepseek-v4-flash',
      enabled: true,
      order: 1,
      timeout_ms: 3000,
      api_key_configured: true,
      api_key_masked: 'sk-...b456',
      health_status: 'unhealthy',
      breaker_status: 'cooldown',
      cooldown_until: '2099-08-17T00:01:00Z',
      last_latency_ms: 3000,
      last_error: 'timeout',
    },
  ],
  all_groups: true,
  group_ids: [],
  user_email_whitelist: [],
  record_non_hits: false,
  block_status: 403,
  block_message: '请调整输入后重试',
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
  layer1_keywords: ['窃取他人账号密码'],
  layer2_keywords: ['账号密码'],
}

const riskLog = {
  id: 11,
  created_at: '2026-08-17T00:00:00Z',
  request_id: 'req-11',
  user_email: 'user@example.test',
  group_name: '生产组',
  provider: 'deepseek',
  model: 'gpt-5.5',
  action: 'second_layer_shadow',
  decision_source: 'deepseek_primary',
  keyword_tier: 'layer2',
  flagged: true,
  input_excerpt: '[已脱敏证据]',
  upstream_latency_ms: 1280,
  deepseek_confidence: 0.91,
  deepseek_category: 'cyber_abuse',
  deepseek_reason: '明确攻击意图',
  review_outcome: 'shadow_risk',
  reviewer_disagreement: false,
  review_attempts: [
    { channel_id: 'deepseek-official', channel_name: 'DeepSeek 官方', outcome: 'risk', latency_ms: 1280 },
  ],
}

function json(route: Route, body: unknown) {
  return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(body) })
}

async function mockAPIs(page: Page) {
  await page.addInitScript(
    ({ user }) => {
      localStorage.setItem('auth_token', 'browser-test-token')
      localStorage.setItem('auth_user', JSON.stringify(user))
      localStorage.setItem('sub2api_locale', 'zh')
      localStorage.setItem('admin_guide_1_admin_v4_interactive', 'true')
      Object.defineProperty(window, '__APP_CONFIG__', {
        configurable: true,
        writable: true,
        value: {
          site_name: 'Sub2API',
          site_logo: '',
          version: 'test',
          risk_control_enabled: true,
          backend_mode_enabled: false,
          custom_menu_items: [],
          payment_enabled: false,
          channel_monitor_enabled: false,
          affiliate_enabled: false,
        },
      })
    },
    { user: adminUser }
  )

  await page.route('**/api/v1/**', async (route) => {
    const request = route.request()
    const path = new URL(request.url()).pathname

    if (path.endsWith('/auth/me')) return json(route, { ...adminUser, run_mode: 'standard' })
    if (path.endsWith('/setup/status')) return json(route, { needs_setup: false })
    if (path.endsWith('/admin/risk-control/config')) return json(route, riskConfig)
    if (path.endsWith('/admin/risk-control/status')) {
      return json(route, {
        enabled: true,
        risk_control_enabled: true,
        mode: 'pre_block',
        pre_block_checked: 248,
        deepseek_failover_count: 7,
        deepseek_unavailable_count: 1,
        second_layer_enforce_ready: false,
        second_layer_enforce_reason: '请先完成渠道连通性检查',
      })
    }
    if (path.endsWith('/admin/risk-control/logs')) {
      return json(route, { items: [riskLog], total: 1, page: 1, page_size: 20, pages: 1 })
    }
    if (path.endsWith('/admin/groups/all')) return json(route, [])
    if (path.includes('/admin/risk-control/deepseek/channels/') && path.endsWith('/test')) {
      return json(route, {
        channel_id: 'deepseek-official',
        reachable: true,
        health_valid: true,
        latency_ms: 18,
        http_status: 404,
        checked_at: '2026-08-17T00:01:00Z',
      })
    }
    if (path.endsWith('/announcements') || path.endsWith('/subscriptions/active')) return json(route, [])
    return json(route, {})
  })
}

test('风控中心在桌面与移动视口完整呈现', async ({ page }, testInfo) => {
  await mockAPIs(page)
  await page.goto('/admin/risk-control')

  await expect(page.locator('[data-test="risk-control-view"]')).toBeVisible()
  await expect(page.locator('[data-test="risk-overview"]')).not.toContainText('admin.riskControl.')
  await expect(page.locator('[data-test="deepseek-enabled"]')).toHaveAttribute('aria-checked', 'true')
  await expect(page.locator('[data-test="yufeng-enabled"]')).toHaveAttribute('aria-checked', 'false')
  await expect(page.locator('[data-test="deepseek-channel-0"]')).toContainText('DeepSeek 官方')
  await expect(page.locator('[data-test="deepseek-channel-1"]')).toContainText('熔断中')
  await expect(page.locator('[data-test="deepseek-channel-key-0"]')).toHaveAttribute('type', 'password')
  await expect(page.locator('[data-test="deepseek-channel-key-0"]')).toHaveAttribute('placeholder', 'sk-...a123')
  await expect(page.locator('[data-test="deepseek-channel-key-0"]')).toHaveValue('')
  await expect(page.locator('[data-test="layer1-stage-shadow"]')).toBeVisible()
  await expect(page.locator('[data-test="layer2-stage-shadow"]')).toBeVisible()
  await expect(page.locator('[data-test="layer2-stage-enforce"]')).toBeDisabled()
  await expect(page.locator('[data-test="enforce-health-gate"]')).toContainText('请先完成渠道连通性检查')
  await expect(page.locator('[data-test="audit-log-table"]')).toContainText('cyber_abuse')

  await page.locator('[data-test="deepseek-channel-move-up-1"]').click()
  await expect(page.locator('[data-test="deepseek-channel-0"]')).toContainText('备用渠道')

  const filteredLogs = page.waitForRequest((request) => {
    const url = new URL(request.url())
    return url.pathname.endsWith('/admin/risk-control/logs') && url.searchParams.get('result') === 'review_unavailable'
  })
  await page.locator('[data-test="record-tab-review_unavailable"]').click()
  await filteredLogs

  if (testInfo.project.name.startsWith('mobile')) {
    await page.getByRole('button', { name: '切换菜单' }).click()
  }
  const riskControlLink = page.getByRole('link', { name: '风控中心' })
  await expect(riskControlLink).toBeVisible()
  if (testInfo.project.name.startsWith('mobile')) {
    await riskControlLink.click()
  }

  const hasDocumentOverflow = await page.evaluate(() => document.documentElement.scrollWidth > window.innerWidth + 1)
  expect(hasDocumentOverflow).toBe(false)
  await expect(page.locator('[data-test="risk-control-view"] .card .card')).toHaveCount(0)

  await page.evaluate(() => window.scrollTo(0, 0))

  const screenshotDir = process.env.RISK_CONTROL_SCREENSHOT_DIR || '/tmp/sub2api-risk-control-screenshots'
  mkdirSync(screenshotDir, { recursive: true })
  await page.screenshot({
    path: resolve(screenshotDir, `${testInfo.project.name}.png`),
    fullPage: true,
  })
})
