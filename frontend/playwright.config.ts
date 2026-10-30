import { defineConfig, devices } from '@playwright/test'

const port = Number(process.env.PLAYWRIGHT_PORT || 43179)
const baseURL = `http://127.0.0.1:${port}`

export default defineConfig({
  testDir: './e2e',
  testMatch: ['risk-control.spec.ts', 'guide.spec.ts', 'guide-admin.spec.ts', 'performance-optimization.spec.ts'],
  fullyParallel: false,
  workers: 1,
  retries: 0,
  timeout: 30_000,
  outputDir: process.env.PLAYWRIGHT_OUTPUT_DIR || '/tmp/sub2api-risk-control-playwright',
  reporter: [['list']],
  use: {
    baseURL,
    launchOptions: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH
      ? { executablePath: process.env.PLAYWRIGHT_CHROMIUM_EXECUTABLE_PATH }
      : undefined,
    locale: 'zh-CN',
    colorScheme: 'light',
    trace: 'retain-on-failure',
  },
  projects: [
    {
      name: 'desktop-1440x900',
      use: { ...devices['Desktop Chrome'], viewport: { width: 1440, height: 900 } },
    },
    {
      name: 'mobile-390x844',
      use: { ...devices['Pixel 5'], viewport: { width: 390, height: 844 } },
    },
  ],
  webServer: {
    command: `corepack pnpm@9.15.9 --ignore-workspace exec vite --host 127.0.0.1 --strictPort --port ${port}`,
    url: baseURL,
    // Tests mock all APIs. Never let Vite's HTML settings fetch reach production.
    env: { VITE_DEV_PROXY_TARGET: 'http://127.0.0.1:9', VITE_API_BASE_URL: '/api/v1' },
    reuseExistingServer: false,
    timeout: 60_000,
  },
})
