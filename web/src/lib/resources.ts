import type { ResourceVersion, ResourceVersionState, Workspace } from '@/api'

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

function workspaceSettling(state: Workspace['state'] | undefined): boolean {
  return state === 'provisioning' || state === 'deleting'
}

export function workspacePollInterval(state: Workspace['state'] | undefined): number | false {
  return workspaceSettling(state) ? DETAIL_INTERVAL_MS : false
}

export function workspacesPollInterval(items: Workspace[] | undefined): number | false {
  return items?.some((workspace) => workspaceSettling(workspace.state)) ? LIST_INTERVAL_MS : false
}

export type Provenance = { source: 'upload' } | { source: 'workspace'; workspaceId: string; path: string }

// Reads the provenance the server writes (publish.go); the Workspace root is
// stored as "." and shown as "/".
export function parseProvenance(provenance: Record<string, unknown>): Provenance {
  const workspace = provenance.workspace as { id?: unknown; path?: unknown } | undefined
  if (provenance.source === 'workspace' && typeof workspace?.id === 'string') {
    const path = typeof workspace.path === 'string' && workspace.path !== '.' ? workspace.path : ''
    return { source: 'workspace', workspaceId: workspace.id, path }
  }
  return { source: 'upload' }
}
