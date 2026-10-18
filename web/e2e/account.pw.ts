import { expect, request as playwrightRequest, test, type Page } from '@playwright/test'

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

test('admin creates a token, uses it, revokes it and it stops working', async ({ page }) => {
  const requests = watchRequests(page)
  // A cookie-less request context: the API call must be authenticated by the
  // bearer token alone, not by the browser's Session cookie.
  const api = await playwrightRequest.newContext({ baseURL: origin })

  await page.goto('/login?return=%2Faccount')
  await page.getByLabel('Username').fill(ADMIN.username)
  await page.getByLabel('Password').fill(ADMIN.password)
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page).toHaveURL(/\/account$/)

  // The login response carried the Session id, so exactly one row is marked.
  await expect(page.getByTestId('current-session')).toHaveCount(1)

  const name = `pw-token-${Date.now()}`
  await page.getByLabel('Name').fill(name)
  await page.getByRole('button', { name: 'Create token' }).click()

  const plaintext = page.getByTestId('token-plaintext')
  await expect(plaintext).toBeVisible()
  const token = (await plaintext.textContent())?.trim() ?? ''
  expect(token).toMatch(/^ssk_/)

  // The freshly created token authenticates a real API call.
  const first = await api.get('/api/v1/users/me/tokens', {
    headers: { Authorization: `Bearer ${token}` },
  })
  expect(first.status()).toBe(200)

  const row = page.getByRole('listitem').filter({ hasText: name })
  await row.getByRole('button', { name: 'Revoke' }).click()
  await expect(row).toHaveCount(0)

  const second = await api.get('/api/v1/users/me/tokens', {
    headers: { Authorization: `Bearer ${token}` },
  })
  expect(second.status()).toBe(401)
  await api.dispose()

  const disallowed = requests.filter(
    (url) => /^(https?|wss?):/i.test(url) && !isAllowedConsoleUrl(url, scope),
  )
  expect(disallowed, 'requests outside the console origin and port-forward hosts').toEqual([])
})
