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
  created_at: '2026-08-30T00:00:00Z',
  updated_at: '2026-08-30T00:00:00Z',
}

const originalGuide = {
  content: '## 当前教程\n\n这是管理员之前发布的内容。',
  version: 3,
  updated_at: '2026-08-30T10:00:00Z',
  has_custom_content: true,
  revisions: [{
    content: '## 当前教程\n\n这是管理员之前发布的内容。',
    version: 3,
    updated_at: '2026-08-30T10:00:00Z',
  }],
}

function json(route: Route, body: unknown) {
  return route.fulfill({ status: 200, contentType: 'application/json', body: JSON.stringify(body) })
}

async function prepareAdminGuide(page: Page) {
  await page.addInitScript(({ user }) => {
    localStorage.clear()
    localStorage.setItem('auth_token', 'browser-test-token')
    localStorage.setItem('auth_user', JSON.stringify(user))
    localStorage.setItem('sub2api_locale', 'zh')
    localStorage.setItem('theme', 'light')
    localStorage.setItem('admin_guide_1_admin_v4_interactive', 'true')
    Object.defineProperty(window, '__APP_CONFIG__', {
      configurable: true,
      writable: true,
      value: {
        site_name: 'Sub2API',
        site_logo: '',
        version: 'test',
        backend_mode_enabled: false,
        custom_menu_items: [],
        payment_enabled: false,
        channel_monitor_enabled: false,
        affiliate_enabled: false,
      },
    })
  }, { user: adminUser })

  await page.route('**/api/v1/**', async (route) => {
    const request = route.request()
    const path = new URL(request.url()).pathname

    if (path.endsWith('/auth/me')) return json(route, { ...adminUser, run_mode: 'standard' })
    if (path.endsWith('/setup/status')) return json(route, { needs_setup: false })
    if (path.endsWith('/admin/settings/guide')) {
      if (request.method() === 'PUT') {
        const payload = request.postDataJSON() as { content: string; expected_version: number }
        expect(payload.expected_version).toBe(3)
        return json(route, {
          ...originalGuide,
          content: payload.content,
          version: 4,
          updated_at: '2026-08-30T11:00:00Z',
          revisions: [
            ...originalGuide.revisions,
            {
              content: payload.content,
              version: 4,
              updated_at: '2026-08-30T11:00:00Z',
            },
          ],
        })
      }
      return json(route, originalGuide)
    }
    if (path.endsWith('/admin/settings')) return json(route, {})
    if (path.endsWith('/admin/settings/beta-policy')) return json(route, { rules: [] })
    if (path.endsWith('/admin/groups/all') || path.includes('/payment/providers')) {
      return json(route, [])
    }
    if (path.endsWith('/admin/proxies')) return json(route, { items: [] })
    if (path.endsWith('/admin/backups/s3-config')) {
      return json(route, {
        endpoint: '',
        region: 'auto',
        bucket: '',
        access_key_id: '',
        prefix: 'backups/',
        force_path_style: false,
      })
    }
    if (path.endsWith('/admin/backups/image-storage')) {
      return json(route, {
        config: {
          enabled: false,
          reuse_backup_s3: true,
          bucket: '',
          prefix: 'images/',
          public_base_url: '',
          presign_expiry_hours: 24,
          max_download_bytes: 0,
          endpoint: '',
          region: 'auto',
          access_key_id: '',
          force_path_style: false,
        },
        secret_configured: false,
      })
    }
    if (path.endsWith('/admin/backups/schedule')) {
      return json(route, { enabled: false, cron_expr: '0 2 * * *', retain_days: 30, retain_count: 10 })
    }
    if (path.endsWith('/admin/backups')) return json(route, { items: [] })
    if (path.endsWith('/announcements') || path.endsWith('/subscriptions/active')) {
      return json(route, [])
    }
    return json(route, {})
  })
}

test('管理员可以预览并发布使用教程', async ({ page }, testInfo) => {
  await prepareAdminGuide(page)
  await page.goto('/admin/settings')
  await page.waitForLoadState('networkidle')

  const closeNotifications = page.getByRole('button', { name: 'Close notification' })
  await closeNotifications.evaluateAll((buttons) => {
    buttons.forEach((button) => (button as HTMLButtonElement).click())
  })
  await expect(closeNotifications).toHaveCount(0)
  await page.locator('#settings-tab-guide').click()
  await expect(page.locator('[data-test="guide-editor"]')).toBeVisible()
  await expect(page.locator('[data-test="guide-markdown"]')).toHaveValue(/管理员之前发布的内容/)

  const updatedContent = '## 新手教程\n\n照着下面的步骤操作即可。\n\n<script>alert(1)</script>'
  await page.locator('[data-test="guide-markdown"]').fill(updatedContent)
  await expect(page.locator('[data-test="guide-preview"]')).toContainText('照着下面的步骤操作即可。')
  await expect(page.locator('[data-test="guide-preview"] script')).toHaveCount(0)

  await page.locator('[data-test="guide-save"]').click()
  await expect(page.getByText('当前版本：第 4 版')).toBeVisible()
  await expect(page.getByText('教程已发布，刷新 /guide 即可看到')).toBeVisible()

  const layout = await page.evaluate(() => ({
    documentWidth: document.documentElement.scrollWidth,
    viewportWidth: document.documentElement.clientWidth,
  }))
  expect(layout.documentWidth).toBeLessThanOrEqual(layout.viewportWidth + 1)

  const screenshotDir = '/tmp/sub2api-guide-admin-screenshots'
  mkdirSync(screenshotDir, { recursive: true })
  await page.locator('[data-test="guide-editor"]').evaluate((element) => {
    element.scrollIntoView({ block: 'start' })
  })
  await page.screenshot({
    path: resolve(screenshotDir, `${testInfo.project.name}-viewport.png`),
  })
  await page.screenshot({
    path: resolve(screenshotDir, `${testInfo.project.name}.png`),
    fullPage: true,
  })
})
