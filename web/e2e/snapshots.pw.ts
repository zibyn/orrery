import { expect, test, type Page } from '@playwright/test'

import { assertConsoleScope, origin, signIn, watchRequests } from './harness'

type CapabilityEntry = { name: string; value: string }
type NodeList = { items: { capabilities?: { entries?: CapabilityEntry[] } }[] }

// The local e2e Agent reports snapshot_build=enforced only when its BuildKit
// self-check succeeds (runtime.md §12). Without it a build cannot run.
async function snapshotBuildSupported(page: Page): Promise<boolean> {
  const res = await page.context().request.get(`${origin}/api/v1/nodes?limit=200`)
  if (!res.ok()) return false
  const nodes = (await res.json()) as NodeList
  return nodes.items.some((node) =>
    (node.capabilities?.entries ?? []).some(
      (entry) => entry.name === 'snapshot_build' && entry.value === 'enforced',
    ),
  )
}

test('console-parity: list Snapshots, watch a build change state and read its log', async ({
  page,
}) => {
  const requests = watchRequests(page)

  await signIn(page, { returnTo: '/snapshots' })
  await expect(page).toHaveURL(/\/snapshots$/)

  const buildsSupported = await snapshotBuildSupported(page)

  // Create a Snapshot and trigger a build through the API (the console form
  // has its own cases below).
  const snapshotName = `pw-snapshot-${Date.now()}`
  const created = await page
    .context()
    .request.post(`${origin}/api/v1/snapshots`, { data: { name: snapshotName } })
  expect(created.status()).toBe(201)
  const snapshot = (await created.json()) as { id: string }

  const listed = await page.context().request.get(`${origin}/api/v1/snapshots?limit=200`)
  expect(listed.status()).toBe(200)
  const platformSnapshots = ((await listed.json()) as {
    items: { name: string; owner_id: string | null }[]
  }).items.filter((item) => item.owner_id === null)

  // A slow RUN keeps the building state observable in the console. Without a
  // platform base, FROM scratch needs no pull and still exercises BuildKit.
  const build = platformSnapshots[0]
    ? { dockerfile: 'FROM base\nRUN sleep 15\n', base: platformSnapshots[0].name }
    : { dockerfile: 'FROM scratch\n' }
  const createdVersion = await page
    .context()
    .request.post(`${origin}/api/v1/snapshots/${snapshot.id}/versions`, { data: { build } })
  expect(createdVersion.status()).toBe(201)
  const version = (await createdVersion.json()) as { id: string; build_exec_id?: string }
  expect(version.build_exec_id, 'a build version carries its build Exec id').toBeTruthy()

  // The list shows the new Snapshot, and the detail page shows the version.
  await page.goto('/snapshots')
  await expect(page.getByRole('link', { name: snapshotName })).toBeVisible()
  await page.getByRole('link', { name: snapshotName }).click()
  await expect(page).toHaveURL(new RegExp(`/snapshots/${snapshot.id}$`))

  const state = page.getByTestId(`version-state-${version.id}`)
  await expect(state).toBeVisible()

  if (buildsSupported) {
    await expect(state).toHaveText('building', { timeout: 15_000 })

    // Following the build log while the version is building uses the
    // WebSocket output surface; the stored log is re-read afterwards.
    await page.getByTestId(`build-log-link-${version.id}`).click()
    await expect(page).toHaveURL(new RegExp(`version=${version.id}`))
    await expect(page.locator('.xterm-rows')).not.toHaveText('', { timeout: 60_000 })
    // Each inner wait stays well below the 300 s per-test timeout so a later
    // assertion still has its own window to fail in.
    await expect(page.getByTestId('build-log-result')).toHaveText(/^Finished: exited/, {
      timeout: 120_000,
    })
    // The version becomes ready and the current pointer moves to it.
    await expect(state).toHaveText('ready', { timeout: 120_000 })
    await expect(page.getByTestId(`version-current-${version.id}`)).toBeVisible({
      timeout: 120_000,
    })
  } else {
    const reason =
      'build log assertion skipped: no Node reports snapshot_build=enforced (BuildKit is unavailable in this deployment)'
    test.info().annotations.push({ type: 'skip', description: reason })
    console.warn(reason)
    await expect(state, reason).toHaveText(/ready|failed|cancelled/, { timeout: 120_000 })
  }

  assertConsoleScope(requests)
})

