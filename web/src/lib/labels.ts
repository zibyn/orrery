import { ApiError } from '@/api'
import { m } from '@/paraglide/messages'
import { getLocale } from '@/paraglide/runtime'

// Machine values stay in the API; the console owns their display labels
// (api.md §8). Unknown values fall back to the raw server value.
const STATE_LABELS: Record<string, () => string> = {
  pending: m.state_pending,
  provisioning: m.state_provisioning,
  ready: m.state_ready,
  stopping: m.state_stopping,
  stopped: m.state_stopped,
  deleting: m.state_deleting,
  deleted: m.state_deleted,
  failed: m.state_failed,
  building: m.state_building,
  cancelled: m.state_cancelled,
  canceling: m.state_canceling,
  finished: m.state_finished,
  running: m.state_running,
  paused: m.state_paused,
  unavailable: m.state_unavailable,
  outdated: m.state_outdated,
  lost: m.state_lost,
  connected: m.state_connected,
  connecting: m.state_connecting,
  closed: m.state_closed,
  active: m.state_active,
  healthy: m.state_healthy,
  queued: m.state_queued,
  unhealthy: m.state_unhealthy,
}

const CAUSE_LABELS: Record<string, () => string> = {
  exited: m.cause_exited,
  canceled: m.cause_canceled,
  timed_out: m.cause_timed_out,
  oom_killed: m.cause_oom_killed,
  runtime_lost: m.cause_runtime_lost,
  node_lost: m.cause_node_lost,
  pty_idle: m.cause_pty_idle,
  start_failed: m.cause_start_failed,
  admission_timeout: m.cause_admission_timeout,
  unsatisfiable: m.cause_unsatisfiable,
  quota_exceeded: m.cause_quota_exceeded,
  runtime_create_failed: m.cause_runtime_create_failed,
  image_pull_failed: m.cause_image_pull_failed,
  resource_fetch_failed: m.cause_resource_fetch_failed,
  workspace_lost: m.cause_workspace_lost,
  build_interrupted: m.cause_build_interrupted,
  import_failed: m.cause_import_failed,
  build_failed: m.cause_build_failed,
  publish_failed: m.cause_publish_failed,
}

const PROBLEM_TITLES: Record<string, () => string> = {
  invalid_request: m.problem_invalid_request,
  unauthenticated: m.problem_unauthenticated,
  permission_denied: m.problem_permission_denied,
  not_found: m.problem_not_found,
  conflict: m.problem_conflict,
  precondition_failed: m.problem_precondition_failed,
  payload_too_large: m.problem_payload_too_large,
  unsupported: m.problem_unsupported,
  quota_exceeded: m.problem_quota_exceeded,
  rate_limited: m.problem_rate_limited,
  internal: m.problem_internal,
  unavailable: m.problem_unavailable,
}

export function stateLabel(state: string): string {
  return STATE_LABELS[state]?.() ?? state
}

export function causeLabel(cause: string): string {
  return CAUSE_LABELS[cause]?.() ?? cause
}

export function runtimeLabel(runtime: string): string {
  if (runtime === 'container') return m.runtime_container()
  if (runtime === 'local') return m.runtime_local()
  return runtime
}

// A User is either linked to a Polaris identity or a machine User (ADR-0029).
export function identityLabel(user: { machine: boolean }): string {
  return user.machine ? m.identity_machine() : m.identity_polaris()
}

export function scopeLabel(scope: string): string {
  if (scope === 'read_only') return m.scope_read_only()
  if (scope === 'user') return m.scope_user()
  if (scope === 'admin') return m.scope_admin()
  return scope
}

export function workspaceModeLabel(mode: string): string {
  if (mode === 'rw') return m.workspace_mode_rw()
  if (mode === 'ro') return m.workspace_mode_ro()
  return mode
}

export function workspaceKindLabel(kind: string): string {
  if (kind === 'persistent') return m.workspace_kind_persistent()
  if (kind === 'ephemeral') return m.workspace_kind_ephemeral()
  return kind
}

export function sourceLabel(source: string): string {
  if (source === 'build') return m.snapshot_source_build()
  if (source === 'import') return m.snapshot_source_import()
  return source
}

export function conditionLabel(condition: string): string {
  return condition === 'fetching_resources' ? m.condition_fetching_resources() : condition
}

// English keeps the server's own message; Chinese shows a localized title and
// keeps the server detail as diagnostic text (api.md §8).
export function problemOf(error: unknown): { title: string; detail?: string } {
  if (error instanceof ApiError) {
    if (getLocale() === 'en') return { title: error.message || error.code }
    const detail = error.message && error.message !== error.code ? error.message : undefined
    return { title: PROBLEM_TITLES[error.code]?.() ?? error.code, detail }
  }
  return { title: error instanceof Error ? error.message : String(error) }
}

// Idle stop is stored in seconds; whole minutes read better (900 → 15 min).
export function idleStopLabel(seconds: number): string {
  if (seconds === 0) return m.common_off()
  if (seconds % 60 === 0) return m.sandbox_idle_minutes({ minutes: seconds / 60 })
  return m.sandbox_idle_seconds({ seconds })
}
