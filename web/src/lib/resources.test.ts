import { describe, expect, it } from 'vitest'

import { type ResourceVersion } from '@/api'
import {
  parseProvenance,
  resourceVersionPollInterval,
  resourceVersionsPollInterval,
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

describe('parseProvenance', () => {
  it('names the Sandbox path a version was published from', () => {
    expect(parseProvenance({ source: 'sandbox', sandbox: { id: 'sb-1', path: 'out/model' } })).toEqual({
      source: 'sandbox',
      id: 'sb-1',
      path: 'out/model',
    })
  })

  it('names the Volume path a version was published from', () => {
    expect(
      parseProvenance({ source: 'volume', volume: { id: 'vol-1', path: 'data', generation_id: 'gen-1' } }),
    ).toEqual({ source: 'volume', id: 'vol-1', path: 'data' })
  })

  it('shows the root as /', () => {
    expect(parseProvenance({ source: 'sandbox', sandbox: { id: 'sb-1', path: '.' } })).toEqual({
      source: 'sandbox',
      id: 'sb-1',
      path: '',
    })
  })

  it('treats anything else as an upload', () => {
    expect(parseProvenance({ source: 'upload' })).toEqual({ source: 'upload' })
    expect(parseProvenance({ source: 'sandbox' })).toEqual({ source: 'upload' })
    expect(parseProvenance({})).toEqual({ source: 'upload' })
  })
})
