import { defineConfig, devices } from '@playwright/test'
import { dirname } from 'path'
import { fileURLToPath } from 'url'

const configDir = dirname(fileURLToPath(import.meta.url))

export default defineConfig({
  testDir: './e2e',
  testMatch: '**/*.spec.ts',
  fullyParallel: true,
  forbidOnly: Boolean(process.env.CI),
  reporter: process.env.CI ? 'github' : 'list',
  use: {
    baseURL: 'http://127.0.0.1:4173/e2e/harness/',
    screenshot: 'only-on-failure',
    trace: 'retain-on-failure'
  },
  projects: [
    {
      name: 'chromium',
      use: { ...devices['Desktop Chrome'] }
    }
  ],
  webServer: {
    command: 'node node_modules/vite/bin/vite.js --config e2e/vite.config.ts',
    cwd: configDir,
    url: 'http://127.0.0.1:4173/e2e/harness/',
    reuseExistingServer: !process.env.CI
  }
})
