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

export type CreatedToken = {
  id: string
  name: string
  scope: string
  token: string
  created_at: string
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
