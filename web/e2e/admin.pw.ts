import { expect, test, type Page } from '@playwright/test'

import { isAllowedConsoleUrl } from '../src/lib/request-scope'

const ADMIN = { username: 'admin', password: 'admin-password-123' }
const origin = process.env.STARS_E2E_API_URL ?? 'http://127.0.0.1:18080'
const scope = { origin, appsDomain: process.env.STARS_E2E_APPS_DOMAIN ?? 'apps.test' }

// console-parity: every HTTP request and WebSocket must stay on the console
// origin or a port-forward subdomain (api.md §8).
function watchRequests(page: Page): string[] {
  const urls: string[] = []
  page.on('request', (request) => urls.push(request.url()))
  page.on('websocket', (socket) => urls.push(socket.url()))
  return urls
}

async function signIn(page: Page, username: string, password: string) {
  await page.goto('/login?return=%2Fsandboxes')
  await page.getByLabel('Username').fill(username)
  await page.getByLabel('Password').fill(password)
  await page.getByRole('button', { name: 'Sign in' }).click()
}

test('admin creates a User, sets quota, resets the password, and audits it', async ({
  page,
  browser,
}) => {
  const requests = watchRequests(page)

  await signIn(page, ADMIN.username, ADMIN.password)
  await expect(page).toHaveURL(/\/sandboxes$/)

  await page.getByRole('link', { name: 'Users' }).click()
  await expect(page).toHaveURL(/\/admin\/users$/)

  const name = `pw-user-${Date.now()}`
  const initialPassword = 'initial-password-123'
  const resetPassword = 'reset-password-1234'

  await page.getByLabel('Name', { exact: true }).fill(name)
  await page.getByLabel('Email (optional)').fill(`${name}@example.com`)
  await page.getByLabel('Password (at least 12 characters)').fill(initialPassword)
  const [createdResponse] = await Promise.all([
    page.waitForResponse(
      (response) =>
        response.url().endsWith('/api/v1/users') && response.request().method() === 'POST',
    ),
    page.getByRole('button', { name: 'Create User' }).click(),
  ])
  expect(createdResponse.status()).toBe(201)
  const created = (await createdResponse.json()) as { id: string }

  const row = page.getByTestId('user-row').filter({ hasText: name })
  await expect(row).toBeVisible()

  await row.getByLabel(`GPU quota for ${name}`).fill('2')
  await row.getByLabel(`Sandbox quota for ${name}`).fill('3')
  await row.getByRole('button', { name: 'Set quota' }).click()
  await expect(row.getByText('Limits 2 GPUs / 3 Sandboxes')).toBeVisible()

  await row.getByLabel(`New password for ${name}`).fill(resetPassword)
  await row.getByRole('button', { name: 'Reset password' }).click()
  await expect(row.getByLabel(`New password for ${name}`)).toHaveValue('')

  // The new User signs in with the reset password in a fresh browser context,
  // so the admin Session cannot stand in for it.
  const userContext = await browser.newContext({ baseURL: origin })
  const userPage = await userContext.newPage()
  const userRequests = watchRequests(userPage)
  await signIn(userPage, name, resetPassword)
  await expect(userPage).toHaveURL(/\/sandboxes$/)
  // Admin navigation is UI-gated on is_admin (the server still enforces scope).
  await expect(userPage.getByRole('link', { name: 'Users' })).toHaveCount(0)
  await expect(userPage.getByRole('link', { name: 'Audit' })).toHaveCount(0)
  await userContext.close()

  await page.getByRole('link', { name: 'Audit' }).click()
  await expect(page).toHaveURL(/\/admin\/audit$/)

  const apply = page.getByRole('button', { name: 'Apply filters' })
  const action = page.getByLabel('Action')
  const actor = page.getByLabel('Actor')
  const target = page.getByLabel('Target')

  await actor.fill(ADMIN.username)
  await action.fill('user.create')
  await target.fill(created.id)
  await apply.click()
  await expect(page.getByTestId('audit-row').first()).toContainText('user.create')
  await expect(page.getByTestId('audit-row').first()).toContainText(name)

  await action.fill('user.quota_update')
  await apply.click()
  await expect(page.getByTestId('audit-row').first()).toContainText('user.quota_update')
  await expect(page.getByTestId('audit-row').first()).toContainText('"gpus":2')

  await action.fill('user.password_reset')
  await apply.click()
  await expect(page.getByTestId('audit-row').first()).toContainText('user.password_reset')

  for (const urls of [requests, userRequests]) {
    const disallowed = urls.filter(
      (url) => /^(https?|wss?):/i.test(url) && !isAllowedConsoleUrl(url, scope),
    )
    expect(disallowed, 'requests outside the console origin and port-forward hosts').toEqual([])
  }
})
