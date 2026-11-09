import { execFileSync } from 'node:child_process'

import { expect, type Page } from '@playwright/test'

import { isAllowedConsoleUrl } from '../src/lib/request-scope'

// scripts/e2e.sh creates this machine admin with orrery-server user-token.
export const ADMIN = { username: 'admin' }
export const origin = process.env.ORRERY_E2E_API_URL ?? 'http://127.0.0.1:18080'
export const appsDomain = process.env.ORRERY_E2E_APPS_DOMAIN ?? 'apps.test'
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

// mintSession creates (or reuses) the machine User called username and mints a
// Session for it with orrery-server user-token --session: e2e never runs
// Polaris (ADR-0029).
export function mintSession(username: string): string {
  const file = process.env.ORRERY_E2E_COMPOSE_FILE
  const project = process.env.ORRERY_E2E_COMPOSE_PROJECT
  if (!file || !project) {
    throw new Error('ORRERY_E2E_COMPOSE_FILE and ORRERY_E2E_COMPOSE_PROJECT are required (scripts/e2e.sh sets them)')
  }
  const out = execFileSync('docker', [
    'compose', '-f', file, '-p', project,
    'exec', '-T', 'server', 'orrery-server', 'user-token', '--name', username, '--session',
  ])
  return (JSON.parse(out.toString()) as { token: string }).token
}

// signIn sets a fresh Session cookie for username, then opens returnTo, so
// callers can assert the resulting URL.
export async function signIn(
  page: Page,
  options: { username?: string; returnTo?: string } = {},
): Promise<void> {
  const { username = ADMIN.username, returnTo = '/sandboxes' } = options
  // Pin the console to English on the first visit so the existing assertions
  // keep matching the source strings (api.md §8). A language chosen later is
  // kept.
  await page.addInitScript(() => {
    if (!localStorage.getItem('PARAGLIDE_LOCALE')) {
      localStorage.setItem('PARAGLIDE_LOCALE', 'en')
    }
  })
  await page.context().addCookies([{ name: 'orrery_session', value: mintSession(username), url: origin }])
  await page.goto(returnTo)
  await expect(page).not.toHaveURL(/\/login/)
}