test('console: create a Snapshot, reject a duplicate name, then delete it', async ({ page }) => {
  const requests = watchRequests(page)

  await signIn(page, { returnTo: '/snapshots' })
  await expect(page).toHaveURL(/\/snapshots$/)

  const snapshotName = `pw-console-snapshot-${Date.now()}`
  await page.getByRole('button', { name: 'New Snapshot' }).click()
  await page.getByLabel('Name').fill(snapshotName)
  await page.getByRole('button', { name: 'Create', exact: true }).click()

  // Creating lands on the new Snapshot's detail page.
  await expect(page).toHaveURL(/\/snapshots\/[^/?]+$/)
  await expect(page.getByRole('heading', { name: snapshotName })).toBeVisible()
  const detailUrl = page.url()

  // A duplicate name shows the server's problem detail in the Drawer.
  await page.goto('/snapshots')
  await page.getByRole('button', { name: 'New Snapshot' }).click()
  await page.getByLabel('Name').fill(snapshotName)
  await page.getByRole('button', { name: 'Create', exact: true }).click()
  await expect(page.getByRole('dialog').getByRole('alert')).not.toBeEmpty()
  await expect(page).toHaveURL(/\/snapshots$/)

  // Deleting after confirming returns to the list, without the Snapshot.
  await page.goto(detailUrl)
  page.once('dialog', (dialog) => void dialog.accept())
  await page.getByRole('button', { name: 'Delete', exact: true }).click()
  await expect(page).toHaveURL(/\/snapshots$/)
  await expect(page.getByRole('heading', { name: 'Snapshots' })).toBeVisible()
  await expect(page.getByRole('link', { name: snapshotName })).toHaveCount(0)

  assertConsoleScope(requests)
})

test('console: a Platform Snapshot shows no Delete action', async ({ page }) => {
  await signIn(page, { returnTo: '/snapshots' })

  const listed = await page.context().request.get(`${origin}/api/v1/snapshots?limit=200`)
  expect(listed.status()).toBe(200)
  const platform = (
    (await listed.json()) as { items: { id: string; owner_id: string | null }[] }
  ).items.find((item) => item.owner_id === null)
  test.skip(!platform, 'no Platform Snapshot is configured in this deployment')
  if (!platform) return

  await page.goto(`/snapshots/${platform.id}`)
  await expect(page.getByRole('heading', { level: 1 })).toBeVisible()
  await expect(page.getByText('Platform', { exact: true })).toBeVisible()
  await expect(page.getByRole('button', { name: 'Delete', exact: true })).toHaveCount(0)
  await expect(page.locator('[data-testid^="version-cancel-"]')).toHaveCount(0)
  await expect(page.locator('[data-testid^="version-set-current-"]')).toHaveCount(0)
  await expect(page.getByRole('link', { name: 'New version' })).toHaveCount(0)
})

// Creates an owned Snapshot and starts one build per Dockerfile through the
// API, so these cases do not depend on the console's New version form.
async function snapshotWithBuilds(
  page: Page,
  builds: { dockerfile: string; base?: string }[],
): Promise<{ snapshotId: string; versionIds: string[] }> {
  const request = page.context().request
  const created = await request.post(`${origin}/api/v1/snapshots`, {
    data: { name: `pw-versions-${Date.now()}` },
  })
  expect(created.status()).toBe(201)
  const snapshotId = ((await created.json()) as { id: string }).id
  const versionIds: string[] = []
  for (const build of builds) {
    const res = await request.post(`${origin}/api/v1/snapshots/${snapshotId}/versions`, {
      data: { build },
    })
    expect(res.status()).toBe(201)
    versionIds.push(((await res.json()) as { id: string }).id)
  }
  return { snapshotId, versionIds }
}

