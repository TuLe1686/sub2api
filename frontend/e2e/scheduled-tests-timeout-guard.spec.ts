import { expect, test, type Page } from '@playwright/test'

const plan = {
  id: 9,
  account_id: 42,
  model_id: 'gpt-5.4',
  cron_expression: '*/30 * * * *',
  enabled: true,
  max_results: 100,
  auto_recover: true,
  timeout_protection_mode: 'enforce',
  effective_timeout_protection_mode: 'shadow',
  timeout_protection_override_reason: 'force_shadow',
  timeout_seconds: 120,
  consecutive_timeout_threshold: 3,
  retry_delays_seconds: [5, 15, 30],
  consecutive_timeout_count: 7,
  owns_inactive_account: true,
  last_run_at: null,
  next_run_at: null,
  created_at: '2026-08-15T00:00:00Z',
  updated_at: '2026-08-15T00:00:00Z'
}

const result = {
  id: 91,
  plan_id: 9,
  status: 'failed',
  response_text: '',
  error_message: 'ownership conflict',
  latency_ms: 120000,
  run_mode: 'recovery',
  attempt_count: 2,
  classification: 'timeout',
  protection_action: 'blocked',
  blocked_reason: 'account_already_owned',
  started_at: '2026-08-15T00:00:00Z',
  finished_at: '2026-08-15T00:02:00Z',
  created_at: '2026-08-15T00:02:00Z'
}

async function mockScheduledTestAPI(page: Page) {
  const updateRequests: Array<{ url: string; body: unknown }> = []
  const unexpectedRequests: string[] = []
  const observedRequests: string[] = []

  await page.route(/\/api\/v1\//, async (route) => {
    const request = route.request()
    const url = new URL(request.url())
    const method = request.method()
    observedRequests.push(`${method} ${url.pathname}${url.search}`)

    if (method === 'GET' && url.pathname === '/api/v1/admin/accounts/42/scheduled-test-plans') {
      await route.fulfill({ json: [plan] })
      return
    }

    if (method === 'GET' && url.pathname === '/api/v1/admin/scheduled-test-plans/9/results') {
      await route.fulfill({ json: [result] })
      return
    }

    if (method === 'PUT' && url.pathname === '/api/v1/admin/scheduled-test-plans/9') {
      updateRequests.push({ url: request.url(), body: request.postDataJSON() })
      await route.fulfill({ json: { ...plan, enabled: false, owns_inactive_account: false } })
      return
    }

    unexpectedRequests.push(`${method} ${request.url()}`)
    await route.abort('blockedbyclient')
  })

  return { updateRequests, unexpectedRequests, observedRequests }
}

test('accepts timeout guard status and requires ownership confirmation', async ({ page }) => {
  const { updateRequests, unexpectedRequests, observedRequests } = await mockScheduledTestAPI(page)
  const browserErrors: string[] = []
  page.on('pageerror', (error) => browserErrors.push(error.message))
  page.on('console', (message) => {
    if (message.type() === 'error') browserErrors.push(message.text())
  })

  await page.goto('./')
  const openButton = page.getByRole('button', { name: 'Open scheduled tests' })
  await expect(openButton, browserErrors.join('\n')).toBeVisible({ timeout: 5_000 })
  await openButton.click()

  const panel = page.getByRole('dialog', { name: 'Scheduled Tests' })
  await expect(panel).toBeVisible()
  await expect(panel, observedRequests.join('\n')).toContainText('Timeout Protection Mode: Enforce')
  await expect(panel).toContainText('Effective Mode: Shadow')
  await expect(panel).toContainText('Override Reason: Forced Shadow Mode')
  await expect(panel).toContainText('Consecutive Timeouts: 7')
  await expect(panel).toContainText('Owns Inactive Account: Yes')

  await panel.getByRole('button', { name: 'Add Plan' }).click()
  const modeField = panel.getByTestId('new-timeout-protection-mode')
  await expect(modeField).toContainText('Off')
  await expect(panel.getByTestId('new-timeout-seconds').getByRole('spinbutton')).toHaveValue('60')
  await expect(panel.getByTestId('new-consecutive-timeout-threshold').getByRole('spinbutton')).toHaveValue('3')
  await expect(panel.getByTestId('new-retry-delays-seconds').getByRole('textbox')).toHaveValue('10, 20')
  await panel.getByRole('button', { name: 'Cancel' }).click()

  await panel.getByText('gpt-5.4', { exact: true }).click()
  await expect(panel.getByText('Failed', { exact: true })).toBeVisible()
  await expect(panel.getByText(/Run Mode:\s*Recovery Probe/)).toBeVisible()
  await expect(panel.getByText(/Classification:\s*Timeout/)).toBeVisible()
  await expect(panel.getByText(/Protection Action:\s*Blocked/)).toBeVisible()
  await expect(panel.getByText(/Blocked Reason:\s*Account Owned by Another Plan/)).toBeVisible()

  const enabledSwitch = panel.getByRole('switch')
  await enabledSwitch.click()
  await expect(page.getByRole('heading', { name: 'Restore Account and Release Ownership' })).toBeVisible()
  await expect(page.getByText(/restore its inactive account and release plan ownership/)).toBeVisible()
  await expect.poll(() => updateRequests.length).toBe(0)

  await page.getByRole('button', { name: 'Cancel' }).click()
  await expect(enabledSwitch).toHaveAttribute('aria-checked', 'true')
  await expect.poll(() => updateRequests.length).toBe(0)

  await enabledSwitch.click()
  await page.getByRole('button', { name: 'Restore and Disable' }).click()
  await expect.poll(() => updateRequests).toHaveLength(1)
  expect(updateRequests[0].body).toEqual({ enabled: false })
  expect(new URL(updateRequests[0].url).searchParams.get('restore_owned_account')).toBe('true')
  await expect(enabledSwitch).toHaveAttribute('aria-checked', 'false')
  expect(unexpectedRequests).toEqual([])
  expect(browserErrors).toEqual([])
})
