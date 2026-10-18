import { readFile } from 'node:fs/promises'

import { expect, test, type Page } from '@playwright/test'

import { isAllowedConsoleUrl } from '../src/lib/request-scope'

const ADMIN = { username: 'admin', password: 'admin-password-123' }
const origin = process.env.STARS_E2E_API_URL ?? 'http://127.0.0.1:18080'
const scope = { origin, appsDomain: process.env.STARS_E2E_APPS_DOMAIN ?? 'apps.test' }

// Collect every HTTP request and WebSocket so the test can assert the console
// stays on its own origin or a port-forward host (api.md §8).
function watchRequests(page: Page): string[] {
  const urls: string[] = []
  page.on('request', (request) => urls.push(request.url()))
  page.on('websocket', (socket) => urls.push(socket.url()))
  return urls
}

function assertConsoleScope(requests: string[]) {
  const disallowed = requests.filter(
    (url) => /^(https?|wss?):/i.test(url) && !isAllowedConsoleUrl(url, scope),
  )
  expect(disallowed, 'requests outside the console origin and port-forward hosts').toEqual([])
}

async function signIn(page: Page) {
  await page.goto('/login?return=%2Fsandboxes')
  await page.getByLabel('Username').fill(ADMIN.username)
  await page.getByLabel('Password').fill(ADMIN.password)
  await page.getByRole('button', { name: 'Sign in' }).click()
  await expect(page).toHaveURL(/\/sandboxes$/)
}

// scripts/e2e.sh enrolls a Node Agent before running Playwright, so a Sandbox
// created here reaches ready without any extra setup.
async function createReadySandbox(page: Page, name: string) {
  await page.getByRole('link', { name: 'New Sandbox' }).click()
  await expect(page).toHaveURL(/\/sandboxes\/new$/)
  await page.getByLabel('Name').fill(name)
  await page.getByRole('button', { name: 'Create Sandbox' }).click()
  await expect(page).toHaveURL(/\/sandboxes\/[0-9a-f-]{36}$/)
  await expect(page.getByTestId('sandbox-state')).toHaveText('ready', { timeout: 240_000 })
}

test('uploads a file, lists it and downloads the same bytes', async ({ page }) => {
  const requests = watchRequests(page)
  await signIn(page)
  await createReadySandbox(page, `pw-files-${Date.now()}`)

  const name = 'hello.txt'
  const contents = `hello from playwright ${Date.now()}\n`
  await page.locator('input[type=file]').setInputFiles({
    name,
    mimeType: 'text/plain',
    buffer: Buffer.from(contents),
  })

  const row = page.getByTestId(`file-${name}`)
  await expect(row).toBeVisible({ timeout: 30_000 })

  const downloadPromise = page.waitForEvent('download')
  await row.getByRole('link', { name: 'Download' }).click()
  const download = await downloadPromise
  const downloaded = await download.path()
  expect(downloaded).not.toBeNull()
  expect((await readFile(downloaded!)).toString()).toBe(contents)

  assertConsoleScope(requests)
})

test('opens the URL returned for a port ticket', async ({ page }) => {
  const requests = watchRequests(page)
  // Do not depend on *.apps.test resolving: capture window.open instead.
  await page.addInitScript(() => {
    const opened: string[] = []
    ;(window as unknown as { __openedPortUrls: string[] }).__openedPortUrls = opened
    window.open = ((url?: string | URL) => {
      opened.push(String(url))
      return null
    }) as typeof window.open
  })
  await signIn(page)
  await createReadySandbox(page, `pw-ports-${Date.now()}`)

  const ticketResponse = page.waitForResponse(
    (res) => res.url().endsWith('/ports/8080/ticket') && res.request().method() === 'POST',
  )
  await page.getByLabel('Port').fill('8080')
  await page.getByRole('button', { name: 'Open' }).click()

  const response = await ticketResponse
  expect(response.status()).toBe(200)
  const ticket = (await response.json()) as { url: string }
  expect(ticket.url).toMatch(/^https:\/\/8080--[0-9a-f-]+\.apps\.test\//)

  await expect
    .poll(() =>
      page.evaluate(() => (window as unknown as { __openedPortUrls: string[] }).__openedPortUrls),
    )
    .toEqual([ticket.url])

  assertConsoleScope(requests)
})
