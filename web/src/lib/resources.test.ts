import { describe, expect, it } from 'vitest'

import { type ResourceVersion, type Sandbox, type Workspace } from '@/api'
import {
  parseProvenance,
  resourceVersionPollInterval,
  resourceVersionsPollInterval,
  workspacePollInterval,
  workspaceSandboxIds,
  workspacesPollInterval,
} from './resources'

function version(overrides: Partial<ResourceVersion> = {}): ResourceVersion {
  return {
    id: 'version-1',
    resource_id: 'resource-1',
    address: null,
    size: 0,
    state: 'pending',
    cause: null,
    provenance: { source: 'upload' },
    created_at: '2026-01-01T00:00:00Z',
    expires_at: null,
    ...overrides,
  }
}

function workspace(overrides: Partial<Workspace> = {}): Workspace {
  return {
    id: 'workspace-1',
    kind: 'persistent',
    node_id: null,
    state: 'provisioning',
    cause: null,
    unavailable: false,
    conditions: [],
    sandbox_id: null,
    created_at: '2026-01-01T00:00:00Z',
    deleted_at: null,
    ...overrides,
  }
}

describe('resource version polling', () => {
  it('polls a pending version every 2 s and a list holding one every 5 s', () => {
    expect(resourceVersionPollInterval('pending')).toBe(2000)
    expect(resourceVersionsPollInterval([version({ state: 'ready' }), version()])).toBe(5000)
  })

  it('stops once every version is ready, failed or unavailable', () => {
    for (const state of ['ready', 'failed', 'unavailable'] as const) {
      expect(resourceVersionPollInterval(state)).toBe(false)
      expect(resourceVersionsPollInterval([version({ state })])).toBe(false)
    }
    expect(resourceVersionsPollInterval(undefined)).toBe(false)
  })
})

describe('workspace polling', () => {
  it('polls provisioning and deleting Workspaces', () => {
    expect(workspacePollInterval('provisioning')).toBe(2000)
    expect(workspacePollInterval('deleting')).toBe(2000)
    expect(workspacesPollInterval([workspace({ state: 'ready' }), workspace()])).toBe(5000)
  })

  it('stops on ready, deleted, failed and lost', () => {
    for (const state of ['ready', 'deleted', 'failed', 'lost'] as const) {
      expect(workspacePollInterval(state)).toBe(false)
      expect(workspacesPollInterval([workspace({ state })])).toBe(false)
    }
  })
})

describe('parseProvenance', () => {
  it('names the Workspace path a version was published from', () => {
    expect(
      parseProvenance({ source: 'workspace', workspace: { id: 'ws-1', path: 'out/model' } }),
    ).toEqual({ source: 'workspace', workspaceId: 'ws-1', path: 'out/model' })
  })

  it('shows the Workspace root as /', () => {
    expect(parseProvenance({ source: 'workspace', workspace: { id: 'ws-1', path: '.' } })).toEqual({
      source: 'workspace',
      workspaceId: 'ws-1',
      path: '',
    })
  })

  it('treats anything else as an upload', () => {
    expect(parseProvenance({ source: 'upload' })).toEqual({ source: 'upload' })
    expect(parseProvenance({})).toEqual({ source: 'upload' })
  })
})

describe('workspaceSandboxIds', () => {
  const mounted = (id: string, state: Sandbox['state'], workspaceId = 'ws-1') =>
    ({ id, state, workspace: { id: workspaceId, mode: 'ro' } }) as Sandbox

  it('uses sandbox_id for an ephemeral Workspace', () => {
    expect(workspaceSandboxIds(workspace({ kind: 'ephemeral', sandbox_id: 'sb-e' }), [])).toEqual(['sb-e'])
  })

  it('finds the Sandboxes mounting a persistent Workspace, ready first', () => {
    const sandboxes = [
      mounted('sb-stopped', 'stopped'),
      mounted('sb-other', 'ready', 'ws-2'),
      mounted('sb-ready', 'ready'),
      { id: 'sb-none', state: 'ready' } as Sandbox,
    ]
    expect(workspaceSandboxIds(workspace({ id: 'ws-1' }), sandboxes)).toEqual(['sb-ready', 'sb-stopped'])
  })

  it('is empty before the Sandboxes load', () => {
    expect(workspaceSandboxIds(workspace(), undefined)).toEqual([])
  })
})
