import { describe, expect, it } from 'vitest'

import { type Snapshot, type SnapshotVersion, type SnapshotVersionState } from '@/api'
import {
  baseSnapshotOptions,
  snapshotForRef,
  buildExecPollInterval,
  buildLogMode,
  buildLogView,
  isSnapshotVersionTerminal,
  snapshotOwnerLabel,
  snapshotIsWritable,
  snapshotVersionsPollInterval,
  versionActions,
  versionIsCurrent,
} from './snapshots'

function version(overrides: Partial<SnapshotVersion> = {}): SnapshotVersion {
  return {
    id: 'version-1',
    snapshot_id: 'snapshot-1',
    digest: null,
    state: 'building',
    cause: null,
    source: 'build',
    created_at: '2026-01-01T00:00:00Z',
    ...overrides,
  }
}

function snapshot(overrides: Partial<Snapshot> = {}): Snapshot {
  return {
    id: 'snapshot-1',
    owner_id: 'user-1',
    name: 'snapshot',
    current_version_id: null,
    created_at: '2026-01-01T00:00:00Z',
    ...overrides,
  }
}

describe('isSnapshotVersionTerminal', () => {
  it('treats ready, failed and cancelled as terminal', () => {
    for (const state of ['ready', 'failed', 'cancelled'] as const) {
      expect(isSnapshotVersionTerminal(state)).toBe(true)
    }
  })

  it('keeps building non-terminal', () => {
    expect(isSnapshotVersionTerminal('building')).toBe(false)
    expect(isSnapshotVersionTerminal(undefined)).toBe(false)
  })
})

describe('snapshotVersionsPollInterval', () => {
  it('polls every 5 seconds while any version is building', () => {
    expect(snapshotVersionsPollInterval([version(), version({ state: 'ready' })])).toBe(5000)
  })

  it('stops once every version is terminal', () => {
    const states: SnapshotVersionState[] = ['ready', 'failed', 'cancelled']
    expect(snapshotVersionsPollInterval(states.map((state) => version({ state })))).toBe(false)
    expect(snapshotVersionsPollInterval([])).toBe(false)
    expect(snapshotVersionsPollInterval(undefined)).toBe(false)
  })
})

describe('versionIsCurrent', () => {
  it('matches the Snapshot current_version_id', () => {
    expect(versionIsCurrent(snapshot({ current_version_id: 'v1' }), 'v1')).toBe(true)
    expect(versionIsCurrent(snapshot({ current_version_id: 'v1' }), 'v2')).toBe(false)
  })

  it('has no current version when the pointer is null or the Snapshot is missing', () => {
    expect(versionIsCurrent(snapshot({ current_version_id: null }), 'v1')).toBe(false)
    expect(versionIsCurrent(undefined, 'v1')).toBe(false)
  })
})

describe('buildLogMode', () => {
  it('follows the live log while the version is building', () => {
    expect(buildLogMode(version({ state: 'building', build_exec_id: 'exec-1' }))).toBe('follow')
  })

  it('reads the stored log once the version is terminal', () => {
    for (const state of ['ready', 'failed', 'cancelled'] as const) {
      expect(buildLogMode(version({ state, build_exec_id: 'exec-1' }))).toBe('stored')
    }
  })

  it('has no log without a build_exec_id', () => {
    expect(buildLogMode(version({ source: 'import' }))).toBe('none')
  })
})

describe('buildLogView', () => {
  it('shows the queue notice while the build Exec is pending, then follows once it runs', () => {
    expect(buildLogView('follow', 'pending')).toBe('queued')
    expect(buildExecPollInterval('pending')).toBe(5000)
    expect(buildLogView('follow', 'running')).toBe('follow')
    expect(buildExecPollInterval('running')).toBe(false)
  })

  it('reads the stored log when the build Exec finishes straight from pending', () => {
    expect(buildLogView('follow', 'pending')).toBe('queued')
    expect(buildLogView('follow', 'finished')).toBe('stored')
    expect(buildExecPollInterval('finished')).toBe(false)
  })

  it('keeps following while the build Exec cancels', () => {
    expect(buildLogView('follow', 'canceling')).toBe('follow')
  })

  it('waits for the Exec state before opening the follow stream', () => {
    expect(buildLogView('follow', undefined)).toBe('loading')
  })

  it('opens the follow stream when the Exec state cannot be read', () => {
    expect(buildLogView('follow', undefined, true)).toBe('follow')
  })

  it('reads the stored log of a terminal version without the Exec state', () => {
    expect(buildLogView('stored', undefined)).toBe('stored')
  })
})

