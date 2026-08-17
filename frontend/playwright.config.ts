import { defineConfig, devices } from '@playwright/test'

export default defineConfig({
  testDir: './e2e',
  testMatch: 'risk-control.spec.ts',
  fullyParallel: false,
  workers: 1,
  retries: 0,
  timeout: 30_000,
  outputDir: '/tmp/sub2api-risk-control-playwright',
  reporter: [['list']],
  use: {
    baseURL: 'http://127.0.0.1:43179',
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
    command: 'corepack pnpm exec vite --host 127.0.0.1 --port 43179',
    url: 'http://127.0.0.1:43179',
    reuseExistingServer: false,
    timeout: 60_000,
  },
})
