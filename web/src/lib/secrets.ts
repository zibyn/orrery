import { m } from '@/paraglide/messages'

// Mirror the server's Secret name rules (api.md §8 / internal/api/secrets.go)
// so the form can reject a doomed request before it is sent.
const RESERVED_NAMES = new Set(['PATH', 'HOME'])
const RESERVED_PREFIXES = ['GIT_', 'LD_', 'STARS_']

export function secretNameError(raw: string): string | null {
  const name = raw.trim()
  if (!name) return m.secret_error_name_required()
  if (name.length > 255) return m.secret_error_name_too_long()
  if (RESERVED_NAMES.has(name) || RESERVED_PREFIXES.some((prefix) => name.startsWith(prefix))) {
    return m.secret_error_name_reserved()
  }
  return null
}
