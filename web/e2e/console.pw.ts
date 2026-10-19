import { expect, test } from '@playwright/test'

import { assertConsoleScope, signIn, watchRequests } from './harness'

test('admin signs in, creates a ready Sandbox and deletes it', async ({ page }) => {
  const requests = watchRequests(page)

  await signIn(page)
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

  assertConsoleScope(requests)
})

test('the console switches between English and Chinese', async ({ page }) => {
  await signIn(page)
  await expect(page.getByRole('link', { name: 'New Sandbox' })).toBeVisible()

  await page.getByRole('group', { name: 'Language' }).getByRole('button', { name: '中文' }).click()
  await expect(page.getByRole('link', { name: '新建 Sandbox' })).toBeVisible()
  await expect(page.locator('html')).toHaveAttribute('lang', 'zh')

  // The stored choice survives a reload.
  await page.reload()
  await expect(page.getByRole('link', { name: '新建 Sandbox' })).toBeVisible()

  await page.getByRole('button', { name: 'English' }).click()
  await expect(page.getByRole('link', { name: 'New Sandbox' })).toBeVisible()
  await expect(page.locator('html')).toHaveAttribute('lang', 'en')
})
