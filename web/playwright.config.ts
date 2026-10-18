import { defineConfig } from '@playwright/test'

// The console under test is the embedded production build served by the
// Compose deployment from scripts/e2e.sh; this config never starts Vite.
export default defineConfig({
  testDir: './e2e',
  testMatch: '**/*.pw.ts',
  fullyParallel: false,
  workers: 1,
  timeout: 300_000,
  use: {
    baseURL: process.env.STARS_E2E_API_URL ?? 'http://127.0.0.1:18080',
    trace: 'retain-on-failure',
  },
})
