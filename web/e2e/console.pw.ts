import { expect, test, type Page } from '@playwright/test'

import { isAllowedConsoleUrl } from '../src/lib/request-scope'

const ADMIN = { username: 'admin', password: 'admin-password-123' }
const origin = process.env.STARS_E2E_API_URL ?? 'http://127.0.0.1:18080'
const scope = { origin, appsDomain: process.env.STARS_E2E_APPS_DOMAIN ?? 'apps.test' }

// console-parity: collect every HTTP request and WebSocket, then assert each
// URL stays on the console origin or a port-forward subdomain (api.md §8).
function watchRequests(page: Page): string[] {
  const urls: string[] = []
  page.on('request', (request) => urls.push(request.url()))
  page.on('websocket', (socket) => urls.push(socket.url()))
  return urls
}

test('admin signs in, creates a ready Sandbox and deletes it', async ({ page }) => {
  const requests = watchRequests(page)

  await page.goto('/login?return=%2Fsandboxes')
  await page.getByLabel('Username').fill(ADMIN.username)
  await page.getByLabel('Password').fill(ADMIN.password)
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page).toHaveURL(/\/sandboxes$/)

  const name = `pw-console-${Date.now()}`
  await page.getByRole('link', { name: 'New Sandbox' }).click()
  await expect(page).toHaveURL(/\/sandboxes\/new$/)
  await page.getByLabel('Name').fill(name)
  await page.getByRole('button', { name: 'Create Sandbox' }).click()
  await expect(page).toHaveURL(/\/sandboxes\/[0-9a-f-]{36}$/)

  await expect(page.getByTestId('sandbox-state')).toHaveText('ready', { timeout: 240_000 })

  await page.getByRole('button', { name: 'Delete' }).click()
  await expect(page).toHaveURL(/\/sandboxes$/)
  await expect(page.getByRole('link', { name })).toHaveCount(0, { timeout: 120_000 })

  const disallowed = requests.filter(
    (url) => /^(https?|wss?):/i.test(url) && !isAllowedConsoleUrl(url, scope),
  )
  expect(disallowed, 'requests outside the console origin and port-forward hosts').toEqual([])
})
