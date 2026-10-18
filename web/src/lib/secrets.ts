// Mirror the server's Secret name rules (api.md §8 / internal/api/secrets.go)
// so the form can reject a doomed request before it is sent.
const RESERVED_NAMES = new Set(['PATH', 'HOME'])
const RESERVED_PREFIXES = ['GIT_', 'LD_', 'STARS_']

export function secretNameError(raw: string): string | null {
  const name = raw.trim()
  if (!name) return 'Enter a name'
  if (name.length > 255) return 'Name must be at most 255 characters'
  if (RESERVED_NAMES.has(name) || RESERVED_PREFIXES.some((prefix) => name.startsWith(prefix))) {
    return 'That name is reserved'
  }
  return null
}
