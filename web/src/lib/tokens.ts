export const scopeOptions = ['read_only', 'user', 'admin'] as const

export type TokenScope = (typeof scopeOptions)[number]

// Display timestamps in one stable shape (UTC) instead of the browser locale.
export function formatTimestamp(value: string | null | undefined): string {
  if (!value) return '—'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  return date.toISOString().replace('T', ' ').replace(/\.\d{3}Z$/, ' UTC')
}

// A datetime-local value has no timezone; the browser resolves it as local
// time. Empty means "no expiry".
export function expiresAtFromInput(value: string): string | undefined {
  if (!value) return undefined
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return undefined
  return date.toISOString()
}
