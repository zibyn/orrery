// Audit filters (api.md §8): GET /audit takes actor, action, target and an
// RFC 3339 time interval, plus cursor pagination. Cursors are opaque: only the
// server's next_cursor is ever echoed back.

export type AuditFilters = {
  actor: string
  action: string
  target: string
  from: string
  to: string
}

const FILTER_FIELDS = ['actor', 'action', 'target', 'from', 'to'] as const

export function auditPath(
  filters: AuditFilters,
  cursor?: string | null,
  limit?: number,
): string {
  const params = new URLSearchParams()
  for (const key of FILTER_FIELDS) {
    const value = filters[key].trim()
    if (value) params.set(key, value)
  }
  if (cursor) params.set('cursor', cursor)
  if (limit !== undefined) params.set('limit', String(limit))
  const query = params.toString()
  return query ? `/audit?${query}` : '/audit'
}

// datetime-local has no zone; keep the wall-clock time and append the local
// offset so the server receives RFC 3339. Unparseable input is dropped.
export function toRfc3339(local: string): string {
  if (!local) return ''
  const date = new Date(local)
  if (Number.isNaN(date.getTime())) return ''
  const pad = (n: number) => String(n).padStart(2, '0')
  const offset = -date.getTimezoneOffset()
  const sign = offset >= 0 ? '+' : '-'
  const abs = Math.abs(offset)
  return (
    `${date.getFullYear()}-${pad(date.getMonth() + 1)}-${pad(date.getDate())}` +
    `T${pad(date.getHours())}:${pad(date.getMinutes())}:${pad(date.getSeconds())}` +
    `${sign}${pad(Math.floor(abs / 60))}:${pad(abs % 60)}`
  )
}
