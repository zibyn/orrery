import type { ResourceVersion, ResourceVersionState } from '@/api'

// api.md §8: detail every 2 s, lists every 5 s, while non-terminal.
const DETAIL_INTERVAL_MS = 2000
const LIST_INTERVAL_MS = 5000

// `unavailable` only returns to ready after a later upload repairs the object,
// which no amount of polling here will cause (api.md §3).
export function resourceVersionPollInterval(state: ResourceVersionState | undefined): number | false {
  return state === 'pending' ? DETAIL_INTERVAL_MS : false
}

export function resourceVersionsPollInterval(items: ResourceVersion[] | undefined): number | false {
  return items?.some((version) => version.state === 'pending') ? LIST_INTERVAL_MS : false
}

export type Provenance = { source: 'upload' } | { source: 'sandbox' | 'volume'; id: string; path: string }

// Reads the provenance the server writes (publish.go, volume_files.go):
// sandbox{id, path} or volume{id, path}; the root is stored as "." and shown as "/".
export function parseProvenance(provenance: Record<string, unknown>): Provenance {
  const source = provenance.source
  if (source !== 'sandbox' && source !== 'volume') return { source: 'upload' }
  const ref = provenance[source] as { id?: unknown; path?: unknown } | undefined
  if (typeof ref?.id !== 'string') return { source: 'upload' }
  const path = typeof ref.path === 'string' && ref.path !== '.' ? ref.path : ''
  return { source, id: ref.id, path }
}