test('console: cancel a building version', async ({ page }) => {
  const requests = watchRequests(page)
  await signIn(page, { returnTo: '/snapshots' })
  test.skip(!(await snapshotBuildSupported(page)), 'no Node reports snapshot_build=enforced')

  const listed = await page.context().request.get(`${origin}/api/v1/snapshots?limit=200`)
  const platform = (
    (await listed.json()) as { items: { name: string; owner_id: string | null }[] }
  ).items.find((item) => item.owner_id === null)
  // A slow RUN needs a base image; FROM scratch finishes before it can be cancelled.
  test.skip(!platform, 'no Platform Snapshot to build a slow version on')
  if (!platform) return

  const { snapshotId, versionIds } = await snapshotWithBuilds(page, [
    { dockerfile: 'FROM base\nRUN sleep 120\n', base: platform.name },
  ])
  const [version] = versionIds
  await page.goto(`/snapshots/${snapshotId}`)
  await expect(page.getByTestId(`version-state-${version}`)).toHaveText('building')

  await page.getByTestId(`version-cancel-${version}`).click()
  await expect(page.getByTestId(`version-state-${version}`)).toHaveText('cancelled', {
    timeout: 60_000,
  })
  await expect(page.getByTestId(`version-cancel-${version}`)).toHaveCount(0)

  assertConsoleScope(requests)
})

test('console: set a version as current, and delete only a non-current one', async ({
  page,
}) => {
  const requests = watchRequests(page)
  await signIn(page, { returnTo: '/snapshots' })
  test.skip(!(await snapshotBuildSupported(page)), 'no Node reports snapshot_build=enforced')

  const { snapshotId, versionIds } = await snapshotWithBuilds(page, [
    { dockerfile: 'FROM scratch\n' },
    { dockerfile: 'FROM scratch\nLABEL second=1\n' },
  ])
  const [first, second] = versionIds
  const setCurrent = async (version: string) => {
    const res = await page
      .context()
      .request.put(`${origin}/api/v1/snapshots/${snapshotId}/current`, { data: { version } })
    expect(res.status()).toBe(200)
  }

  await page.goto(`/snapshots/${snapshotId}`)
  for (const id of versionIds) {
    await expect(page.getByTestId(`version-state-${id}`)).toHaveText('ready', {
      timeout: 120_000,
    })
  }
  // The pointer follows whichever build became ready last; pin it to the second.
  await setCurrent(second)
  await page.reload()
  await expect(page.getByTestId(`version-current-${second}`)).toBeVisible()
  await expect(page.getByTestId(`version-delete-${second}`)).toHaveCount(0)
  await expect(page.getByTestId(`version-set-current-${second}`)).toHaveCount(0)

  // Set the first version as current; the header and the badge follow.
  await page.getByTestId(`version-set-current-${first}`).click()
  await expect(page.getByTestId(`version-current-${first}`)).toBeVisible()
  await expect(page.getByText(/^current version/)).toContainText(first)
  await expect(page.getByTestId(`version-delete-${first}`)).toHaveCount(0)

  // Move the pointer back behind the page's back: the server refuses to
  // delete the current version and the console shows its problem detail.
  await setCurrent(second)
  page.once('dialog', (dialog) => void dialog.accept())
  await page.getByTestId(`version-delete-${second}`).click()
  await expect(page.getByTestId('version-action-error')).toContainText('current')
  await expect(page.getByTestId(`version-${second}`)).toBeVisible()

  // Deleting a non-current version after confirming removes it from the table.
  await page.reload()
  await expect(page.getByTestId(`version-current-${second}`)).toBeVisible()
  page.once('dialog', (dialog) => void dialog.accept())
  await page.getByTestId(`version-delete-${first}`).click()
  await expect(page.getByTestId(`version-${first}`)).toHaveCount(0)

  assertConsoleScope(requests)
})

