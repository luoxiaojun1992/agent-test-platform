import { defineConfig } from '@playwright/test'

// The E2E stack (MySQL, API, workers, mock LLM, mock Agent) is started by e2e/stack.sh; see README "UI end-to-end tests".
export default defineConfig({
  testDir: './e2e',
  timeout: 120_000,
  expect: { timeout: 20_000 },
  fullyParallel: false,
  workers: 1,
  reporter: [['list']],
  use: {
    baseURL: process.env.E2E_BASE_URL ?? 'http://127.0.0.1:4173',
    trace: 'retain-on-failure',
    screenshot: 'only-on-failure',
  },
})
