import { expect, request as playwrightRequest, test } from '@playwright/test'

import { assertConsoleScope, origin, signIn, watchRequests } from './harness'

test('admin creates a token, uses it, revokes it and it stops working', async ({ page }) => {
  const requests = watchRequests(page)
  // A cookie-less request context: the API call must be authenticated by the
  // bearer token alone, not by the browser's Session cookie.
  const api = await playwrightRequest.newContext({ baseURL: origin })

  await signIn(page, { returnTo: '/account' })
  await expect(page).toHaveURL(/\/account$/)

  // The login response carried the Session id, so exactly one row is marked.
  await expect(page.getByTestId('current-session')).toHaveCount(1)

  const name = `pw-token-${Date.now()}`
  await page.getByRole('button', { name: 'Create token' }).click()
  await page.getByLabel('Name').fill(name)
  await page.getByRole('button', { name: 'Create', exact: true }).click()

  const plaintext = page.getByTestId('token-plaintext')
  await expect(plaintext).toBeVisible()
  const token = (await plaintext.textContent())?.trim() ?? ''
  expect(token).toMatch(/^orr_/)

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

  assertConsoleScope(requests)
})
