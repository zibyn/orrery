// The console talks only to the public REST API, authenticated by the Session
// cookie (ADR-0020, api.md §8).

export class ApiError extends Error {
  readonly status: number
  readonly code: string

  constructor(status: number, code: string, detail: string) {
    super(detail || code)
    this.status = status
    this.code = code
  }
}

export type Quota = {
  gpus: number
  sandboxes: number
}

export type User = {
  id: string
  name: string
  email: string | null
  is_admin: boolean
  login_type: 'password' | 'oidc' | 'none'
  quota: Quota
  created_at: string
  deleted_at: string | null
}

export type QuotaUsage = {
  gpus: { limit: number; used: number }
  sandboxes: { limit: number; used: number }
}

export type Token = {
  id: string
  kind: 'token' | 'session'
  name: string
  scope: 'read_only' | 'user' | 'admin'
  expires_at: string | null
  last_used_at: string | null
  created_at: string
}

// The plaintext `token` is present only on creation (api.md §8).
export type CreatedToken = Token & { token: string }

export type Secret = {
  name: string
  updated_at: string
}

export type ListResponse<T> = {
  items: T[]
  next_cursor: string | null
}

export type GitProvider = {
  id: string
  type: string
  host: string
}

export type GitLink = {
  provider_id: string
  linked_at: string
}

export type SandboxState =
  | 'pending'
  | 'provisioning'
  | 'ready'
  | 'stopping'
  | 'stopped'
  | 'deleting'
  | 'deleted'
  | 'failed'

export type Sandbox = {
  id: string
  name: string
  labels: Record<string, string>
  runtime: 'container' | 'local'
  state: SandboxState
  cause: string | null
  node_id: string | null
  snapshot: string
  cpu: number
  memory: number
  gpu?: { count: number; model?: string }
  idle_stop: number
  workspace?: { id: string; mode: 'rw' | 'ro' }
  conditions: string[]
  queue_position?: number
  blocked_reason?: string
  created_at: string
}

export type CreateSandboxRequest = {
  name: string
  snapshot?: string
  cpu: number
  memory: number
  gpu?: { count: number; model?: string }
  workspace?: { id: string; mode: 'rw' | 'ro' }
  idle_stop?: number
}

export type Snapshot = {
  id: string
  owner_id: string | null
  name: string
  current_version_id: string | null
  created_at: string | null
  deleted_at?: string
}

export type Workspace = {
  id: string
  kind: 'ephemeral' | 'persistent'
  node_id: string | null
  state: 'provisioning' | 'ready' | 'deleting' | 'deleted' | 'failed' | 'lost'
  cause: string | null
  unavailable: boolean
  conditions: string[]
  sandbox_id: string | null
  created_at: string
  deleted_at: string | null
}

export type ExecState = 'pending' | 'running' | 'canceling' | 'finished'

export type ExecResult = {
  cause: string
  exit_code?: number
  signal?: number
}

export type Exec = {
  id: string
  sandbox_id: string
  argv: string[]
  cwd: string
  timeout: number | null
  state: ExecState
  result: ExecResult | null
  log_truncated: boolean
  accepted_at: string
  started_at: string | null
  finished_at: string | null
  deadline_at: string | null
}

export type ExecOutputFrame = {
  seq: number
  stream: 'stdout' | 'stderr' | 'truncated'
  data: string
  encoding?: 'base64'
}

export type ExecResultFrame = ExecResult & {
  type: 'result'
  log_truncated: boolean
}

export type PtyExitFrame = ExecResult & {
  type: 'exit'
}

export type CreateExecRequest = {
  argv: string[]
  env?: Record<string, string>
  cwd?: string
  timeout?: number
  stdin?: string
  pty?: { rows: number; cols: number }
}

export type CapacityGPU = {
  model: string
  total: number
  allocated: number
}

export type CapacityNode = {
  node_id: string
  name: string
  status: string
  gpus: CapacityGPU[]
}

export type Node = {
  id: string
  name: string
  status: 'ready' | 'paused' | 'unavailable' | 'outdated' | 'lost'
  paused: boolean
  lost_at: string | null
  agent_version: string | null
  proto_major: number | null
  proto_minor: number | null
  last_seen_at: string | null
  runtime_health: string | null
  capabilities: unknown
  hardware: unknown
  gpus: { uuid: string; model: string }[]
  created_at: string
}

export type AuditRecord = {
  id: number
  at: string
  actor_user_id: string | null
  actor_name: string | null
  actor_token_id: string | null
  action: string
  target_type: string
  target_id: string
  correlation_id: string | null
  detail: unknown
}

export type FileEntry = {
  path: string
  name: string
  type: 'file' | 'dir' | 'symlink' | 'other'
  size: number
  mode: number
  mod_time: string
  link_target?: string
}

export type PortTicket = { url: string }

type Problem = { code?: string; detail?: string }

async function problemError(res: Response): Promise<ApiError> {
  const problem: Problem = await res.json().catch(() => ({}))
  return new ApiError(res.status, problem.code ?? 'internal', problem.detail ?? res.statusText)
}

export async function api<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(`/api/v1${path}`, {
    method,
    credentials: 'include',
    headers: body === undefined ? undefined : { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  if (!res.ok) throw await problemError(res)
  if (res.status === 204) return undefined as T
  return (await res.json()) as T
}

// File uploads send the File itself as the raw request body and the server
// answers 204, which api() cannot parse (api.md §5).
export async function apiVoid(method: string, path: string, body?: BodyInit): Promise<void> {
  const res = await fetch(`/api/v1${path}`, {
    method,
    credentials: 'include',
    body,
  })
  if (!res.ok) throw await problemError(res)
}

export function getMe(): Promise<User> {
  return api('GET', '/users/me')
}
