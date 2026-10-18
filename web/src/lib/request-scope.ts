// Every request the console makes must stay on its own origin, except a
// port-forward URL the Gateway hands out (api.md §8, ADR-0020). The Playwright
// suite imports this so the whitelist assertion and the module share one rule.
export type ConsoleScope = {
  origin: string
  appsDomain?: string
}

const HTTP_LIKE = new Set(['http:', 'https:', 'ws:', 'wss:'])

export function isAllowedConsoleUrl(raw: string, scope: ConsoleScope): boolean {
  let url: URL
  try {
    url = new URL(raw, scope.origin)
  } catch {
    return false
  }
  if (!HTTP_LIKE.has(url.protocol)) return false

  const base = new URL(scope.origin)
  // ws/wss are the same origin as http/https for CSP's `connect-src 'self'`.
  const protocol = url.protocol === 'ws:' ? 'http:' : url.protocol === 'wss:' ? 'https:' : url.protocol
  if (protocol === base.protocol && url.host === base.host) return true

  const appsDomain = scope.appsDomain?.trim().replace(/^\./, '')
  if (!appsDomain) return false
  const escaped = appsDomain.replace(/[.*+?^${}()|[\]\\]/g, '\\$&')
  const match = new RegExp(`^(\\d{1,5})--([A-Za-z0-9-]+)\\.${escaped}$`, 'i').exec(url.hostname)
  if (!match) return false
  const port = Number(match[1])
  return port >= 1 && port <= 65535
}
