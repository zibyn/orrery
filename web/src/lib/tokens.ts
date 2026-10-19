import { getLocale } from '@/paraglide/runtime'

export const scopeOptions = ['read_only', 'user', 'admin'] as const

export type TokenScope = (typeof scopeOptions)[number]

// Timestamps keep UTC so every User sees the same instant, but the shape
// follows the console's language (api.md §8).
export function formatTimestamp(value: string | null | undefined): string {
  if (!value) return '—'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  return `${date.toLocaleString(getLocale(), {
    dateStyle: 'medium',
    timeStyle: 'medium',
    timeZone: 'UTC',
  })} UTC`
}

// Records such as audit times and file modification times are shown in the
// browser's local zone, matching the datetime-local filters and the filesystem.
export function formatLocalDateTime(value: string | null | undefined): string {
  if (!value) return '—'
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return value
  return date.toLocaleString(getLocale(), { dateStyle: 'medium', timeStyle: 'medium' })
}

// A datetime-local value has no timezone; the browser resolves it as local
// time. Empty means "no expiry".
export function expiresAtFromInput(value: string): string | undefined {
  if (!value) return undefined
  const date = new Date(value)
  if (Number.isNaN(date.getTime())) return undefined
  return date.toISOString()
}
