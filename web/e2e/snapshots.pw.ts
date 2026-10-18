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

  // Create a Snapshot and trigger a build through the API (building is
  // CLI-only in the console, api.md §8).
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