async function createSnapshotViaApi(page: Page, prefix: string): Promise<string> {
  const created = await page
    .context()
    .request.post(`${origin}/api/v1/snapshots`, { data: { name: `${prefix}-${Date.now()}` } })
  expect(created.status()).toBe(201)
  return ((await created.json()) as { id: string }).id
}

// The build log card shows the live log, the queue notice or the stored result.
function buildLogShown(page: Page) {
  return page
    .getByTestId('build-log-queued')
    .or(page.locator('.xterm'))
    .or(page.getByTestId('build-log-result'))
    .first()
}

test('console: build a new version and land on its build log, one Idempotency-Key per form', async ({
  page,
}) => {
  const requests = watchRequests(page)
  await signIn(page, { returnTo: '/snapshots' })
  const snapshotId = await createSnapshotViaApi(page, 'pw-console-build')

  await page.goto(`/snapshots/${snapshotId}`)
  await page.getByRole('link', { name: 'New version' }).click()
  await expect(page).toHaveURL(new RegExp(`/snapshots/${snapshotId}/versions/new$`))

  // The first response is lost on the way back, so the User submits again.
  // Both submits carry the same key and the server returns the same version.
  const keys: string[] = []
  let firstVersionId = ''
  await page.route(`**/api/v1/snapshots/${snapshotId}/versions`, async (route) => {
    keys.push((await route.request().headerValue('idempotency-key')) ?? '')
    const response = await route.fetch()
    if (keys.length === 1) {
      firstVersionId = ((await response.json()) as { id: string }).id
      await route.fulfill({ status: 503, json: { code: 'unavailable', detail: 'lost' } })
      return
    }
    await route.fulfill({ response })
  })

  await page.getByLabel('Dockerfile').fill('FROM scratch\n')
  await page.getByRole('button', { name: 'Create version' }).click()
  await expect(page.getByRole('alert')).not.toBeEmpty()
  await page.getByRole('button', { name: 'Create version' }).click()

  await expect(page).toHaveURL(new RegExp(`/snapshots/${snapshotId}\\?version=${firstVersionId}`))
  expect(keys).toHaveLength(2)
  expect(keys[0]).not.toBe('')
  expect(keys[1]).toBe(keys[0])
  await expect(page.getByTestId(`version-state-${firstVersionId}`)).toBeVisible()
  await expect(buildLogShown(page)).toBeVisible({ timeout: 30_000 })

  assertConsoleScope(requests)
})

test('console: import rejects an unpinned ref before any request, then lands on the version', async ({
  page,
}) => {
  const requests = watchRequests(page)
  await signIn(page, { returnTo: '/snapshots' })
  const snapshotId = await createSnapshotViaApi(page, 'pw-console-import')

  let posts = 0
  page.on('request', (request) => {
    if (request.method() === 'POST' && request.url().endsWith(`/snapshots/${snapshotId}/versions`)) {
      posts++
    }
  })

  await page.goto(`/snapshots/${snapshotId}/versions/new`)
  await page.getByLabel('Source').selectOption('import')
  await page.getByLabel('Image ref').fill('docker.io/library/busybox:latest')
  await page.getByRole('button', { name: 'Create version' }).click()
  await expect(page.getByRole('alert')).toContainText('@sha256:')
  expect(posts).toBe(0)

  // The import itself may fail without registry access; landing on the
  // selected version is what the console owns.
  await page.getByLabel('Image ref').fill(`docker.io/library/busybox@sha256:${'a'.repeat(64)}`)
  await page.getByRole('button', { name: 'Create version' }).click()
  await expect(page).toHaveURL(new RegExp(`/snapshots/${snapshotId}\\?version=`))
  expect(posts).toBe(1)
  const versionId = new URL(page.url()).searchParams.get('version')
  await expect(page.getByTestId(`version-state-${versionId}`)).toBeVisible()

  assertConsoleScope(requests)
})
