import type { Token } from '@/api'

// The login response is the only place the console learns the current
// Session's id (OIDC logins never see it). Store it so the token list can mark
// that row; a stale id simply matches nothing.
const STORAGE_KEY = 'stars-sandbox.current-session'

export function matchesCurrentSession(
  token: Pick<Token, 'id' | 'kind'>,
  currentId: string | null,
): boolean {
  return Boolean(currentId) && token.kind === 'session' && token.id === currentId
}

export function readCurrentSessionId(): string | null {
  try {
    return localStorage.getItem(STORAGE_KEY)
  } catch {
    return null
  }
}

export function rememberCurrentSession(id: string): void {
  try {
    localStorage.setItem(STORAGE_KEY, id)
  } catch {
    // Storage can be unavailable in private modes; the marker is optional.
  }
}

export function forgetCurrentSession(): void {
  try {
    localStorage.removeItem(STORAGE_KEY)
  } catch {
    // Nothing to clear when storage is unavailable.
  }
}
