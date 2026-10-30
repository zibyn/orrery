import type { ExecState, Snapshot, SnapshotVersion, SnapshotVersionState } from '@/api'
import { m } from '@/paraglide/messages'

// api.md §8: lists poll every 5 s while a resource is non-terminal.
const LIST_INTERVAL_MS = 5000

export function isSnapshotVersionTerminal(state: SnapshotVersionState | undefined): boolean {
  return state === 'ready' || state === 'failed' || state === 'cancelled'
}

export function snapshotVersionsPollInterval(items: SnapshotVersion[] | undefined): number | false {
  return items?.some((version) => !isSnapshotVersionTerminal(version.state)) ? LIST_INTERVAL_MS : false
}

export function versionIsCurrent(snapshot: Snapshot | undefined, versionId: string): boolean {
  return snapshot?.current_version_id === versionId
}

// A build log follows the live Exec while the version is building and reads
// the stored Output pages once it is terminal (api.md §3, §8).
export function buildLogMode(version: SnapshotVersion): 'follow' | 'stored' | 'none' {
  if (!version.build_exec_id) return 'none'
  return version.state === 'building' ? 'follow' : 'stored'
}

// A building version's Exec stays pending while it waits for admission and has
// no frames yet, so the log shows a queue notice until the Exec runs; one that
// finishes straight from pending has only its stored result (api.md §3).
export function buildLogView(
  mode: 'follow' | 'stored',
  execState: ExecState | undefined,
): 'loading' | 'queued' | 'follow' | 'stored' {
  if (mode === 'stored' || execState === 'finished') return 'stored'
  if (execState === undefined) return 'loading'
  return execState === 'pending' ? 'queued' : 'follow'
}

export function buildExecPollInterval(state: ExecState | undefined): number | false {
  return state === 'pending' ? LIST_INTERVAL_MS : false
}

export function snapshotOwnerLabel(snapshot: Snapshot, currentUserId: string | undefined): string {
  if (snapshot.owner_id === null) return m.snapshot_owner_platform()
  if (currentUserId && snapshot.owner_id === currentUserId) return m.snapshot_owner_you()
  return snapshot.owner_id
}
