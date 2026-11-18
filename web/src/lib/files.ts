// File paths are relative to the browsed root (a Sandbox's /workspace or a
// Volume's root); '' and '.' both mean that root (docs/spec/api.md §3, §5).

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

// `source` is the API path owning the files: `/sandboxes/{id}` or
// `/volumes/{id}`. These paths are API-relative because api() adds /api/v1.
export function filesPath(source: string, path: string): string {
  return `${source}/files?path=${encoded(normalizePath(path))}`
}

export function fileContentPath(source: string, path: string): string {
  return `${source}/files/content?path=${encoded(normalizePath(path))}`
}

export function archivePath(source: string, path: string): string {
  return `${source}/files/archive?path=${encoded(normalizePath(path))}`
}

// Downloads bypass api(): a same-origin <a href> lets the browser attach the
// Session cookie and stream the body (api.md §5).
export function downloadHref(kind: DownloadKind, source: string, path: string): string {
  const target = kind === 'dir' ? archivePath(source, path) : fileContentPath(source, path)
  return `${API_PREFIX}${target}`
}
