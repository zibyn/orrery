import { readFile } from 'node:fs/promises'
import http from 'node:http'

import { expect, test, type Page } from '@playwright/test'

import { appsDomain, assertConsoleScope, origin, signIn, watchRequests } from './harness'

// scripts/e2e.sh enrolls a Node Agent before running Playwright, so a Sandbox
// created here reaches ready without any extra setup.
async function createReadySandbox(page: Page, name: string): Promise<string> {
  await page.getByRole('link', { name: 'New Sandbox' }).click()
  await expect(page).toHaveURL(/\/sandboxes\/new$/)
  await page.getByLabel('Name').fill(name)
  await page.getByRole('button', { name: 'Create Sandbox' }).click()
  await expect(page).toHaveURL(/\/sandboxes\/[0-9a-f-]{36}$/)
  const sandboxId = page.url().split('/').pop()!
  await expect(page.getByTestId('sandbox-state')).toHaveText('ready', { timeout: 240_000 })
  return sandboxId
}

type GatewayReply = {
  status: number
  setCookie: string[]
  location?: string
  body: string
}

// The wildcard *.apps.test host does not resolve on this machine, so reach the
// Gateway by sending the port subdomain as the Host header to the API origin,
// exactly as e2e/portforward_test.go does.
function gatewayRequest(host: string, path: string, cookie?: string): Promise<GatewayReply> {
  const target = new URL(origin)
  return new Promise((resolve, reject) => {
    const req = http.request(
      {
        hostname: target.hostname,
        port: target.port || 80,
        path,
        method: 'GET',
        headers: { Host: host, ...(cookie ? { Cookie: cookie } : {}) },
      },
      (res) => {
        let body = ''
        res.setEncoding('utf8')
        res.on('data', (chunk: string) => (body += chunk))
        res.on('end', () =>
          resolve({
            status: res.statusCode ?? 0,
            setCookie: res.headers['set-cookie'] ?? [],
            location: res.headers.location,
            body,
          }),
        )
      },
    )
    req.on('error', reject)
    req.end()
  })
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

test('opens the port URL and reaches the Sandbox through the Gateway', async ({ page }) => {
  const requests = watchRequests(page)
  // Do not depend on *.apps.test resolving in the browser: capture the popup
  // handle and its navigation instead.
  await page.addInitScript(() => {
    const opened: { href: string; closed: boolean }[] = []
    ;(window as unknown as { __openedPortWindows: typeof opened }).__openedPortWindows = opened
    window.open = (() => {
      const win = {
        href: '',
        closed: false,
        location: {
          get href() {
            return win.href
          },
          set href(value: string) {
            win.href = value
          },
        },
        close() {
          win.closed = true
        },
      }
      opened.push(win)
      return win
    }) as typeof window.open
  })
  await signIn(page)
  const sandboxId = await createReadySandbox(page, `pw-ports-${Date.now()}`)

  // A plain HTTP service inside the Sandbox (the same nc loop the Go e2e
  // uses; Alpine's busybox has nc but no httpd). The probe waits for the
  // listener, not the test clock.
  const serve =
    "while true; do printf 'HTTP/1.1 200 OK\\r\\nContent-Length: 11\\r\\nConnection: close\\r\\n\\r\\nhello-port\\n' | nc -l -p 8000; done"
  const serveResponse = await page
    .context()
    .request.post(`${origin}/api/v1/sandboxes/${sandboxId}/execs`, {
      data: { argv: ['sh', '-c', serve] },
    })
  expect(serveResponse.status()).toBe(201)

  const probeResponse = await page
    .context()
    .request.post(`${origin}/api/v1/sandboxes/${sandboxId}/execs`, {
      data: {
        argv: [
          'sh',
          '-c',
          'for i in $(seq 1 50); do wget -qO- http://127.0.0.1:8000/ && exit 0; sleep 0.1; done; exit 1',
        ],
      },
    })
  expect(probeResponse.status()).toBe(201)
  const probe = (await probeResponse.json()) as { id: string }
  const probeUrl = `${origin}/api/v1/execs/${probe.id}`
  await expect
    .poll(async () => ((await (await page.context().request.get(probeUrl)).json()) as { state: string }).state, {
      timeout: 30_000,
    })
    .toBe('finished')
  const finished = (await (await page.context().request.get(probeUrl)).json()) as {
    result?: { cause?: string; exit_code?: number }
  }
  expect(finished.result?.cause).toBe('exited')
  expect(finished.result?.exit_code).toBe(0)

  const ticketResponse = page.waitForResponse(
    (res) => res.url().endsWith('/ports/8000/ticket') && res.request().method() === 'POST',
  )
  await page.getByLabel('Port').fill('8000')
  await page.getByRole('button', { name: 'Open' }).click()

  const response = await ticketResponse
  expect(response.status()).toBe(200)
  const ticket = (await response.json()) as { url: string }
  const ticketUrl = new URL(ticket.url)
  // The scheme is fixed to https by the server; the host is what matters.
  expect(ticketUrl.hostname).toBe(`8000--${sandboxId}.${appsDomain}`)
  expect(ticketUrl.searchParams.get('ticket')).toBeTruthy()

  await expect
    .poll(() =>
      page.evaluate(() =>
        (
          window as unknown as { __openedPortWindows: { href: string }[] }
        ).__openedPortWindows.map((win) => win.href),
      ),
    )
    .toEqual([ticket.url])

  // Exchange the ticket for the stars_port cookie, then fetch the app through
  // the Gateway. The nc loop restarts between connections, so retry briefly.
  const host = ticketUrl.hostname
  const exchange = await gatewayRequest(host, `/${ticketUrl.search}`)
  expect(exchange.status).toBe(303)
  expect(exchange.location).toBe('/')
  const grant = exchange.setCookie.find((value) => value.startsWith('stars_port='))
  expect(grant, 'the ticket exchange sets the stars_port cookie').toBeTruthy()

  let app = await gatewayRequest(host, '/', grant!.split(';')[0])
  for (let attempt = 0; attempt < 20 && app.status !== 200; attempt += 1) {
    await new Promise((resolve) => setTimeout(resolve, 200))
    app = await gatewayRequest(host, '/', grant!.split(';')[0])
  }
  expect(app.status).toBe(200)
  expect(app.body).toContain('hello-port')

  assertConsoleScope(requests)
})
