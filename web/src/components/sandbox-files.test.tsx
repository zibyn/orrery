import { QueryClient, QueryClientProvider } from '@tanstack/react-query'
import { renderToStaticMarkup } from 'react-dom/server'
import { describe, expect, it } from 'vitest'

import type { FileEntry } from '@/api'
import { m } from '@/paraglide/messages'

import { SandboxFiles } from './sandbox-files'

const items: FileEntry[] = [
  { path: 'data', name: 'data', type: 'dir', size: 0, mode: 0o755, mod_time: '2026-01-01T00:00:00Z' },
  { path: 'a.txt', name: 'a.txt', type: 'file', size: 3, mode: 0o644, mod_time: '2026-01-01T00:00:00Z' },
]

// Server rendering shows what the cached listing renders, without a DOM.
function render(source: string, readOnly: boolean) {
  const client = new QueryClient()
  client.setQueryData(['files', source, ''], { items })
  return renderToStaticMarkup(
    <QueryClientProvider client={client}>
      <SandboxFiles source={source} readOnly={readOnly} />
    </QueryClientProvider>,
  )
}

describe('SandboxFiles', () => {
  it('offers upload for a Sandbox', () => {
    const html = render('/sandboxes/sb-1', false)
    expect(html).toContain('type="file"')
    expect(html).toContain(m.files_upload())
  })

  it('in read-only mode lists, downloads and publishes but offers no write action', () => {
    const html = render('/volumes/v-1', true)
    expect(html).toContain('/api/v1/volumes/v-1/files/content?path=a.txt')
    expect(html).toContain('/api/v1/volumes/v-1/files/archive?path=data')
    expect(html).toContain('data-testid="files-publish-dir"')
    expect(html).not.toContain('type="file"')
    expect(html).not.toContain(m.files_upload())
    expect(html).not.toMatch(/rename|delete/i)
  })
})
