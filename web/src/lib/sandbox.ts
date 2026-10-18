import type { Sandbox, SandboxState } from '@/api'

// api.md §8: poll a non-terminal resource (detail every 2 s, lists every 5 s)
// and stop once it reaches a terminal state.
const DETAIL_INTERVAL_MS = 2000
const LIST_INTERVAL_MS = 5000

export function isSandboxTerminal(state: SandboxState | undefined): boolean {
  return state === 'failed' || state === 'deleted'
}

export function sandboxPollInterval(state: SandboxState | undefined): number | false {
  return isSandboxTerminal(state) ? false : DETAIL_INTERVAL_MS
}

export function sandboxListPollInterval(items: Sandbox[] | undefined): number | false {
  return items?.some((sandbox) => !isSandboxTerminal(sandbox.state)) ? LIST_INTERVAL_MS : false
}