describe('snapshotOwnerLabel', () => {
  it('labels platform Snapshots', () => {
    expect(snapshotOwnerLabel(snapshot({ owner_id: null }), 'user-1')).toBe('Platform')
  })

  it("labels the caller's own Snapshots as You", () => {
    expect(snapshotOwnerLabel(snapshot({ owner_id: 'user-1' }), 'user-1')).toBe('You')
  })

  it("falls back to the owner id for another user's Snapshot", () => {
    expect(snapshotOwnerLabel(snapshot({ owner_id: 'user-2' }), 'user-1')).toBe('user-2')
  })
})

describe('baseSnapshotOptions', () => {
  it('offers other Snapshots that have a current version', () => {
    const self = snapshot({ id: 'self', current_version_id: 'v1' })
    const ready = snapshot({ id: 'ready', current_version_id: 'v2' })
    const platform = snapshot({ id: 'platform', owner_id: null, current_version_id: 'v3' })
    const empty = snapshot({ id: 'empty', current_version_id: null })
    expect(baseSnapshotOptions([self, ready, platform, empty], 'self')).toEqual([ready, platform])
  })
})

describe('snapshotIsWritable', () => {
  it('lets the owner write their own Snapshot', () => {
    expect(snapshotIsWritable(snapshot({ owner_id: 'user-1' }))).toBe(true)
  })

  it('keeps Platform Snapshots and a missing Snapshot read-only', () => {
    expect(snapshotIsWritable(snapshot({ owner_id: null }))).toBe(false)
    expect(snapshotIsWritable(undefined)).toBe(false)
  })
})

describe('versionActions', () => {
  // [state, current, writable] -> [cancel, setCurrent, delete], for a build-source version
  const cases: [SnapshotVersionState, boolean, boolean, boolean, boolean, boolean][] = [
    ['building', false, true, true, false, false],
    ['building', true, true, true, false, false],
    ['ready', false, true, false, true, true],
    ['ready', true, true, false, false, false],
    ['failed', false, true, false, false, true],
    ['failed', true, true, false, false, false],
    ['cancelled', false, true, false, false, true],
    ['cancelled', true, true, false, false, false],
    ['building', false, false, false, false, false],
    ['building', true, false, false, false, false],
    ['ready', false, false, false, false, false],
    ['ready', true, false, false, false, false],
    ['failed', false, false, false, false, false],
    ['failed', true, false, false, false, false],
    ['cancelled', false, false, false, false, false],
    ['cancelled', true, false, false, false, false],
  ]

  it.each(cases)(
    'a %s version (current %s, writable %s)',
    (state, current, writable, cancel, setCurrent, remove) => {
      const owned = snapshot({
        owner_id: writable ? 'user-1' : null,
        current_version_id: current ? 'version-1' : 'version-0',
      })
      expect(versionActions(owned, version({ state }))).toEqual({
        cancel,
        setCurrent,
        delete: remove,
      })
    },
  )

  it('offers delete on a building import, which the server cancels', () => {
    const owned = snapshot({ owner_id: 'user-1', current_version_id: 'version-0' })
    expect(versionActions(owned, version({ source: 'import', state: 'building' }))).toEqual({
      cancel: true,
      setCurrent: false,
      delete: true,
    })
  })

  it('offers nothing while the Snapshot is missing', () => {
    expect(versionActions(undefined, version({ state: 'ready' }))).toEqual({
      cancel: false,
      setCurrent: false,
      delete: false,
    })
  })
})

describe('snapshotForRef', () => {
  const platform = { id: 'p', name: 'default', owner_id: null } as Snapshot
  const own = { id: '01a1a8d2-dfec-7a63-80b8-810708dc52de', name: 'python-ml', owner_id: 'u' } as Snapshot

  it('maps a platform ref to the platform Snapshot by name', () => {
    expect(snapshotForRef('127.0.0.1:15000/platform/default@sha256:abc', [own, platform])).toEqual({
      snapshot: platform,
      label: 'default',
      digest: 'sha256:abc',
    })
  })

  it('maps an owned ref to its Snapshot by id', () => {
    const ref = `reg:5000/snapshots/${own.id}@sha256:def`
    expect(snapshotForRef(ref, [platform, own]).snapshot).toBe(own)
  })

  it('falls back to the raw ref when it has neither form', () => {
    expect(snapshotForRef('python:3.12', [])).toEqual({ label: 'python:3.12' })
  })
})
