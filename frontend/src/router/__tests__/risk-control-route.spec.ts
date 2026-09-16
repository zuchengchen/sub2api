import { readFileSync } from 'node:fs'
import { dirname, resolve } from 'node:path'
import { fileURLToPath } from 'node:url'
import { describe, expect, it, vi } from 'vitest'

const authStore = vi.hoisted(() => ({
  checkAuth: vi.fn(),
  isAuthenticated: false,
  isAdmin: false,
  isSimpleMode: false,
}))

vi.mock('@/stores/auth', () => ({ useAuthStore: () => authStore }))
vi.mock('@/stores/app', () => ({
  useAppStore: () => ({ siteName: 'Sub2API', backendModeEnabled: false, cachedPublicSettings: null }),
}))
vi.mock('@/stores/adminSettings', () => ({
  useAdminSettingsStore: () => ({ customMenuItems: [] }),
}))
vi.mock('@/stores/adminCompliance', () => ({
  useAdminComplianceStore: () => ({ initialized: true, fetchStatus: vi.fn(), requireAcknowledgement: vi.fn() }),
}))
vi.mock('@/composables/useNavigationLoading', () => ({
  useNavigationLoadingState: () => ({
    startNavigation: vi.fn(),
    endNavigation: vi.fn(),
    isLoading: { value: false },
  }),
}))
vi.mock('@/composables/useRoutePrefetch', () => ({
  useRoutePrefetch: () => ({
    triggerPrefetch: vi.fn(),
    cancelPendingPrefetch: vi.fn(),
    resetPrefetchState: vi.fn(),
  }),
}))

describe('unified risk-control routes', () => {
  it('registers Risk Control and removes the legacy Prompt Audit route', async () => {
    const { default: router } = await import('@/router')

    expect(router.getRoutes().some((route) => route.path === '/admin/risk-control')).toBe(true)
    expect(router.getRoutes().some((route) => route.path === '/admin/prompt-audit')).toBe(false)
    expect(router.getRoutes().some((route) => route.name === 'AdminPromptAudit')).toBe(false)
    expect(router.resolve('/admin/prompt-audit').name).toBe('NotFound')
  })

  it('registers intelligent-test routes with mode props and redirects the old path', async () => {
    const { default: router } = await import('@/router')
    const propsOf = (path: string) => {
      const matched = router.resolve(path).matched[0]
      const props = matched.props as { default?: unknown }
      return typeof props.default === 'function' ? props.default(router.resolve(path)) : props.default
    }
    expect(router.getRoutes().some((route) => route.path === '/admin/accounts/tests')).toBe(true)
    expect(router.getRoutes().some((route) => route.path === '/admin/accounts/test-history')).toBe(true)
    expect(router.getRoutes().some((route) => route.path === '/admin/accounts/test-settings')).toBe(true)
    expect(propsOf('/admin/accounts/tests')).toEqual({ mode: 'tests' })
    expect(propsOf('/admin/accounts/test-history')).toEqual({ mode: 'history' })
    expect(propsOf('/admin/accounts/test-settings')).toEqual({ mode: 'settings' })
    expect(router.getRoutes().find((route) => route.path === '/admin/intelligent-tests')?.redirect).toBe('/admin/accounts/tests')
  })

  it('exposes Risk Control as a direct sidebar item', () => {
    const sidebarPath = resolve(dirname(fileURLToPath(import.meta.url)), '../../components/layout/AppSidebar.vue')
    const source = readFileSync(sidebarPath, 'utf8')
    const riskItem = source.match(
      /\{\s*path: '\/admin\/risk-control',[\s\S]*?featureFlag: flagRiskControl,[\s\S]*?\}/
    )?.[0]

    expect(riskItem).toContain("label: t('nav.riskControl')")
    expect(riskItem).not.toContain('children:')
  })
})
