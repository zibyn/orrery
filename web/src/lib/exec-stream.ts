import type { ExecOutputFrame, ExecResult, ExecResultFrame, ExecState } from '@/api'

// Every stored frame is a 5-byte header (stream byte plus a big-endian length)
// followed by its payload; `seq` is the frame's offset in the record
// (runtime.md §3, api.md §4).
const FRAME_HEADER_BYTES = 5

export const TRUNCATION_NOTICE = '[output truncated — full log available via the CLI]'

export function decodeFrame(frame: ExecOutputFrame): string {
  if (frame.encoding !== 'base64') return frame.data
  const binary = atob(frame.data)
  const bytes = Uint8Array.from(binary, (char) => char.charCodeAt(0))
  return new TextDecoder().decode(bytes)
}

export function frameText(frame: ExecOutputFrame): string {
  if (frame.stream === 'truncated') return `\r\n\x1b[33m${TRUNCATION_NOTICE}\x1b[0m\r\n`
  return decodeFrame(frame)
}

export function frameEnd(frame: ExecOutputFrame): number {
  // The offset counts record bytes: decoded bytes for base64 frames, UTF-8
  // bytes otherwise, even when the payload is not valid UTF-8.
  const bytes =
    frame.encoding === 'base64'
      ? atob(frame.data).length
      : new TextEncoder().encode(frame.data).length
  return frame.seq + FRAME_HEADER_BYTES + bytes
}

export function isResultFrame(value: unknown): value is ExecResultFrame {
  return typeof value === 'object' && value !== null && (value as { type?: unknown }).type === 'result'
}

export function formatExecResult(result: ExecResult & { log_truncated?: boolean }): string {
  const suffix =
    result.exit_code !== undefined
      ? ` (exit code ${result.exit_code})`
      : result.signal !== undefined
        ? ` (signal ${result.signal})`
        : ''
  return `${result.cause}${suffix}${result.log_truncated ? ' · log truncated' : ''}`
}

export function execListPollInterval(items: Pick<{ state: ExecState }, 'state'>[] | undefined) {
  return items?.some((exec) => exec.state !== 'finished') ? 5000 : false
}

export function parseArgv(text: string): string[] {
  return text.split(/\s+/).filter(Boolean)
}

// The console is same-origin with the API (api.md §8); only the ws scheme
// changes with the page.
export function consoleWebSocketUrl(path: string): string {
  const protocol = window.location.protocol === 'https:' ? 'wss:' : 'ws:'
  return `${protocol}//${window.location.host}/api/v1${path}`
}
