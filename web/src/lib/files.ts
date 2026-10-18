// Workspace file paths are relative to the Workspace root; '' and '.' both
// mean the root (api.md §5, docs/spec/api.md §8).

export type DownloadKind = 'file' | 'dir'

export function normalizePath(path: string): string {
  const trimmed = path.replace(/^\.\/+/, '').replace(/\/+$/, '')
  return trimmed === '.' ? '' : trimmed
}

export function joinPath(dir: string, name: string): string {
  const base = normalizePath(dir)
  return base ? `${base}/${name}` : name
}

export function parentPath(path: string): string {
  const normalized = normalizePath(path)
  const slash = normalized.lastIndexOf('/')
  return slash < 0 ? '' : normalized.slice(0, slash)
}

export function formatBytes(size: number): string {
  if (!Number.isFinite(size) || size < 0) return '—'
  if (size < 1024) return `${size} B`
  const units = ['KiB', 'MiB', 'GiB', 'TiB', 'PiB']
  let value = size / 1024
  let unit = 0
  while (value >= 1024 && unit < units.length - 1) {
    value /= 1024
    unit += 1
  }
  return `${value.toFixed(1)} ${units[unit]}`
}

const API_PREFIX = '/api/v1'

function encoded(name: string): string {
  return encodeURIComponent(name)
}

// These paths are API-relative because api() adds the /api/v1 prefix.
export function filesPath(sandboxId: string, path: string): string {
  return `/sandboxes/${encoded(sandboxId)}/files?path=${encoded(normalizePath(path))}`
}

export function fileContentPath(sandboxId: string, path: string): string {
  return `/sandboxes/${encoded(sandboxId)}/files/content?path=${encoded(normalizePath(path))}`
}

export function archivePath(sandboxId: string, path: string): string {
  return `/sandboxes/${encoded(sandboxId)}/files/archive?path=${encoded(normalizePath(path))}`
}

// Downloads bypass api(): a same-origin <a href> lets the browser attach the
// Session cookie and stream the body (api.md §5).
export function downloadHref(kind: DownloadKind, sandboxId: string, path: string): string {
  const target = kind === 'dir' ? archivePath(sandboxId, path) : fileContentPath(sandboxId, path)
  return `${API_PREFIX}${target}`
}
