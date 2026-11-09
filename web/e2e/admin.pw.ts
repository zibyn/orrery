import { expect, test } from '@playwright/test'

import { ADMIN, assertConsoleScope, origin, signIn, watchRequests } from './harness'

test('admin sets a User\'s quota and audits it; a non-admin sees no admin pages', async ({
  page,
  browser,
}) => {
  const requests = watchRequests(page)
  const name = `pw-user-${Date.now()}`

  // The User exists once user-token mints its Session. A fresh browser context,
  // so the admin Session cannot stand in for it.
  const userContext = await browser.newContext({ baseURL: origin })
  const userPage = await userContext.newPage()
  const userRequests = watchRequests(userPage)
  await signIn(userPage, { username: name })
  await expect(userPage).toHaveURL(/\/sandboxes$/)
  // Admin navigation is UI-gated on is_admin (the server still enforces scope).
  await expect(userPage.getByRole('link', { name: 'Users' })).toHaveCount(0)
  await expect(userPage.getByRole('link', { name: 'Audit' })).toHaveCount(0)
  const me = (await (await userContext.request.get('/api/v1/users/me')).json()) as { id: string }
  await userContext.close()

  await signIn(page)
  await expect(page).toHaveURL(/\/sandboxes$/)
  await page.getByRole('link', { name: 'Users' }).click()
  await expect(page).toHaveURL(/\/admin\/users$/)
  // Users come from Polaris or user-token, so the console cannot create one.
  await expect(page.getByRole('button', { name: 'New user' })).toHaveCount(0)

  const row = page.getByTestId('user-row').filter({ hasText: name })
  await expect(row).toBeVisible()
  await row.getByRole('button', { name: 'Edit' }).click()
  await page.getByLabel(`GPU quota for ${name}`).fill('2')
  await page.getByLabel(`Sandbox quota for ${name}`).fill('3')
  await page.getByRole('button', { name: 'Save changes' }).click()
  await expect(row.getByText('Quota 2 GPUs / 3 Sandboxes')).toBeVisible()

  await page.getByRole('link', { name: 'Audit' }).click()
  await expect(page).toHaveURL(/\/admin\/audit$/)
  await page.getByLabel('Actor').fill(ADMIN.username)
  await page.getByLabel('Action').fill('user.quota_update')
  await page.getByLabel('Target').fill(me.id)
  await page.getByRole('button', { name: 'Apply filters' }).click()
  await expect(page.getByTestId('audit-row').first()).toContainText('user.quota_update')
  await expect(page.getByTestId('audit-row').first()).toContainText('"gpus":2')

  assertConsoleScope(requests)
  assertConsoleScope(userRequests)
})
