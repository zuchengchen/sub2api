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
})
