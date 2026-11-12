import { afterEach, describe, expect, it, vi } from 'vitest'

import { listAll } from './api'

function stubPages(pages: Record<string, { items: number[]; next_cursor: string | null }>) {
  const fetch = vi.fn(async (url: string) => {
    const cursor = new URL(url, 'http://x').searchParams.get('cursor') ?? ''
    return new Response(JSON.stringify(pages[cursor]), { status: 200 })
  })
  vi.stubGlobal('fetch', fetch)
  return fetch
}

afterEach(() => vi.unstubAllGlobals())

describe('listAll', () => {
  it('returns a single page', async () => {
    const fetch = stubPages({ '': { items: [1, 2], next_cursor: null } })
    expect(await listAll('/snapshots')).toEqual([1, 2])
    expect(fetch).toHaveBeenCalledOnce()
    expect(fetch.mock.calls[0][0]).toBe('/api/v1/snapshots?limit=200')
  })

  it('follows next_cursor and joins the pages in order', async () => {
    const fetch = stubPages({
      '': { items: [1, 2], next_cursor: 'a' },
      a: { items: [3], next_cursor: 'b' },
      b: { items: [4], next_cursor: null },
    })
    expect(await listAll('/snapshots/s/versions')).toEqual([1, 2, 3, 4])
    expect(fetch.mock.calls.map((call) => call[0])).toEqual([
      '/api/v1/snapshots/s/versions?limit=200',
      '/api/v1/snapshots/s/versions?limit=200&cursor=a',
      '/api/v1/snapshots/s/versions?limit=200&cursor=b',
    ])
  })

  it('returns an empty list', async () => {
    stubPages({ '': { items: [], next_cursor: null } })
    expect(await listAll('/nodes')).toEqual([])
  })

  it('appends to a path that already has a query', async () => {
    const fetch = stubPages({ '': { items: [], next_cursor: null } })
    await listAll('/sandboxes?owner=u')
    expect(fetch.mock.calls[0][0]).toBe('/api/v1/sandboxes?owner=u&limit=200')
  })
})
