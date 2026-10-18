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

test('console-parity: follow an Exec log, use a PTY terminal and delete the Sandbox', async ({
  page,
}) => {
  const requests = watchRequests(page)

  await page.goto('/login?return=%2Fsandboxes')
  await page.getByLabel('Username').fill(ADMIN.username)
  await page.getByLabel('Password').fill(ADMIN.password)
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page).toHaveURL(/\/sandboxes$/)

  const name = `pw-exec-${Date.now()}`
  await page.getByRole('link', { name: 'New Sandbox' }).click()
  await expect(page).toHaveURL(/\/sandboxes\/new$/)
  await page.getByLabel('Name').fill(name)
  await page.getByRole('button', { name: 'Create Sandbox' }).click()
  await expect(page).toHaveURL(/\/sandboxes\/[0-9a-f-]{36}$/)
  const sandboxId = page.url().split('/').pop()!
  await expect(page.getByTestId('sandbox-state')).toHaveText('ready', { timeout: 240_000 })

  // Create a non-PTY Exec through the API with the browser context's cookie
  // jar, then follow its log from offset 0 in the console.
  const logMarker = `exec-log-${Date.now()}`
  const created = await page.context().request.post(`${origin}/api/v1/sandboxes/${sandboxId}/execs`, {
    data: { argv: ['/bin/sh', '-c', `printf '${logMarker}\\n'; exit 3`] },
  })
  expect(created.status()).toBe(201)
  const exec = (await created.json()) as { id: string }

  await page.goto(`/sandboxes/${sandboxId}/execs?exec=${exec.id}`)
  await expect(page.locator('.xterm-rows')).toContainText(logMarker, { timeout: 60_000 })
  await expect(page.getByTestId('exec-result')).toHaveText('Finished: exited (exit code 3)', {
    timeout: 60_000,
  })

  // Open a PTY Exec from the terminal page and run a command in it.
  await page.goto(`/sandboxes/${sandboxId}/terminal`)
  await page.getByRole('button', { name: 'Open terminal' }).click()
  await expect(page.getByTestId('terminal-status')).toHaveText('connected', { timeout: 60_000 })
  const terminalMarker = `terminal-marker-${Date.now()}`
  await page.locator('.xterm').click()
  await page.keyboard.type(`echo ${terminalMarker}`)
  await page.keyboard.press('Enter')
  await expect(page.locator('.xterm-rows')).toContainText(terminalMarker, { timeout: 60_000 })

  await page.goto(`/sandboxes/${sandboxId}`)
  await page.getByRole('button', { name: 'Delete' }).click()
  await expect(page).toHaveURL(/\/sandboxes$/)
  await expect(page.getByRole('link', { name })).toHaveCount(0, { timeout: 120_000 })

  const disallowed = requests.filter(
    (url) => /^(https?|wss?):/i.test(url) && !isAllowedConsoleUrl(url, scope),
  )
  expect(disallowed, 'requests outside the console origin and port-forward hosts').toEqual([])
})
