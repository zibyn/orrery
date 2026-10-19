import { expect, test } from '@playwright/test'

import { ADMIN, assertConsoleScope, origin, signIn, watchRequests } from './harness'

test('admin creates a User, sets quota, resets the password, and audits it', async ({
  page,
  browser,
}) => {
  const requests = watchRequests(page)

  await signIn(page)
  await expect(page).toHaveURL(/\/sandboxes$/)

  await page.getByRole('link', { name: 'Users' }).click()
  await expect(page).toHaveURL(/\/admin\/users$/)

  const name = `pw-user-${Date.now()}`
  const initialPassword = 'initial-password-123'
  const resetPassword = 'reset-password-1234'

  await page.getByRole('button', { name: 'New User' }).click()
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

  await row.getByRole('button', { name: 'Edit' }).click()
  await page.getByLabel(`GPU quota for ${name}`).fill('2')
  await page.getByLabel(`Sandbox quota for ${name}`).fill('3')
  await page.getByRole('button', { name: 'Set quota' }).click()
  await expect(row.getByText('Limits 2 GPUs / 3 Sandboxes')).toBeVisible()

  await page.getByLabel(`New password for ${name}`).fill(resetPassword)
  await page.getByRole('button', { name: 'Reset password' }).click()
  await expect(page.getByLabel(`New password for ${name}`)).toHaveValue('')
  await page.getByRole('button', { name: 'Close' }).click()

  // The new User signs in with the reset password in a fresh browser context,
  // so the admin Session cannot stand in for it.
  const userContext = await browser.newContext({ baseURL: origin })
  const userPage = await userContext.newPage()
  const userRequests = watchRequests(userPage)
  await signIn(userPage, { username: name, password: resetPassword })
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

  assertConsoleScope(requests)
  assertConsoleScope(userRequests)
})
