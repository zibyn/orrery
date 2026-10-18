import { expect, type Page } from '@playwright/test'

import { isAllowedConsoleUrl } from '../src/lib/request-scope'

export const ADMIN = { username: 'admin', password: 'admin-password-123' }
export const origin = process.env.STARS_E2E_API_URL ?? 'http://127.0.0.1:18080'
export const appsDomain = process.env.STARS_E2E_APPS_DOMAIN ?? 'apps.test'
export const scope = { origin, appsDomain }

// console-parity: collect every HTTP request and WebSocket, then assert each
// URL stays on the console origin or a port-forward subdomain (api.md §8).
export function watchRequests(page: Page): string[] {
  const urls: string[] = []
  page.on('request', (request) => urls.push(request.url()))
  page.on('websocket', (socket) => urls.push(socket.url()))
  return urls
}

export function assertConsoleScope(requests: string[]): void {
  const disallowed = requests.filter(
    (url) => /^(https?|wss?):/i.test(url) && !isAllowedConsoleUrl(url, scope),
  )
  expect(
    disallowed,
    'requests outside the console origin and its public paths or a port-forward host',
  ).toEqual([])
}

// The login flow reloads the whole page, so callers can assert the resulting
// URL. `returnTo` mirrors the console's own ?return= parameter.
export async function signIn(
  page: Page,
  options: { username?: string; password?: string; returnTo?: string } = {},
): Promise<void> {
  const { username = ADMIN.username, password = ADMIN.password, returnTo = '/sandboxes' } = options
  await page.goto(`/login?return=${encodeURIComponent(returnTo)}`)
  await page.getByLabel('Username').fill(username)
  await page.getByLabel('Password').fill(password)
  await page.getByRole('button', { name: 'Sign in' }).click()
}
