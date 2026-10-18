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

export type User = {
  id: string
  name: string
  email: string | null
  is_admin: boolean
  login_type: 'password' | 'oidc' | 'none'
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

type Problem = { code?: string; detail?: string }

export async function api<T>(method: string, path: string, body?: unknown): Promise<T> {
  const res = await fetch(`/api/v1${path}`, {
    method,
    credentials: 'include',
    headers: body === undefined ? undefined : { 'Content-Type': 'application/json' },
    body: body === undefined ? undefined : JSON.stringify(body),
  })
  if (!res.ok) {
    const problem: Problem = await res.json().catch(() => ({}))
    throw new ApiError(res.status, problem.code ?? 'internal', problem.detail ?? res.statusText)
  }
  return (await res.json()) as T
}

export function getMe(): Promise<User> {
  return api('GET', '/users/me')
}
