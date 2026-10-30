import { describe, expect, it } from 'vitest'

import { type Snapshot, type SnapshotVersion, type SnapshotVersionState } from '@/api'
import {
  buildExecPollInterval,
  buildLogMode,
  buildLogView,
  isSnapshotVersionTerminal,
  snapshotOwnerLabel,
  snapshotVersionsPollInterval,
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
